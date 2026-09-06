package policies

import (
	"context"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryClosesWhollyMissedRolloutWithoutGateHistory(t *testing.T) {
	policy := Policy{
		ID: 21, Name: "missed waves", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	watermark := time.Date(2026, 1, 5, 2, 59, 0, 0, time.UTC)
	var runs []Run
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
		if len(scopes) == 0 {
			return nil, nil
		}
		var out []Run
		for _, run := range runs {
			for _, scope := range scopes {
				if run.PolicyID == scope.PolicyID && run.ScheduledForUTC == scope.ScheduledForUTC {
					out = append(out, run)
				}
			}
		}
		return out, nil
	}
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		if req.Outcome != "" {
			runs = append(runs, Run{
				PolicyID: req.Policy.ID, ServerName: req.Server.Name,
				ScheduledForUTC: req.ScheduledForUTC, Status: RunSkipped, Reason: req.Outcome,
			})
		}
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}

	if err := service.ProcessDueWithRecovery(time.Date(2026, 1, 5, 3, 10, 0, 0, time.UTC), store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want both rollout targets closed exactly once", handled)
	}
	for _, req := range handled {
		if req.Outcome != RunReasonSchedulerMissed {
			t.Fatalf("handled request = %+v, want only scheduler_missed and no rollout_gate", req)
		}
	}
}

func TestProcessDueWithRecoveryDoesNotManufactureOccurrenceBeforePolicyCreation(t *testing.T) {
	watermark := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	handled := 0
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{
			ID: 22, Name: "new policy", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
			CadenceKind: CadenceDaily, TimeLocal: "09:00",
			CreatedAt: "2026-01-05T10:00:00Z",
		}}, nil
	}
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(ScheduledRunRequest) ScheduledRunResult {
		handled++
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}

	if err := service.ProcessDueWithRecovery(time.Date(2026, 1, 5, 10, 5, 0, 0, time.UTC), store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if handled != 0 {
		t.Fatalf("handled = %d, want no occurrence before policy creation", handled)
	}
}

func TestProcessDueWithRecoveryPreservesInitialMaintenanceTickWithoutWatermark(t *testing.T) {
	var watermark time.Time
	found := false
	var handled []ScheduledRunRequest
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	deps := testServiceDeps()
	deps.Maintenance = coordinator
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{
			ID: 23, Name: "first maintenance", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
			CadenceKind: CadenceDaily, TimeLocal: "03:00",
		}}, nil
	}
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, found, nil },
		Save: func(value time.Time) error { watermark = value; found = true; return nil },
	}

	exclusive, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(origin, store); err != nil {
		t.Fatalf("blocked ProcessDueWithRecovery() error = %v", err)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := service.ProcessDueWithRecovery(origin.Add(time.Minute), store); err != nil {
		t.Fatalf("recovery ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 1 || handled[0].Outcome != RunReasonMaintenance {
		t.Fatalf("handled = %+v, want first blocked occurrence persisted as maintenance skip", handled)
	}
	if !found || !watermark.Equal(origin.Add(time.Minute)) {
		t.Fatalf("watermark = %v found=%t, want initialized after preserved maintenance tick", watermark, found)
	}
}
