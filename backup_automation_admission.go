package main

import (
	"context"
	"errors"

	maintenancepkg "debian-updater/internal/maintenance"
)

// Polling idle backup settings must not close admission for scheduled host work.
// Release shared admission before attempting exclusive admission; Tick reloads
// and revalidates the state under the exclusive lease before doing any work.
func (a *backupAutomation) needsExclusiveTick(ctx context.Context) (bool, error) {
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return false, errors.New("backup deferred while exclusive maintenance is active")
	}
	defer lease.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.load(ctx)
	if err != nil {
		return false, err
	}
	// Interrupted work and temporary archive cleanup still need exclusive admission.
	if s.Outcome == "running" || s.Temporary != "" {
		return true, nil
	}
	now := a.now()
	if err := a.rebaseTimezone(ctx, &s, now); err != nil {
		return false, err
	}
	if (!s.Config.Enabled && !s.PendingManual) || s.Approval == "" {
		return false, nil
	}
	if s.PendingAt != "" {
		return !now.Before(parseBackupStamp(s.RetryAt)), nil
	}
	return !now.Before(parseBackupStamp(s.NextAt)), nil
}
