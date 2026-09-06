package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessMissedDueSlotRequiresPersistedEarlierRolloutOrigin(t *testing.T) {
	high := Policy{
		ID: 71, Name: "missed high rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 72, Name: "selected low", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:05",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	slot := origin.Add(5 * time.Minute)
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{high, low}, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return nil, nil }
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{
			{Name: "srv-a", Tags: []string{"prod"}},
			{Name: "srv-b", Tags: []string{"prod"}},
		}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	if err := service.processMissedDueSlotWithStore(slot, RunReasonSchedulerMissed, map[int64]struct{}{low.ID: {}}, SchedulerWatermarkStore{}); err != nil {
		t.Fatalf("processMissedDueSlotWithStore() error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want only selected low historical row", handled)
	}
	if handled[0].Policy.ID != low.ID || handled[0].Server.Name != "srv-b" || handled[0].Outcome != RunReasonSchedulerMissed {
		t.Fatalf("handled[0] = %+v, want low scheduler_missed without an unstarted rollout competitor", handled[0])
	}
}

func TestProcessMissedDueSlotUsesRecoveredSlotBoundaryForPersistedRollout(t *testing.T) {
	high := Policy{
		ID: 73, Name: "edited active rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-05T03:03:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 74, Name: "selected low", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:05",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	slot := origin.Add(5 * time.Minute)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	persisted := []Run{{
		ID: 903, PolicyID: high.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSucceeded,
		CreatedAt:  origin.Add(30 * time.Second).Format(DefaultTimestampLayout),
		StartedAt:  origin.Add(30 * time.Second).Format(DefaultTimestampLayout),
		FinishedAt: origin.Add(time.Minute).Format(DefaultTimestampLayout),
	}}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{high, low}, nil }
	deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
		for _, scope := range scopes {
			if scope.PolicyID == high.ID && scope.ScheduledForUTC == scheduled {
				return append([]Run(nil), persisted...), nil
			}
		}
		return nil, nil
	}
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{
			{Name: "srv-a", Tags: []string{"prod"}},
			{Name: "srv-b", Tags: []string{"prod"}},
		}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	if err := service.processMissedDueSlotWithStore(slot, RunReasonSchedulerMissed, map[int64]struct{}{low.ID: {}}, SchedulerWatermarkStore{}); err != nil {
		t.Fatalf("processMissedDueSlotWithStore() error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want only selected low historical row", handled)
	}
	if handled[0].Policy.ID != low.ID || handled[0].Server.Name != "srv-b" || handled[0].Outcome != RunReasonSuperseded {
		t.Fatalf("handled[0] = %+v, want low superseded by the persisted rollout wave after the edit boundary", handled[0])
	}
}
