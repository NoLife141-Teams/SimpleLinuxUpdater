package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryUsesAllDuePoliciesForHistoricalCompetition(t *testing.T) {
	watermark := time.Date(2026, 1, 6, 4, 0, 0, 0, time.UTC) // Tuesday.
	policies := []Policy{
		{
			ID: 31, Name: "daily priority", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
			CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z",
		},
		{
			ID: 32, Name: "weekly lower", Enabled: true, TargetServers: []string{"srv"},
			PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
			CadenceKind: CadenceWeekly, TimeLocal: "03:00", Weekdays: []string{"wed"},
			CreatedAt: "2026-01-01T00:00:00Z",
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
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}

	// Friday 04:00 means the daily policy's selected recovery occurrence is
	// Friday 03:00 while the weekly policy's selected occurrence is Wednesday
	// 03:00. The daily policy must still participate in Wednesday competition.
	now := time.Date(2026, 1, 9, 4, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}

	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want weekly superseded plus latest daily miss", handled)
	}
	outcomes := map[int64]ScheduledRunRequest{}
	for _, req := range handled {
		outcomes[req.Policy.ID] = req
	}
	weekly := outcomes[32]
	if weekly.Outcome != RunReasonSuperseded || weekly.ScheduledForUTC != "2026-01-07T03:00:00.000000000Z" {
		t.Fatalf("weekly recovery = %+v, want Wednesday superseded by due daily policy", weekly)
	}
	daily := outcomes[31]
	if daily.Outcome != RunReasonSchedulerMissed || daily.ScheduledForUTC != "2026-01-09T03:00:00.000000000Z" {
		t.Fatalf("daily recovery = %+v, want only latest Friday scheduler miss", daily)
	}
}

func TestProcessDueWithRecoveryResumesPartialRecoveryOnlyRollout(t *testing.T) {
	policy := Policy{
		ID: 33, Name: "partial missed rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
		CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	watermark := origin.Add(-time.Minute)
	runs := []Run{{
		PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSkipped, Reason: RunReasonSchedulerMissed,
	}}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
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
		return []servers.Server{
			{Name: "srv-a", Tags: []string{"prod"}},
			{Name: "srv-b", Tags: []string{"prod"}},
			{Name: "srv-c", Tags: []string{"prod"}},
		}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		if req.Outcome != "" {
			runs = append(runs, Run{
				PolicyID: req.Policy.ID, ServerName: req.Server.Name, ScheduledForUTC: req.ScheduledForUTC,
				Status: RunSkipped, Reason: req.Outcome,
			})
		}
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}

	if err := service.ProcessDueWithRecovery(origin.Add(10*time.Minute), store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want only the two missing recovery rows", handled)
	}
	for _, req := range handled {
		if req.Server.Name == "srv-a" || req.Outcome != RunReasonSchedulerMissed {
			t.Fatalf("handled request = %+v, want missing targets closed as scheduler_missed", req)
		}
	}
	if len(runs) != 3 {
		t.Fatalf("runs = %+v, want recovery completed for all rollout targets", runs)
	}
}

func TestProcessDueWithRecoveryPreservesOlderPendingMaintenanceOccurrence(t *testing.T) {
	watermark := time.Date(2026, 1, 5, 2, 59, 0, 0, time.UTC)
	policy := Policy{
		ID: 34, Name: "daily maintenance history", Enabled: true, TargetServers: []string{"srv"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z",
	}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	monday := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	service.RememberMissedTick(monday)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
	}

	// Tuesday's occurrence is the policy's latest bounded recovery slot, but the
	// explicit Monday maintenance tick must still be persisted before it is
	// forgotten.
	now := time.Date(2026, 1, 6, 3, 5, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want Monday maintenance plus Tuesday scheduler miss", handled)
	}
	byTime := map[string]string{}
	for _, req := range handled {
		byTime[req.ScheduledForUTC] = req.Outcome
	}
	if byTime["2026-01-05T03:00:00.000000000Z"] != RunReasonMaintenance {
		t.Fatalf("Monday outcome = %q, want maintenance", byTime["2026-01-05T03:00:00.000000000Z"])
	}
	if byTime["2026-01-06T03:00:00.000000000Z"] != RunReasonSchedulerMissed {
		t.Fatalf("Tuesday outcome = %q, want scheduler_missed", byTime["2026-01-06T03:00:00.000000000Z"])
	}
	if got := service.PendingMissedTicks(); len(got) != 0 {
		t.Fatalf("pending ticks = %v, want processed maintenance tick forgotten", got)
	}
}
