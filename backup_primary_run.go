package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"os"
	"slices"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

const primaryBackupCompletionTimeout = 2 * time.Second

var (
	errPrimaryBackupBusy             = errors.New("primary backup storage is still being accessed")
	errPrimaryBackupInvalidated      = errors.New("automated backup was superseded by a configuration change or restore")
	errPrimaryBackupDeferred         = errors.New("automated backup deferred for active work")
	errPrimaryBackupApprovalRequired = errors.New("validate the local backup destination and secret before running a control backup")
)

type primaryBackupAttempt struct {
	db    *sql.DB
	state backupAutomationState
}

func (a *backupAutomation) Tick(ctx context.Context, manual bool) (resultErr error) {
	if !a.primaryBusy.CompareAndSwap(false, true) {
		if manual {
			return errPrimaryBackupBusy
		}
		return nil
	}
	var drainingStorage <-chan error
	defer func() {
		if drainingStorage == nil {
			a.primaryBusy.Store(false)
			return
		}
		go func() {
			<-drainingStorage
			a.primaryBusy.Store(false)
		}()
	}()
	a.health.Tick(a.now())
	defer func() { a.health.Finish(a.now(), resultErr) }()
	if !manual {
		needed, err := a.needsExclusiveTick(ctx)
		if err != nil || !needed {
			return err
		}
	}
	attempt, err := a.reservePrimaryBackup(ctx, manual)
	if err != nil || attempt.db == nil {
		return err
	}
	ctx, finish, err := beginHTTPTransferBudgetContext(ctx, false, false)
	if err != nil {
		return a.finishPrimaryBackup(ctx, attempt, err)
	}
	defer finish()
	ctx, cancel := context.WithTimeout(ctx, defaultHTTPResourceBudgets.backup)
	a.primaryCancel.Store(&cancel)
	defer a.primaryCancel.Store(nil)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- a.runPrimaryBackup(ctx, attempt) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		err = ctx.Err()
		// A filesystem syscall can outlive its context. Keep the slot reserved
		// until that worker exits, even though this request no longer waits.
		select {
		case <-done:
		default:
			drainingStorage = done
		}
	}
	if errors.Is(err, errPrimaryBackupDeferred) {
		return nil
	}
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	return a.finishPrimaryBackup(ctx, attempt, err)
}

// Reserve durably before touching the destination. Only this token can finish
// the occurrence, and a process restart treats an outstanding claim as failed.
func (a *backupAutomation) reservePrimaryBackup(ctx context.Context, manual bool) (primaryBackupAttempt, error) {
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return primaryBackupAttempt{}, errors.New("backup deferred while exclusive maintenance is active")
	}
	defer lease.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.load(ctx)
	if err != nil {
		return primaryBackupAttempt{}, err
	}
	if err := a.recoverInterrupted(ctx, &s); err != nil {
		return primaryBackupAttempt{}, err
	}
	now := a.now()
	if err := a.rebaseTimezone(ctx, &s, now); err != nil {
		return primaryBackupAttempt{}, err
	}
	if !s.hasPrimaryApproval() {
		if manual {
			return primaryBackupAttempt{}, errPrimaryBackupApprovalRequired
		}
		return primaryBackupAttempt{}, nil
	}
	attempt := primaryBackupAttempt{db: a.deps.DB(), state: s}
	due := s.Config.Enabled && !now.Before(parseBackupStamp(s.NextAt))
	if s.PendingAt != "" {
		due = (s.Config.Enabled || s.PendingManual) && !now.Before(parseBackupStamp(s.RetryAt))
	}
	if !manual && !due {
		if s.Temporary != "" || s.PrimaryRetiring != "" {
			return attempt, nil
		}
		return primaryBackupAttempt{}, nil
	}
	if s.PendingAt == "" {
		s.PendingManual = manual
		s.PendingAt = s.NextAt
		if manual {
			s.PendingAt = backupStamp(now)
		}
	}
	occurrence := parseBackupStamp(s.PendingAt)
	s.OccurrenceAt = s.PendingAt
	s.LastAttemptAt = backupStamp(now)
	if now.Sub(occurrence) >= backupDeferralWindow {
		s.NextAt = a.next(s.Config, now)
		s.PendingAt, s.RetryAt, s.PendingManual = "", "", false
		s.Outcome, s.Message = "failed", "Backup occurrence expired; no backlog will be replayed."
		if err := a.save(ctx, s); err != nil {
			return primaryBackupAttempt{}, err
		}
		a.record(s, "backup.automatic.failed", "failure")
		return primaryBackupAttempt{}, nil
	}
	s.Outcome, s.Message = "running", "Preparing automated backup."
	s.NextAt = a.next(s.Config, now)
	s.RetryAt = ""
	s.PrimaryAttempt = rand.Text()
	if err := a.save(ctx, s); err != nil {
		return primaryBackupAttempt{}, err
	}
	attempt.state = s
	return attempt, nil
}

// Database operations on the worker's behalf are short and occur under
// admission, so a restore can replace persistence between storage operations.
func (a *backupAutomation) updatePrimaryBackup(ctx context.Context, attempt primaryBackupAttempt, apply func(*backupAutomationState) error) error {
	return a.withPrimaryBackupAdmission(ctx, func() error {
		s, err := a.currentPrimaryAttempt(ctx, attempt)
		if err != nil {
			return err
		}
		return apply(&s)
	})
}

func (a *backupAutomation) withPrimaryBackupAdmission(ctx context.Context, apply func() error) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for ctx.Err() == nil {
		lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
		if decision.Allowed {
			if a.mu.TryLock() {
				defer lease.Close()
				defer a.mu.Unlock()
				return apply()
			}
			lease.Close()
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
	return ctx.Err()
}

func (a *backupAutomation) currentPrimaryAttempt(ctx context.Context, attempt primaryBackupAttempt) (backupAutomationState, error) {
	if err := ctx.Err(); err != nil {
		return backupAutomationState{}, err
	}
	if a.deps.DB() != attempt.db {
		return backupAutomationState{}, errPrimaryBackupInvalidated
	}
	s, err := a.load(ctx)
	if err != nil {
		return s, err
	}
	original := attempt.state
	if s.Approval != original.Approval || s.ApprovedDirectory != original.ApprovedDirectory ||
		s.PrimaryAttempt != original.PrimaryAttempt ||
		(original.PrimaryAttempt != "" && s.Outcome != "running") ||
		(original.PrimaryAttempt == "" && s.Temporary != original.Temporary) {
		return s, errPrimaryBackupInvalidated
	}
	return s, nil
}

func (a *backupAutomation) runPrimaryBackup(ctx context.Context, attempt primaryBackupAttempt) error {
	original := attempt.state
	root, pass, owner, binding, err := a.destination(false)
	if err != nil {
		return err
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !original.approvesPrimary(root, binding) {
		return errPrimaryBackupMountUnavailable
	}
	if original.PrimaryRetiring != "" {
		if err := a.recoverPrimaryRetirement(ctx, attempt, root); err != nil {
			return err
		}
	}
	if original.Temporary != "" {
		if err := a.updatePrimaryBackup(ctx, attempt, func(*backupAutomationState) error { return nil }); err != nil {
			return err
		}
		if err := a.cleanupTemporary(root, &original, owner); err != nil {
			return err
		}
		if err := a.updatePrimaryBackup(ctx, attempt, func(s *backupAutomationState) error {
			s.Temporary = ""
			return a.save(ctx, *s)
		}); err != nil {
			return err
		}
	}
	if original.PrimaryAttempt == "" {
		return nil
	}
	name := "slu-auto-" + owner + "-" + original.PrimaryAttempt + internalbackup.FileExtension
	temp := name + ".tmp"
	outcome, err := a.capturePrimaryBackup(ctx, attempt, pass, temp)
	if err != nil {
		return err
	}
	defer outcome.ExportFile.Remove()
	if outcome.Kind != backupOperationSucceeded {
		return errors.New(outcome.PublicError)
	}

	archive, err := a.publish(ctx, root, outcome.ExportFile, pass, owner, name, temp, original.ApprovedDirectory, func() error {
		return a.updatePrimaryBackup(ctx, attempt, func(*backupAutomationState) error { return nil })
	})
	if err != nil {
		// Only this attempt's temporary file can be cleaned here. The approved
		// directory and the ownership record still gate future cleanup.
		if primaryBackupMountAvailable(root, original.ApprovedDirectory) {
			original.Temporary = temp
			_ = a.cleanupTemporary(root, &original, owner)
		}
		return err
	}
	archive.JobID = outcome.JobID
	if !primaryBackupMountAvailable(root, original.ApprovedDirectory) {
		return errPrimaryBackupMountUnavailable
	}
	var registered backupAutomationState
	if err := a.updatePrimaryBackup(ctx, attempt, func(s *backupAutomationState) error {
		s.Temporary = ""
		s.Archives = append(s.Archives, archive)
		a.queueSecondaryCopy(s, archive)
		a.recordArchiveObservation(s, newBackupArchiveObservation(a.now(), "available", owner, archive.DirectoryIdentity))
		if err := a.save(ctx, *s, a.verifiedPrimaryPublicationEvents(archive, outcome.ExportFile.Size, outcome.KnownHostsIncluded)...); err != nil {
			return errors.New("published archive retained, but ownership could not be recorded")
		}
		registered = *s
		return nil
	}); err != nil {
		return err
	}
	return a.prunePrimaryBackupArchives(ctx, attempt, root, registered)
}

func (a *backupAutomation) capturePrimaryBackup(ctx context.Context, attempt primaryBackupAttempt, pass, temp string) (backupOperationOutcome, error) {
	lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupExport)
	if !decision.Allowed {
		return backupOperationOutcome{}, a.deferPrimaryAttempt(ctx, attempt)
	}
	defer lease.Close()
	if len(a.deps.ServerState.ActiveActionNames()) > 0 {
		if err := lease.Close(); err != nil {
			return backupOperationOutcome{}, err
		}
		return backupOperationOutcome{}, a.deferPrimaryAttempt(ctx, attempt)
	}
	a.mu.Lock()
	s, err := a.currentPrimaryAttempt(ctx, attempt)
	if err == nil {
		s.Temporary = temp
		err = a.save(ctx, s)
	}
	a.mu.Unlock()
	if err != nil {
		return backupOperationOutcome{}, err
	}
	outcome := a.export(ctx, backupExportCommand{
		Actor: "backup-scheduler", Lease: lease, HoldLease: true, LocalCapture: true,
		Request: backupExportRequest{Passphrase: pass, IncludeKnownHosts: &s.Config.IncludeKnownHosts},
	})
	if err := lease.Close(); err != nil {
		_ = outcome.ExportFile.Remove()
		return backupOperationOutcome{}, err
	}
	return outcome, nil
}

func (a *backupAutomation) deferPrimaryAttempt(ctx context.Context, attempt primaryBackupAttempt) error {
	if err := a.updatePrimaryBackup(ctx, attempt, func(s *backupAutomationState) error {
		s.PrimaryAttempt = ""
		return a.deferLocked(ctx, s, a.now(), s.PendingManual)
	}); err != nil {
		return err
	}
	return errPrimaryBackupDeferred
}

func (a *backupAutomation) prunePrimaryBackupArchives(ctx context.Context, attempt primaryBackupAttempt, root *os.Root, registered backupAutomationState) error {
	pruned := registered
	if err := a.retain(ctx, root, &pruned, func(archive ownedBackupArchive) (bool, error) {
		return a.removeRetiredPrimaryArchive(ctx, attempt, root, archive)
	}); err != nil {
		return err
	}
	if len(pruned.Archives) == len(registered.Archives) {
		return nil
	}
	removed := make(map[string]bool, len(registered.Archives))
	for _, archive := range registered.Archives {
		removed[archive.ID] = true
	}
	for _, archive := range pruned.Archives {
		delete(removed, archive.ID)
	}
	return a.updatePrimaryBackup(ctx, attempt, func(s *backupAutomationState) error {
		s.Archives = slices.DeleteFunc(s.Archives, func(archive ownedBackupArchive) bool { return removed[archive.ID] })
		return a.save(ctx, *s)
	})
}

func (a *backupAutomation) removeRetiredPrimaryArchive(ctx context.Context, attempt primaryBackupAttempt, root *os.Root, archive ownedBackupArchive) (bool, error) {
	protected := false
	err := a.updatePrimaryBackup(ctx, attempt, func(s *backupAutomationState) error {
		protected = a.secondaryProtectsSource(s.Secondary, archive.ID)
		if protected {
			return nil
		}
		// The observer may run between deletion and its acknowledgement. An
		// authorized retirement must not start an archive-loss episode.
		s.PrimaryRetiring = archive.ID
		return a.save(ctx, *s)
	})
	if err != nil || protected {
		return false, err
	}
	removeErr := ctx.Err()
	if removeErr == nil {
		removeErr = root.Remove(archive.Name)
		if removeErr != nil {
			// An I/O error can leave the deletion outcome unknown. Recover it
			// later against the original approved mount.
			return false, errors.New("verified backup published, but an owned archive could not be removed by retention")
		}
	}
	// Once the unlink has completed, acknowledge that fact even if the caller
	// timed out. The intent is scoped to this catalogue and approved mount.
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), primaryBackupCompletionTimeout)
	defer cancel()
	if err := a.resolvePrimaryRetirement(persistCtx, attempt, archive.ID, removeErr == nil); err != nil {
		return false, err
	}
	return removeErr == nil, removeErr
}

func (a *backupAutomation) recoverPrimaryRetirement(ctx context.Context, attempt primaryBackupAttempt, root *os.Root) error {
	id := attempt.state.PrimaryRetiring
	removed := false
	for _, archive := range attempt.state.Archives {
		if archive.ID != id {
			continue
		}
		_, err := root.Lstat(archive.Name)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("could not determine the result of an interrupted archive retirement")
		}
		removed = errors.Is(err, os.ErrNotExist)
		break
	}
	if !primaryBackupMountAvailable(root, attempt.state.ApprovedDirectory) {
		return errPrimaryBackupMountUnavailable
	}
	return a.resolvePrimaryRetirement(ctx, attempt, id, removed)
}

func (a *backupAutomation) resolvePrimaryRetirement(ctx context.Context, attempt primaryBackupAttempt, id string, removed bool) error {
	return a.withPrimaryBackupAdmission(ctx, func() error {
		if a.deps.DB() != attempt.db {
			return errPrimaryBackupInvalidated
		}
		s, err := a.load(ctx)
		if err != nil {
			return err
		}
		if s.PrimaryRetiring != id || s.Approval != attempt.state.Approval ||
			s.ApprovedDirectory != attempt.state.ApprovedDirectory {
			return errPrimaryBackupInvalidated
		}
		s.PrimaryRetiring = ""
		if removed {
			s.Archives = slices.DeleteFunc(s.Archives, func(archive ownedBackupArchive) bool { return archive.ID == id })
		}
		return a.save(ctx, s)
	})
}

func (a *backupAutomation) finishPrimaryBackup(ctx context.Context, attempt primaryBackupAttempt, runErr error) error {
	if attempt.state.PrimaryAttempt == "" {
		return runErr
	}
	persistCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), primaryBackupCompletionTimeout)
	defer cancel()
	err := a.updatePrimaryBackup(persistCtx, attempt, func(s *backupAutomationState) error {
		s.PrimaryAttempt = ""
		s.PendingAt, s.RetryAt, s.PendingManual = "", "", false
		if !parseBackupStamp(s.NextAt).After(a.now()) {
			s.NextAt = a.next(s.Config, a.now())
		}
		action, status := "backup.automatic.succeeded", "success"
		s.Outcome, s.Message = "succeeded", "Verified encrypted backup published."
		if runErr == nil {
			s.LastSuccessAt = backupStamp(a.now())
		} else {
			action, status = "backup.automatic.failed", "failure"
			s.Outcome, s.Message = "failed", runErr.Error()
		}
		if err := a.save(persistCtx, *s); err != nil {
			return err
		}
		if runErr != nil {
			a.record(*s, "backup.export", "failure")
		}
		a.record(*s, action, status)
		return nil
	})
	if err != nil {
		return err
	}
	return runErr
}
