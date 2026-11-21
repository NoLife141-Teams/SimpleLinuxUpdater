package main

import (
	"context"
	"database/sql"
	"errors"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"github.com/gin-gonic/gin"
)

const primaryAdminValidationTimeout = 10 * time.Second

func releaseBackupRequestAdmission(c *gin.Context) {
	if value, ok := c.Get(maintenanceSharedLeaseContextKey); ok {
		if lease, ok := value.(*maintenancepkg.SharedLease); ok {
			lease.Close()
		}
	}
}

type primaryAdminReference struct {
	db    *sql.DB
	epoch uint64
	state backupAutomationState
}

func (a *backupAutomation) withPrimaryAdminState(ctx context.Context, apply func(backupAutomationState) error) error {
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		return errPrimaryBackupInvalidated
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		return errPrimaryBackupBusy
	}
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	s, err := a.load(ctx)
	if err != nil {
		return err
	}
	return apply(s)
}

func (a *backupAutomation) primaryAdminReference(ctx context.Context) (primaryAdminReference, error) {
	var ref primaryAdminReference
	err := a.withPrimaryAdminState(ctx, func(s backupAutomationState) error {
		ref = primaryAdminReference{db: a.deps.DB(), epoch: a.primaryRestoreEpoch.Load(), state: s}
		return nil
	})
	return ref, err
}

func (a *backupAutomation) matchesPrimaryAdminReference(ref primaryAdminReference, s backupAutomationState) bool {
	return a.deps.DB() == ref.db && a.primaryRestoreEpoch.Load() == ref.epoch &&
		s.Approval == ref.state.Approval && s.ApprovedDirectory == ref.state.ApprovedDirectory
}

type primaryAdminIOResult[T any] struct {
	value   T
	cleanup func()
}

// Keep the primary slot until an abandoned device call actually returns. A
// successful handoff gives the caller ownership until it accepts the result.
func runPrimaryAdminIO[T any](ctx context.Context, a *backupAutomation, run func() primaryAdminIOResult[T]) (T, func(), error) {
	var zero T
	if err := ctx.Err(); err != nil {
		return zero, nil, err
	}
	if !a.primaryBusy.CompareAndSwap(false, true) {
		return zero, nil, errPrimaryBackupBusy
	}
	done := make(chan primaryAdminIOResult[T])
	go func() {
		result := run()
		select {
		case done <- result:
		case <-ctx.Done():
			if result.cleanup != nil {
				result.cleanup()
			}
			a.primaryBusy.Store(false)
		}
	}()
	select {
	case result := <-done:
		return result.value, func() {
			a.primaryBusy.Store(false)
			if result.cleanup != nil {
				result.cleanup()
			}
		}, nil
	case <-ctx.Done():
		return zero, nil, ctx.Err()
	}
}

type primaryDestinationProof struct {
	binding, identity, owner string
	checkedAt                time.Time
	err                      error
}

type primaryConfigurationAudit struct {
	actor string
	ip    string
	meta  map[string]any
}

func (a *backupAutomation) Configure(ctx context.Context, config backupScheduleConfig) (backupAutomationStatus, error) {
	return a.configurePrimary(ctx, config, nil, nil)
}

func (a *backupAutomation) configurePrimary(ctx context.Context, config backupScheduleConfig, audit *primaryConfigurationAudit, releaseAdmission func()) (backupAutomationStatus, error) {
	if err := config.validate(); err != nil {
		return backupAutomationStatus{}, err
	}
	ref, err := a.primaryAdminReference(ctx)
	if releaseAdmission != nil {
		releaseAdmission()
	}
	if err != nil {
		return backupAutomationStatus{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, primaryAdminValidationTimeout)
	defer cancel()
	proof, release, err := runPrimaryAdminIO(ctx, a, func() primaryAdminIOResult[primaryDestinationProof] {
		root, _, owner, binding, err := a.openPrimaryAdminDestination(true)
		proof := primaryDestinationProof{binding: binding, owner: owner, err: err}
		if root != nil {
			defer root.Close()
			proof.checkedAt = a.now()
			proof.identity = backupArchiveDirectoryIdentity(root)
			if !backupArchiveMountAvailable(root.Name(), proof.identity) {
				proof.err = errPrimaryBackupMountUnavailable
			}
		}
		return primaryAdminIOResult[primaryDestinationProof]{value: proof}
	})
	if err != nil {
		return backupAutomationStatus{}, err
	}
	defer release()
	if proof.err != nil && (config.Enabled || errors.Is(proof.err, errPrimaryBackupMountUnavailable)) {
		return backupAutomationStatus{}, proof.err
	}
	var status backupAutomationStatus
	err = a.withPrimaryAdminState(ctx, func(s backupAutomationState) error {
		if !a.matchesPrimaryAdminReference(ref, s) {
			return errPrimaryBackupInvalidated
		}
		if proof.err == nil {
			observation := s.Observation
			newer := observation.Process == backupArchiveObservationProcess &&
				observation.StorageHistory.Binding == backupStorageBinding(a.deployment.Directory) &&
				parseBackupStamp(observation.CheckedAt).After(proof.checkedAt)
			if newer && (observation.Storage != "available" || observation.DirectoryIdentity != proof.identity || observation.Owner != proof.owner) {
				return errPrimaryBackupMountUnavailable
			}
			if s.Approval != proof.binding || s.ApprovedDirectory != proof.identity {
				s.Temporary = ""
				s.PrimaryRetiring = ""
			}
			s.Approval, s.ApprovedDirectory = proof.binding, proof.identity
			if !newer {
				a.recordArchiveObservation(&s, newBackupArchiveObservation(proof.checkedAt, "available", proof.owner, proof.identity))
			}
		} else {
			s.Approval, s.ApprovedDirectory, s.PrimaryRetiring = "", "", ""
		}
		s.Config = config
		s.PrimaryAttempt = ""
		s.NextAt = a.next(config, a.now())
		s.Timezone = a.deps.ApplicationTime.Current().DisplayName
		s.PendingAt, s.RetryAt, s.PendingManual = "", "", false
		s.Outcome, s.Message = "configured", "Local backup destination and secret validated."
		if proof.err != nil {
			s.Outcome, s.Message = "disabled", "Automated backups are disabled. Local validation failed: "+proof.err.Error()
		}
		var events []AuditEvent
		if audit != nil && a.deps.AuditService != nil {
			events = append(events, a.deps.AuditService.PrepareEvent("", audit.actor, audit.ip,
				"backup.automatic.configured", "backup", "automatic", "success", "Automated backup settings saved", audit.meta))
		}
		if err := a.save(ctx, s, events...); err != nil {
			return err
		}
		status = a.observedPrimaryBackupStatus(s)
		return nil
	})
	return status, err
}
