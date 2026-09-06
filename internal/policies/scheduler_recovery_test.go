package policies

import (
	"context"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryRecordsLatestMissedOccurrenceWithoutExecuting(t *testing.T) {
	watermark := time.Date(2026, 1, 1, 2, 59, 0, 0, time.UTC)
	found := true
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{
			ID: 1, Name: "daily", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
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
		Save: func(value time.Time) error {
			watermark = value
			found = true
			return nil
		},
	}

	now := time.Date(2026, 1, 4, 3, 5, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want exactly one latest missed occurrence", handled)
	}
	if handled[0].Outcome != RunReasonSchedulerMissed {
		t.Fatalf("missed outcome = %q, want %q", handled[0].Outcome, RunReasonSchedulerMissed)
	}
	if handled[0].ScheduledForUTC != "2026-01-04T03:00:00.000000000Z" {
		t.Fatalf("scheduled_for_utc = %q, want latest daily occurrence", handled[0].ScheduledForUTC)
	}
	if !watermark.Equal(now.UTC().Truncate(time.Minute)) {
		t.Fatalf("watermark = %v, want %v", watermark, now.UTC().Truncate(time.Minute))
	}
}

func TestProcessDueWithRecoveryInitializesWatermarkWithoutRetroactiveReplay(t *testing.T) {
	var watermark time.Time
	found := false
	handled := 0
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{
			ID: 2, Name: "daily", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
			CadenceKind: CadenceDaily, TimeLocal: "03:00",
		}}, nil
	}
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(ScheduledRunRequest) ScheduledRunResult {
		handled++
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, found, nil },
		Save: func(value time.Time) error {
			watermark = value
			found = true
			return nil
		},
	}

	now := time.Date(2026, 1, 4, 3, 5, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if handled != 0 {
		t.Fatalf("handled = %d, want no retroactive replay without an existing watermark", handled)
	}
	if !watermark.Equal(now.UTC().Truncate(time.Minute)) {
		t.Fatalf("watermark = %v, want initialized current minute", watermark)
	}
}

func TestProcessDueWithRecoveryPreservesMaintenanceSkipReason(t *testing.T) {
	watermark := time.Date(2026, 1, 5, 2, 59, 0, 0, time.UTC)
	var handled []ScheduledRunRequest
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	deps := testServiceDeps()
	deps.Maintenance = coordinator
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{
			ID: 3, Name: "maintenance", Enabled: true, TargetServers: []string{"srv"},
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
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error {
			watermark = value
			return nil
		},
	}

	exclusive, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	blockedSlot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(blockedSlot, store); err != nil {
		t.Fatalf("blocked ProcessDueWithRecovery() error = %v", err)
	}
	if len(service.PendingMissedTicks()) != 1 {
		t.Fatalf("pending missed ticks = %v, want blocked maintenance tick", service.PendingMissedTicks())
	}
	if err := exclusive.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := service.ProcessDueWithRecovery(blockedSlot.Add(time.Minute), store); err != nil {
		t.Fatalf("recovery ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 1 || handled[0].Outcome != RunReasonMaintenance {
		t.Fatalf("handled = %+v, want one maintenance skip", handled)
	}
	if got := service.PendingMissedTicks(); len(got) != 0 {
		t.Fatalf("pending missed ticks after recovery = %v, want none", got)
	}
}

func TestProcessDueWithRecoveryContinuesPersistedCanaryWave(t *testing.T) {
	policy := Policy{
		ID: 4, Name: "waves", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	watermark := origin.Add(4 * time.Minute)
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) {
		return []Run{{
			PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: scheduled, Status: RunSucceeded,
		}}, nil
	}
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error {
			watermark = value
			return nil
		},
	}

	if err := service.ProcessDueWithRecovery(origin.Add(10*time.Minute), store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 1 || handled[0].Server.Name != "srv-b" || handled[0].Outcome != "" {
		t.Fatalf("handled = %+v, want current tick to continue the persisted first wave", handled)
	}
}
