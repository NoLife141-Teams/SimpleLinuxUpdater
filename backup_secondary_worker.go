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
	"slices"
	"sort"
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
var errBackupSecondaryMismatch = errors.New("secondary archive does not match its registered file and digest")
var errBackupSecondaryPublicationPending = errors.New("secondary backup publication could not be confirmed")
var errBackupSecondaryStorageUnavailable = errors.New("secondary backup storage is unavailable for a new transfer")

func (a *backupAutomation) cancelSecondaryTransfer() {
	a.secondaryCancelMu.Lock()
	defer a.secondaryCancelMu.Unlock()
	if a.secondaryCancel != nil {
		a.secondaryCancel()
	}
}

func (a *backupAutomation) setSecondaryCancellation(cancel context.CancelFunc) {
	a.secondaryCancelMu.Lock()
	defer a.secondaryCancelMu.Unlock()
	a.secondaryCancel = cancel
}

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
// saving SQLite state. A stalled secondary mount cannot hold the primary
// export's admission or catalogue lock.
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
	workCtx, cancel := context.WithCancel(parent)
	ctx, stopTimeout := context.WithTimeout(workCtx, backupSecondaryCopyTimeout)
	defer stopTimeout()
	defer func() {
		cancel()
		a.setSecondaryCancellation(nil)
	}()

	var pending, cleanup backupSecondaryCopy
	var catchUp, waitingForMount bool
	var verifiedCopies []backupSecondaryCopy
	var binding backupSecondaryBinding
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) {
			return false, nil
		}
		binding = state.Secondary.Binding
		catchUp = state.Secondary.CatchUp || state.Secondary.MountUnavailable
		a.setSecondaryCancellation(cancel)
		now := a.now()
		for _, item := range state.Secondary.Copies {
			waitingForMount = waitingForMount || (item.pending() && item.Failure == "storage_unavailable")
			if item.VerifiedAt != "" {
				verifiedCopies = append(verifiedCopies, item)
			}
			if pending.Archive.ID == "" && item.pending() && !parseBackupStamp(item.RetryAt).After(now) {
				pending = item
			}
			if cleanup.Archive.ID == "" && !item.pending() && item.Temporary != "" {
				cleanup = item
			}
		}
		return state.Secondary.Observation.start(now), nil
	})
	if err != nil || binding.Path == "" {
		return err
	}

	root, actual, err := a.openSecondaryDestination()
	if err != nil || actual != binding {
		if root != nil {
			_ = root.Close()
		}
		failureCtx, cancelFailure := context.WithTimeout(workCtx, backupArchiveObservationBudget)
		defer cancelFailure()
		return a.recordSecondaryFailure(failureCtx, binding, pending.Archive.ID, "storage_unavailable")
	}
	defer root.Close()
	if err := a.observeSecondaryCopies(ctx, root, binding, verifiedCopies); err != nil {
		return err
	}
	if cleanup.Archive.ID != "" && removeOwnedSecondaryTemporary(ctx, root, binding, cleanup) {
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
	var copyErr error
	if pending.Archive.ID != "" {
		copyErr = a.stageAndPublishSecondary(ctx, root, binding, pending)
	} else if !waitingForMount {
		err := a.updateCurrentSecondary(ctx, binding, "", func(state *backupSecondaryState, _ *backupSecondaryCopy) (bool, error) {
			if state.MountUnavailable {
				state.MountUnavailable = false
				return true, nil
			}
			return false, nil
		})
		if err != nil {
			return err
		}
	}
	if copyErr != nil {
		if errors.Is(copyErr, errBackupSecondaryDeferred) || workCtx.Err() != nil {
			return copyErr
		}
		reason := "copy_failed"
		if errors.Is(copyErr, errBackupSecondaryStorageUnavailable) || !backupArchiveMountAvailable(root.Name(), binding.Directory) {
			reason = "storage_unavailable"
		} else if errors.Is(copyErr, errBackupSecondaryPublicationPending) {
			reason = "publication_pending"
		}
		recordCtx, cancelRecord := context.WithTimeout(workCtx, backupArchiveObservationBudget)
		recordErr := a.recordSecondaryFailure(recordCtx, binding, pending.Archive.ID, reason)
		cancelRecord()
		if recordErr != nil {
			return errors.Join(copyErr, recordErr)
		}
	}
	// A failed or slow transfer must not consume the time budget for checking
	// already published copies. Both phases still share restore cancellation.
	stopTimeout()
	checkCtx, stopCheck := context.WithTimeout(workCtx, backupSecondaryCopyTimeout)
	defer stopCheck()
	checkErr := a.checkSecondaryIntegrity(checkCtx, root, binding, verifiedCopies)
	if checkErr == nil && catchUp {
		checkErr = a.catchUpSecondaryCopy(checkCtx, root, binding)
	}
	if err := errors.Join(copyErr, checkErr); err != nil {
		return err
	}
	return a.keepSecondaryRetention(checkCtx, root, binding)
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
		if reason == "storage_unavailable" {
			state.Observation.record(a.now(), false)
			state.MountUnavailable = true
			state.CatchUp = true
		} else {
			if copy == nil && state.LastFailure == reason {
				return false, nil
			}
			state.LastFailure = reason
		}
		if copy != nil && copy.pending() {
			if !copy.Transferring && reason != "storage_unavailable" {
				copy.Attempts++
			} else if reason != "storage_unavailable" && reason != "publication_pending" {
				// A lost mount or an unconfirmed publication can leave the
				// expected final link recoverable, even on the last attempt.
				copy.Transferring = false
			}
			copy.Failure = reason
			delay := min(backupSecondaryMaxAttempts, max(1, copy.Attempts))
			copy.RetryAt = backupStamp(a.now().Add(time.Duration(delay) * backupSecondaryRetryBase))
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
	case os.IsNotExist(err):
		if err := a.stageSecondaryArchive(ctx, root, binding, &copy); err != nil {
			return err
		}
		verified, err := verifySecondaryFile(ctx, root, copy.Temporary, copy.FileIdentity, copy.Archive)
		if err != nil {
			return err
		}
		info, statErr := root.Lstat(copy.Temporary)
		if statErr != nil || !sameBackupIntegrityFile(verified, info) || !backupArchiveMountAvailable(root.Name(), binding.Directory) {
			return errors.New("secondary archive changed before publication")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.linkSecondaryArchive(root, copy.Temporary, copy.Archive.Name); err != nil {
			// A remote filesystem may have created the link before returning an
			// error. Reconcile the registered inode even on the last attempt.
			return fmt.Errorf("%w: link secondary archive: %w", errBackupSecondaryPublicationPending, err)
		}
	default:
		if copy.Transferring {
			return fmt.Errorf("%w: inspect secondary archive destination: %w", errBackupSecondaryPublicationPending, err)
		}
		return err
	}

	// Recover the expected final link even if the earlier attempt used the last
	// transfer slot. Link creation and temporary cleanup change the inode's
	// change time, so the recorded metadata must come from the final digest check.
	named, err := root.Lstat(copy.Archive.Name)
	if os.IsNotExist(err) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: inspect linked secondary archive: %w", errBackupSecondaryPublicationPending, err)
	}
	if !named.Mode().IsRegular() || backupCatalogFileIdentity(named) != copy.FileIdentity {
		return errBackupSecondaryMismatch
	}
	if !removeOwnedSecondaryTemporary(ctx, root, binding, copy) {
		return fmt.Errorf("%w: temporary link could not be removed", errBackupSecondaryPublicationPending)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := a.syncSecondaryDirectory(root); err != nil {
		return fmt.Errorf("%w: sync linked archive: %w", errBackupSecondaryPublicationPending, err)
	}
	published, err := verifySecondaryFile(ctx, root, copy.Archive.Name, copy.FileIdentity, copy.Archive)
	if errors.Is(err, errBackupSecondaryMismatch) || os.IsNotExist(err) {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w: verify linked archive: %w", errBackupSecondaryPublicationPending, err)
	}
	named, err = root.Lstat(copy.Archive.Name)
	if err != nil || !sameBackupIntegrityFile(named, published) || !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return fmt.Errorf("%w: archive changed after verification", errBackupSecondaryPublicationPending)
	}
	err = a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(state *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !current.pending() || current.FileIdentity != copy.FileIdentity {
			return false, errBackupSecondaryDeferred
		}
		current.VerifiedAt = backupStamp(a.now())
		current.Integrity = confirmedBackupIntegrity(current.VerifiedAt)
		current.ModifiedAt = backupStamp(published.ModTime())
		current.ChangedAt = backupStamp(backupIntegrityChangeTime(published))
		current.Transferring = false
		current.Temporary, current.RetryAt, current.Failure = "", "", ""
		state.LastFailure = ""
		state.MountUnavailable = false
		return true, nil
	})
	if err != nil {
		return fmt.Errorf("%w: register linked archive: %w", errBackupSecondaryPublicationPending, err)
	}
	return nil
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
	destination, err := a.startSecondaryTransfer(ctx, root, binding, copy)
	if err != nil {
		return err
	}
	defer destination.Close()
	reader := secondaryArchiveReader{ctx: ctx, src: source}
	writer := secondaryArchiveWriter{ctx: ctx, dst: destination}
	written, err := a.copyArchive(writer, io.LimitReader(reader, internalbackup.MaxUploadBytes+1))
	if err != nil {
		return err
	}
	if written != info.Size() {
		return errors.New("secondary copy does not match the source size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := destination.Sync(); err != nil {
		return err
	}
	return destination.Close()
}

func (a *backupAutomation) startSecondaryTransfer(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy *backupSecondaryCopy) (*os.File, error) {
	if copy.Attempts >= copy.attemptLimit() {
		return nil, errors.New("secondary backup attempt budget is exhausted")
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return nil, errBackupSecondaryStorageUnavailable
	}
	if !removeOwnedSecondaryTemporary(ctx, root, binding, *copy) {
		return nil, fmt.Errorf("%w: previous secondary backup temporary file could not be removed", errBackupSecondaryStorageUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	temporary := ".slu-secondary-" + rand.Text() + ".tmp"
	destination, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("%w: create secondary backup temporary file: %w", errBackupSecondaryStorageUnavailable, err)
	}
	staged, err := destination.Stat()
	if err != nil {
		_ = destination.Close()
		return nil, fmt.Errorf("%w: inspect secondary backup temporary file: %w", errBackupSecondaryStorageUnavailable, err)
	}
	identity := backupCatalogFileIdentity(staged)
	if identity == "" {
		_ = destination.Close()
		return nil, fmt.Errorf("%w: cannot identify secondary backup temporary file", errBackupSecondaryStorageUnavailable)
	}
	err = a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(state *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !current.pending() || current.Attempts != copy.Attempts || current.FileIdentity != copy.FileIdentity {
			return false, errBackupSecondaryDeferred
		}
		current.Temporary, current.FileIdentity = temporary, identity
		current.Attempts++
		current.Transferring = true
		current.Failure = ""
		state.MountUnavailable = false
		return true, nil
	})
	if err != nil {
		_ = destination.Close()
		// The only new temporary file this call can remove is the one it
		// created and identified before writing any archive bytes.
		removeOwnedSecondaryTemporary(ctx, root, binding, backupSecondaryCopy{Temporary: temporary, FileIdentity: identity})
		return nil, err
	}
	copy.Temporary, copy.FileIdentity = temporary, identity
	return destination, nil
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

type secondaryArchiveWriter struct {
	ctx context.Context
	dst io.Writer
}

func (w secondaryArchiveWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.dst.Write(p)
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
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() || backupCatalogFileIdentity(before) != identity ||
		archive.SizeBytes == nil || before.Size() != *archive.SizeBytes || before.Size() <= 0 || before.Size() > internalbackup.MaxUploadBytes {
		return nil, errBackupSecondaryMismatch
	}
	digest := sha256.New()
	reader := secondaryArchiveReader{ctx: ctx, src: file}
	bytes, err := io.Copy(digest, io.LimitReader(reader, internalbackup.MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !sameBackupIntegrityFile(before, after) {
		return nil, errors.New("secondary archive changed while being verified")
	}
	if bytes != before.Size() {
		return nil, io.ErrUnexpectedEOF
	}
	if fmt.Sprintf("%x", digest.Sum(nil)) != archive.SHA256 {
		return after, errBackupSecondaryMismatch
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

func removeOwnedSecondaryTemporary(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy) bool {
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
	return ctx.Err() == nil && root.Remove(copy.Temporary) == nil
}

func secondaryCopyMatches(info os.FileInfo, copy backupSecondaryCopy) bool {
	return info.Mode().IsRegular() && info.Mode().Perm()&0444 != 0 && copy.Archive.SizeBytes != nil &&
		info.Size() == *copy.Archive.SizeBytes && backupCatalogFileIdentity(info) == copy.FileIdentity &&
		info.ModTime().Equal(parseBackupStamp(copy.ModifiedAt)) &&
		backupIntegrityChangeTime(info).Equal(parseBackupStamp(copy.ChangedAt))
}

func (a *backupAutomation) observeSecondaryCopies(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copies []backupSecondaryCopy) error {
	changes := make(map[string]bool)
	for i := range copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		copy := &copies[i]
		unavailable := true
		if backupArchiveOwned(copy.Archive, binding.Owner) {
			info, err := root.Lstat(copy.Archive.Name)
			unavailable = err != nil || !secondaryCopyMatches(info, *copy)
		}
		if unavailable != copy.Unavailable {
			changes[copy.Archive.ID] = unavailable
			// Use the same availability when choosing this cycle's digest.
			copy.Unavailable = unavailable
		}
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		if err := a.recordSecondaryFailure(ctx, binding, "", "storage_unavailable"); err != nil {
			return err
		}
		return errBackupSecondaryDeferred
	}
	return a.updateCurrentSecondary(ctx, binding, "", func(state *backupSecondaryState, _ *backupSecondaryCopy) (bool, error) {
		state.Observation.record(a.now(), true)
		for i := range state.Copies {
			copy := &state.Copies[i]
			if unavailable, ok := changes[copy.Archive.ID]; ok && copy.VerifiedAt != "" {
				copy.Unavailable = unavailable
			}
		}
		return true, nil
	})
}

func (a *backupAutomation) retainSecondaryBackups(ctx context.Context, root *os.Root, binding backupSecondaryBinding) error {
	var oldest backupSecondaryCopy
	var keepers []backupSecondaryCopy
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding {
			return false, errBackupSecondaryDeferred
		}
		keep := max(1, state.Config.Retain)
		ordered := slices.Clone(state.Secondary.Copies)
		slices.Reverse(ordered)
		sort.SliceStable(ordered, func(i, j int) bool { return secondaryCapturedAfter(ordered[i], ordered[j]) })
		for _, item := range ordered {
			if item.VerifiedAt == "" {
				continue
			}
			if len(keepers) == keep {
				oldest = item
				break
			}
			if secondaryIntegrityFresh(item, a.now()) {
				keepers = append(keepers, item)
			}
		}
		return false, nil
	})
	if err != nil || oldest.Archive.ID == "" {
		return err
	}
	if !backupArchiveOwned(oldest.Archive, binding.Owner) || oldest.FileIdentity == "" {
		return errors.New("secondary archive has no installation ownership proof")
	}
	var checked os.FileInfo
	named, verifyErr := root.Lstat(oldest.Archive.Name)
	if verifyErr == nil {
		if !named.Mode().IsRegular() || backupCatalogFileIdentity(named) != oldest.FileIdentity {
			verifyErr = errBackupSecondaryMismatch
		} else {
			checked, verifyErr = verifySecondaryFile(ctx, root, oldest.Archive.Name, oldest.FileIdentity, oldest.Archive)
			if verifyErr != nil {
				checked = nil
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if verifyErr != nil && !os.IsNotExist(verifyErr) && !errors.Is(verifyErr, errBackupSecondaryMismatch) {
		// An inaccessible or unstable file may still be the registered copy.
		// Preserve its registration so retention can retry after recovery.
		return fmt.Errorf("verify old secondary archive for retention: %w", verifyErr)
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return errBackupSecondaryDeferred
	}
	// Recheck every survivor for this removal. The configured retention count
	// bounds the number of files, and each read shares the worker's deadline.
	// A proof from another cycle may predate a change invisible in metadata.
	for i, keeper := range keepers {
		verified, err := a.verifyAndRecordSecondaryIntegrity(ctx, root, binding, keeper)
		if err != nil {
			return err
		}
		if !secondaryIntegrityFresh(verified, a.now()) {
			return nil
		}
		keepers[i] = verified
	}
	if checked != nil {
		if err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
			if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding ||
				len(keepers) < max(1, state.Config.Retain) {
				return false, errBackupSecondaryDeferred
			}
			for _, keeper := range keepers {
				if !secondaryIntegrityFresh(keeper, a.now()) {
					return false, errBackupSecondaryDeferred
				}
			}
			// Persist the retirement before unlinking so a restart or a
			// directory sync failure cannot make catch-up republish it.
			return state.recordSecondaryRetirement(oldest.Archive.ID), nil
		}); err != nil {
			return err
		}
		for _, keeper := range keepers {
			keeperInfo, keeperErr := root.Lstat(keeper.Archive.Name)
			if keeperErr != nil || !secondaryCopyMatches(keeperInfo, keeper) {
				return errors.New("retained secondary copy changed during retention")
			}
		}
		if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
			return errBackupSecondaryDeferred
		}
		info, err := root.Lstat(oldest.Archive.Name)
		if err != nil || !sameBackupIntegrityFile(checked, info) {
			return errors.New("secondary archive changed before retention")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := root.Remove(oldest.Archive.Name); err != nil {
			return err
		}
	}
	// An earlier cycle may have removed the file and failed to sync its
	// directory. Confirm that removal before dropping its registration.
	if checked != nil || os.IsNotExist(verifyErr) {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := a.syncSecondaryDirectory(root); err != nil {
			return err
		}
	}
	return a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding {
			return false, errBackupSecondaryDeferred
		}
		for i := range state.Secondary.Copies {
			item := state.Secondary.Copies[i]
			if item.Archive.ID == oldest.Archive.ID && item.FileIdentity == oldest.FileIdentity {
				// Missing or non-matching names no longer convey retention authority.
				state.Secondary.Copies = slices.Delete(state.Secondary.Copies, i, i+1)
				state.recordSecondaryRetirement(item.Archive.ID)
				return true, nil
			}
		}
		return false, nil
	})
}
