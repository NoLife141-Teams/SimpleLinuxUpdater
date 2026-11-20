package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

const (
	backupSecondaryPollInterval = 30 * time.Second
	backupSecondaryCopyTimeout  = 15 * time.Minute
)

var errBackupSecondaryDeferred = errors.New("secondary backup waiting for installation maintenance")

func (a *backupAutomation) monitorSecondaryBackups(ctx context.Context) {
	ticker := time.NewTicker(backupSecondaryPollInterval)
	defer ticker.Stop()
	for {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = a.copyNextSecondaryBackup(ctx)
		}()
		select {
		case <-ctx.Done():
			return
		case <-done:
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Catalog transactions hold maintenance admission only while reading or
// saving state, or publishing a verified temporary file. Copying and hashing
// mounted files runs outside the primary export's exclusive lease.
func (a *backupAutomation) withSecondaryState(ctx context.Context, apply func(*backupAutomationState) (bool, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupSecondaryDeferred
	}
	defer lease.Close()
	if a.deps.RestoreIsolation.BlocksExternalWork() || !a.mu.TryLock() {
		return errBackupSecondaryDeferred
	}
	defer a.mu.Unlock()
	state, err := a.load(ctx)
	if err != nil {
		return err
	}
	changed, err := apply(&state)
	if err != nil || !changed {
		return err
	}
	return a.save(ctx, state)
}

func (a *backupAutomation) copyNextSecondaryBackup(parent context.Context) error {
	if !a.secondaryMu.TryLock() {
		return errBackupSecondaryDeferred
	}
	defer a.secondaryMu.Unlock()
	ctx, cancel := context.WithTimeout(parent, backupSecondaryCopyTimeout)
	defer cancel()

	var pending, cleanup backupSecondaryCopy
	var binding backupSecondaryBinding
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) {
			return false, nil
		}
		binding = state.Secondary.Binding
		now := a.now()
		for _, item := range state.Secondary.Copies {
			if pending.Archive.ID == "" && item.pending() && !parseBackupStamp(item.RetryAt).After(now) {
				pending = item
			}
			if cleanup.Archive.ID == "" && !item.pending() && item.Temporary != "" {
				cleanup = item
			}
		}
		return false, nil
	})
	if err != nil || binding.Path == "" {
		return err
	}

	root, actual, err := a.openSecondaryDestination()
	if err != nil || actual != binding {
		if root != nil {
			_ = root.Close()
		}
		failureCtx, cancelFailure := context.WithTimeout(parent, backupArchiveObservationBudget)
		defer cancelFailure()
		return a.recordSecondaryFailure(failureCtx, binding, pending.Archive.ID, "storage_unavailable")
	}
	defer root.Close()
	if cleanup.Archive.ID != "" && removeOwnedSecondaryTemporary(root, binding, cleanup) {
		err := a.updateCurrentSecondary(ctx, binding, cleanup.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
			if current.Temporary != cleanup.Temporary || current.FileIdentity != cleanup.FileIdentity {
				return false, errBackupSecondaryDeferred
			}
			current.Temporary = ""
			return true, nil
		})
		if err != nil {
			return err
		}
	}
	if pending.Archive.ID == "" {
		err := a.updateCurrentSecondary(ctx, binding, "", func(state *backupSecondaryState, _ *backupSecondaryCopy) (bool, error) {
			if state.LastFailure == "storage_unavailable" {
				state.LastFailure = ""
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return err
		}
		return a.keepSecondaryRetention(ctx, root, binding)
	}
	err = a.stageAndPublishSecondary(ctx, root, binding, pending)
	if err != nil {
		if errors.Is(err, errBackupSecondaryDeferred) || parent.Err() != nil {
			return err
		}
		recordCtx, cancelRecord := context.WithTimeout(parent, backupArchiveObservationBudget)
		defer cancelRecord()
		if recordErr := a.recordSecondaryFailure(recordCtx, binding, pending.Archive.ID, "copy_failed"); recordErr != nil {
			return recordErr
		}
		return err
	}
	return a.keepSecondaryRetention(ctx, root, binding)
}

func (a *backupAutomation) keepSecondaryRetention(ctx context.Context, root *os.Root, binding backupSecondaryBinding) error {
	err := a.retainSecondaryBackups(ctx, root, binding)
	if err != nil && !errors.Is(err, errBackupSecondaryDeferred) {
		_ = a.recordSecondaryFailure(ctx, binding, "", "retention_failed")
	}
	if err == nil {
		return a.updateCurrentSecondary(ctx, binding, "", func(state *backupSecondaryState, _ *backupSecondaryCopy) (bool, error) {
			if state.LastFailure == "retention_failed" {
				state.LastFailure = ""
				return true, nil
			}
			return false, nil
		})
	}
	return err
}

func (a *backupAutomation) updateCurrentSecondary(ctx context.Context, binding backupSecondaryBinding, id string,
	apply func(*backupSecondaryState, *backupSecondaryCopy) (bool, error)) error {
	return a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding {
			return false, errBackupSecondaryDeferred
		}
		if id == "" {
			return apply(&state.Secondary, nil)
		}
		for i := range state.Secondary.Copies {
			if state.Secondary.Copies[i].Archive.ID == id {
				return apply(&state.Secondary, &state.Secondary.Copies[i])
			}
		}
		return false, errBackupSecondaryDeferred
	})
}

func (a *backupAutomation) recordSecondaryFailure(ctx context.Context, binding backupSecondaryBinding, id, reason string) error {
	return a.updateCurrentSecondary(ctx, binding, id, func(state *backupSecondaryState, copy *backupSecondaryCopy) (bool, error) {
		if copy == nil && state.LastFailure == reason {
			return false, nil
		}
		state.LastFailure = reason
		if copy != nil && copy.pending() {
			copy.Attempts++
			copy.Failure = reason
			copy.RetryAt = backupStamp(a.now().Add(time.Duration(copy.Attempts) * backupSecondaryRetryBase))
		}
		return true, nil
	})
}

func (a *backupAutomation) stageAndPublishSecondary(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy) error {
	if !backupArchiveOwned(copy.Archive, binding.Owner) || !validBackupIntegrityDigest(copy.Archive.SHA256) {
		return errors.New("secondary copy has no valid source proof")
	}
	_, err := root.Lstat(copy.Archive.Name)
	switch {
	case err == nil:
		// The expected final name may survive a crash between linking the file
		// and registering it. A same-name file is foreign without the recorded inode.
		if copy.FileIdentity == "" {
			return errors.New("secondary archive name is already occupied")
		}
	case !os.IsNotExist(err):
		return err
	default:
		if err := a.stageSecondaryArchive(ctx, root, binding, &copy); err != nil {
			return err
		}
	}
	verifyName := copy.Archive.Name
	linkRequired := os.IsNotExist(err)
	if linkRequired {
		verifyName = copy.Temporary
	}
	verified, err := verifySecondaryFile(ctx, root, verifyName, copy.FileIdentity, copy.Archive)
	if err != nil {
		return err
	}
	err = a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(state *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !current.pending() || current.FileIdentity != copy.FileIdentity || !backupArchiveMountAvailable(root.Name(), binding.Directory) {
			return false, errBackupSecondaryDeferred
		}
		info, statErr := root.Lstat(verifyName)
		if statErr != nil || !sameBackupIntegrityFile(verified, info) {
			return false, errors.New("secondary archive changed before publication")
		}
		if linkRequired {
			if err := root.Link(copy.Temporary, copy.Archive.Name); err != nil {
				return false, err
			}
		}
		if err := syncSecondaryDirectory(root); err != nil {
			return false, err
		}
		current.VerifiedAt = backupStamp(a.now())
		current.RetryAt, current.Failure = "", ""
		state.LastFailure = ""
		return true, nil
	})
	if err == nil {
		removeOwnedSecondaryTemporary(root, binding, copy)
	}
	return err
}

func (a *backupAutomation) stageSecondaryArchive(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy *backupSecondaryCopy) error {
	sourceDirectory, err := backupRealPath(a.deployment.Directory)
	if err != nil {
		return err
	}
	sourceRoot, err := os.OpenRoot(sourceDirectory)
	if err != nil {
		return err
	}
	defer sourceRoot.Close()
	source, err := openBackupCatalogFile(sourceRoot, copy.Archive.Name)
	if err != nil {
		return err
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !secondarySourceMatches(sourceRoot, copy.Archive, info) {
		return errors.New("secondary backup source no longer matches its verified archive")
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return errors.New("secondary archive mount changed before staging")
	}
	if !removeOwnedSecondaryTemporary(root, binding, *copy) {
		return errors.New("previous secondary backup temporary file could not be removed")
	}
	temporary := ".slu-secondary-" + rand.Text() + ".tmp"
	destination, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer destination.Close()
	staged, err := destination.Stat()
	if err != nil {
		return err
	}
	copy.Temporary, copy.FileIdentity = temporary, backupCatalogFileIdentity(staged)
	if copy.FileIdentity == "" {
		return errors.New("cannot identify secondary backup temporary file")
	}
	if err := a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !current.pending() {
			return false, errBackupSecondaryDeferred
		}
		current.Temporary, current.FileIdentity = copy.Temporary, copy.FileIdentity
		return true, nil
	}); err != nil {
		removeOwnedSecondaryTemporary(root, binding, *copy)
		return err
	}
	reader := secondaryArchiveReader{ctx: ctx, src: source}
	written, err := a.copyArchive(destination, io.LimitReader(reader, internalbackup.MaxUploadBytes+1))
	if err != nil {
		return err
	}
	if written != info.Size() {
		return errors.New("secondary copy does not match the source size")
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	return destination.Close()
}

type secondaryArchiveReader struct {
	ctx context.Context
	src io.Reader
}

func (r secondaryArchiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.src.Read(p)
}

func secondarySourceMatches(root *os.Root, archive ownedBackupArchive, info os.FileInfo) bool {
	return archive.DirectoryIdentity != "" && backupArchiveDirectoryIdentity(root) == archive.DirectoryIdentity &&
		info.Mode().IsRegular() && backupCatalogFileIdentity(info) == archive.FileIdentity && archive.SizeBytes != nil &&
		info.Size() > 0 && info.Size() <= internalbackup.MaxUploadBytes && info.Size() == *archive.SizeBytes &&
		parseBackupStamp(archive.FileModifiedAt).Equal(info.ModTime())
}

func verifySecondaryFile(ctx context.Context, root *os.Root, name, identity string, archive ownedBackupArchive) (os.FileInfo, error) {
	file, err := openBackupCatalogFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !before.Mode().IsRegular() || backupCatalogFileIdentity(before) != identity ||
		archive.SizeBytes == nil || before.Size() != *archive.SizeBytes || before.Size() <= 0 || before.Size() > internalbackup.MaxUploadBytes {
		return nil, errors.New("secondary archive does not match its registered file and size")
	}
	digest := sha256.New()
	reader := secondaryArchiveReader{ctx: ctx, src: file}
	bytes, err := io.Copy(digest, io.LimitReader(reader, internalbackup.MaxUploadBytes+1))
	after, statErr := file.Stat()
	if err != nil || statErr != nil || bytes != before.Size() || !sameBackupIntegrityFile(before, after) ||
		fmt.Sprintf("%x", digest.Sum(nil)) != archive.SHA256 {
		return nil, errors.New("secondary archive did not pass integrity verification")
	}
	return after, nil
}

func syncSecondaryDirectory(root *os.Root) error {
	dir, err := root.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func removeOwnedSecondaryTemporary(root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy) bool {
	if copy.Temporary == "" {
		return true
	}
	if filepath.Base(copy.Temporary) != copy.Temporary || !strings.HasPrefix(copy.Temporary, ".slu-secondary-") ||
		!strings.HasSuffix(copy.Temporary, ".tmp") || copy.FileIdentity == "" {
		return true
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return false
	}
	info, err := root.Lstat(copy.Temporary)
	if err != nil {
		return os.IsNotExist(err)
	}
	if !info.Mode().IsRegular() || backupCatalogFileIdentity(info) != copy.FileIdentity {
		return true
	}
	return root.Remove(copy.Temporary) == nil
}

func (a *backupAutomation) retainSecondaryBackups(ctx context.Context, root *os.Root, binding backupSecondaryBinding) error {
	var oldest, newest backupSecondaryCopy
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding {
			return false, errBackupSecondaryDeferred
		}
		keep := max(1, state.Config.Retain)
		for i := len(state.Secondary.Copies) - 1; i >= 0; i-- {
			item := state.Secondary.Copies[i]
			if item.VerifiedAt == "" {
				continue
			}
			if newest.Archive.ID == "" {
				newest = item
			}
			if keep > 0 {
				keep--
			} else {
				oldest = item
				break
			}
		}
		return false, nil
	})
	if err != nil || oldest.Archive.ID == "" {
		return err
	}
	if !backupArchiveOwned(oldest.Archive, binding.Owner) || oldest.FileIdentity == "" ||
		!backupArchiveOwned(newest.Archive, binding.Owner) || newest.FileIdentity == "" {
		return errors.New("secondary archive has no installation ownership proof")
	}
	keeper, err := verifySecondaryFile(ctx, root, newest.Archive.Name, newest.FileIdentity, newest.Archive)
	if err != nil {
		return errors.New("secondary retention could not verify the newest copy")
	}
	checked, err := verifySecondaryFile(ctx, root, oldest.Archive.Name, oldest.FileIdentity, oldest.Archive)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding ||
			!backupArchiveMountAvailable(root.Name(), binding.Directory) {
			return false, errBackupSecondaryDeferred
		}
		newerVerified := 0
		for i := len(state.Secondary.Copies) - 1; i >= 0; i-- {
			item := state.Secondary.Copies[i]
			if item.Archive.ID != oldest.Archive.ID {
				if item.VerifiedAt != "" {
					newerVerified++
				}
				continue
			}
			if item.FileIdentity != oldest.FileIdentity || newerVerified < max(1, state.Config.Retain) {
				return false, nil
			}
			keeperInfo, keeperErr := root.Lstat(newest.Archive.Name)
			if keeperErr != nil || !sameBackupIntegrityFile(keeper, keeperInfo) {
				return false, errors.New("newest secondary copy changed during retention")
			}
			if err == nil {
				info, err := root.Lstat(item.Archive.Name)
				if err != nil || !sameBackupIntegrityFile(checked, info) {
					return false, errors.New("secondary archive changed before retention")
				}
				if err := root.Remove(item.Archive.Name); err != nil {
					return false, err
				}
				if err := syncSecondaryDirectory(root); err != nil {
					return false, err
				}
			}
			// A non-matching old name remains on disk without retention authority.
			state.Secondary.Copies = append(state.Secondary.Copies[:i], state.Secondary.Copies[i+1:]...)
			return true, nil
		}
		return false, nil
	})
}
