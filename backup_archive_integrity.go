package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"os"
	"reflect"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

const (
	backupIntegrityInterval    = 5 * time.Minute
	backupIntegrityNewestAfter = time.Hour
	backupIntegrityOtherAfter  = 24 * time.Hour
	backupIntegrityRetryAfter  = 30 * time.Minute
	backupIntegrityReadBudget  = 45 * time.Second

	backupIntegrityMatched     = "matched"
	backupIntegrityMismatch    = "mismatch"
	backupIntegrityUnavailable = "unavailable"
)

type backupArchiveIntegrity struct {
	State       string `json:"state,omitempty"`
	CheckedAt   string `json:"checked_at,omitempty"`
	AttemptedAt string `json:"attempted_at,omitempty"`
}

func confirmedBackupIntegrity(at string) backupArchiveIntegrity {
	return backupArchiveIntegrity{State: backupIntegrityMatched, CheckedAt: at, AttemptedAt: at}
}

func (a *backupAutomation) monitorArchiveIntegrity(ctx context.Context) {
	ticker := time.NewTicker(backupIntegrityInterval)
	defer ticker.Stop()
	for {
		if err := a.checkArchiveIntegrity(ctx); err != nil && !errors.Is(err, errBackupArchiveObservationDeferred) && ctx.Err() == nil {
			log.Printf("automated backup integrity observation failed: %v", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func backupIntegrityCandidate(s backupAutomationState, now time.Time) (ownedBackupArchive, bool) {
	observation := s.Observation
	checked := parseBackupStamp(observation.CheckedAt)
	if observation.Process != backupArchiveObservationProcess || observation.Storage != "available" ||
		checked.IsZero() || now.Sub(checked) > 2*backupArchiveObservationInterval {
		return ownedBackupArchive{}, false
	}
	var newest, oldestDue *ownedBackupArchive
	var newestCapture time.Time
	for i := range s.Archives {
		archive := &s.Archives[i]
		if !backupArchiveOwned(*archive, observation.Owner) || archive.DirectoryIdentity == "" ||
			archive.DirectoryIdentity != observation.DirectoryIdentity || archive.FileIdentity == "" ||
			archive.ObservationProcess != observation.Process || archive.Verification != "verified" ||
			archive.Availability != "present" || archive.LossActive || !validBackupIntegrityDigest(archive.SHA256) {
			continue
		}
		observed := parseBackupStamp(archive.ObservedAt)
		if observed.IsZero() || now.Sub(observed) > 2*backupArchiveObservationInterval {
			continue
		}
		capture := parseBackupStamp(archive.CapturedAt)
		if archive.Integrity.State != backupIntegrityUnavailable && (newest == nil || capture.After(newestCapture)) {
			newest, newestCapture = archive, capture
		}
		if backupIntegrityDue(*archive, now, backupIntegrityOtherAfter) && (oldestDue == nil ||
			parseBackupStamp(archive.Integrity.AttemptedAt).Before(parseBackupStamp(oldestDue.Integrity.AttemptedAt))) {
			oldestDue = archive
		}
	}
	if newest != nil && backupIntegrityDue(*newest, now, backupIntegrityNewestAfter) {
		return *newest, true
	}
	if oldestDue != nil {
		return *oldestDue, true
	}
	return ownedBackupArchive{}, false
}

func validBackupIntegrityDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func backupIntegrityDue(archive ownedBackupArchive, now time.Time, interval time.Duration) bool {
	if archive.Integrity.State == backupIntegrityUnavailable {
		interval = backupIntegrityRetryAfter
	}
	last := parseBackupStamp(archive.Integrity.AttemptedAt)
	return last.IsZero() || last.After(now) || now.Sub(last) >= interval
}

func (a *backupAutomation) checkArchiveIntegrity(ctx context.Context) error {
	return a.checkArchiveIntegrityWithScan(ctx, a.scanArchiveIntegrity)
}

func (a *backupAutomation) checkArchiveIntegrityWithScan(ctx context.Context, scan func(context.Context, ownedBackupArchive) string) error {
	coord := a.deps.MaintenanceCoordinator
	lease, decision := coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	if !a.mu.TryLock() {
		lease.Close()
		return errBackupArchiveObservationDeferred
	}
	readCtx, cancelRead := context.WithTimeout(ctx, backupArchiveObservationBudget)
	before, err := a.readState(readCtx)
	cancelRead()
	a.mu.Unlock()
	lease.Close()
	if err != nil {
		return err
	}
	archive, due := backupIntegrityCandidate(before, a.now())
	if !due || !a.integrityMu.TryLock() {
		return nil
	}
	scanCtx, cancelScan := context.WithTimeout(ctx, backupIntegrityReadBudget)
	defer cancelScan()
	results := make(chan string, 1)
	go func() {
		defer a.integrityMu.Unlock()
		results <- scan(scanCtx, archive)
	}()
	state := backupIntegrityUnavailable
	select {
	case observed := <-results:
		if scanCtx.Err() == nil {
			state = observed
		}
	case <-scanCtx.Done():
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	at := backupStamp(a.now())

	// Slow filesystem I/O holds neither the publication mutex nor a shared
	// maintenance lease. Discard stale results rather than updating another
	// archive generation when publication, reverify, or restore intervenes.
	lease, decision = coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		return errBackupArchiveObservationDeferred
	}
	defer a.mu.Unlock()
	writeCtx, cancelWrite := context.WithTimeout(ctx, backupArchiveObservationBudget)
	defer cancelWrite()
	current, err := a.readState(writeCtx)
	if err != nil {
		return err
	}
	for i := range current.Archives {
		target := &current.Archives[i]
		if target.ID != archive.ID {
			continue
		}
		if !sameBackupIntegrityRegistration(*target, archive) || current.Observation.Owner != archive.Owner ||
			current.Observation.DirectoryIdentity != archive.DirectoryIdentity || current.Observation.Storage != "available" {
			return errBackupArchiveObservationDeferred
		}
		target.Integrity.State = state
		target.Integrity.AttemptedAt = at
		if state != backupIntegrityUnavailable {
			target.Integrity.CheckedAt = at
		}
		var events []AuditEvent
		if state == backupIntegrityMismatch {
			if event := a.observeArchive(target, "corrupt", at, "backup-scheduler", ""); event != nil {
				events = append(events, *event)
			}
		}
		return a.save(writeCtx, current, events...)
	}
	return errBackupArchiveObservationDeferred
}

func sameBackupIntegrityRegistration(current, previous ownedBackupArchive) bool {
	return current.Owner == previous.Owner && current.Name == previous.Name &&
		current.SHA256 == previous.SHA256 && current.DirectoryIdentity == previous.DirectoryIdentity &&
		current.FileIdentity == previous.FileIdentity && current.FileModifiedAt == previous.FileModifiedAt &&
		reflect.DeepEqual(current.SizeBytes, previous.SizeBytes) &&
		current.LastVerifiedAt == previous.LastVerifiedAt && current.Integrity == previous.Integrity &&
		current.Verification == "verified" && current.Availability == "present" && !current.LossActive
}

func (a *backupAutomation) scanArchiveIntegrity(ctx context.Context, archive ownedBackupArchive) string {
	owner, err := a.localIdentity(false)
	if err != nil || !backupArchiveOwned(archive, owner) {
		return backupIntegrityUnavailable
	}
	directory, err := backupRealPath(a.deployment.Directory)
	if err != nil {
		return backupIntegrityUnavailable
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return backupIntegrityUnavailable
	}
	defer root.Close()
	dirInfo, err := root.Lstat(".")
	if err != nil || !backupArchiveDirectoryAccessible(dirInfo) || backupCatalogFileIdentity(dirInfo) != archive.DirectoryIdentity {
		return backupIntegrityUnavailable
	}
	named, err := root.Lstat(archive.Name)
	if err != nil || !named.Mode().IsRegular() || named.Mode().Perm()&0444 == 0 {
		return backupIntegrityUnavailable
	}
	file, err := openBackupCatalogFile(root, archive.Name)
	if err != nil {
		return backupIntegrityUnavailable
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil || !sameBackupIntegrityFile(named, before) || archive.FileIdentity != backupCatalogFileIdentity(before) ||
		!parseBackupStamp(archive.FileModifiedAt).Equal(before.ModTime()) || before.Size() <= 0 ||
		before.Size() > internalbackup.MaxUploadBytes || (archive.SizeBytes != nil && before.Size() != *archive.SizeBytes) {
		return backupIntegrityUnavailable
	}
	digest := sha256.New()
	read, copyErr := io.Copy(digest, io.LimitReader(backupCatalogReader{ctx, file}, before.Size()+1))
	after, statErr := file.Stat()
	named, nameErr := root.Lstat(archive.Name)
	presentDir, dirErr := os.Lstat(directory)
	if copyErr != nil || read != before.Size() || statErr != nil || nameErr != nil || dirErr != nil || ctx.Err() != nil ||
		!sameBackupIntegrityFile(before, after) || !sameBackupIntegrityFile(after, named) ||
		!backupArchiveDirectoryAccessible(presentDir) || !os.SameFile(dirInfo, presentDir) {
		return backupIntegrityUnavailable
	}
	if hex.EncodeToString(digest.Sum(nil)) != archive.SHA256 {
		return backupIntegrityMismatch
	}
	return backupIntegrityMatched
}

func sameBackupIntegrityFile(first, second os.FileInfo) bool {
	return first.Mode().IsRegular() && second.Mode().IsRegular() && os.SameFile(first, second) &&
		first.Size() == second.Size() && first.ModTime().Equal(second.ModTime()) &&
		backupIntegrityChangeTime(first) == backupIntegrityChangeTime(second)
}
