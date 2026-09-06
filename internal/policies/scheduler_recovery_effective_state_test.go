package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessMissedDueSlotExcludesNotYetCreatedCompetitor(t *testing.T) {
	slot := time.Date(2026, 1, 7, 3, 0, 0, 0, time.UTC) // Wednesday.
	policies := []Policy{
		{
			ID: 41, Name: "future daily priority", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
			CadenceKind: CadenceDaily, TimeLocal: "03:00",
			CreatedAt: "2026-01-08T00:00:00Z", UpdatedAt: "2026-01-08T00:00:00Z",
		},
		{
			ID: 42, Name: "existing weekly", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
			CadenceKind: CadenceWeekly, TimeLocal: "03:00", Weekdays: []string{"wed"},
			CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		},
	}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return append([]Policy(nil), policies...), nil }
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	if err := service.processMissedDueSlot(slot, RunReasonSchedulerMissed, map[int64]struct{}{42: {}}); err != nil {
		t.Fatalf("processMissedDueSlot() error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want only existing weekly policy", handled)
	}
	if handled[0].Policy.ID != 42 || handled[0].Outcome != RunReasonSchedulerMissed {
		t.Fatalf("handled[0] = %+v, want weekly scheduler_missed", handled[0])
	}
}

func TestProcessDueWithRecoveryDoesNotProjectUpdatedPolicyBackward(t *testing.T) {
	policy := Policy{
		ID: 43, Name: "rescheduled", Enabled: true, TargetServers: []string{"srv"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "09:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-05T10:00:00Z",
	}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	watermark := time.Date(2026, 1, 5, 8, 0, 0, 0, time.UTC)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}
	service := NewService(deps)
	now := time.Date(2026, 1, 5, 10, 5, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 0 {
		t.Fatalf("handled = %+v, want no 09:00 history from configuration updated at 10:00", handled)
	}
	if want := now.UTC().Truncate(time.Minute); !watermark.Equal(want) {
		t.Fatalf("watermark = %v, want %v", watermark, want)
	}
}

func TestProcessMissedDueSlotTreatsMaintenanceRolloutHistoryAsAuthoritative(t *testing.T) {
	policy := Policy{
		ID: 44, Name: "maintenance rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	scheduled := CanonicalScheduledForUTC(slot, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	existing := []Run{{
		PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSkipped, Reason: RunReasonMaintenance,
	}}
	if rolloutHistoryIsRecoveryOnly(existing) {
		t.Fatal("maintenance skip classified as recovery-only history")
	}

	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return append([]Run(nil), existing...), nil }
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{
			{Name: "srv-a", Tags: []string{"prod"}},
			{Name: "srv-b", Tags: []string{"prod"}},
			{Name: "srv-c", Tags: []string{"prod"}},
		}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	if err := service.processMissedDueSlot(slot, RunReasonSchedulerMissed, map[int64]struct{}{policy.ID: {}}); err != nil {
		t.Fatalf("processMissedDueSlot() error = %v", err)
	}
	if len(handled) != 0 {
		t.Fatalf("handled = %+v, want persisted maintenance rollout history left authoritative", handled)
	}
}
