package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryUsesOneCapturedStateAfterFingerprintValidation(t *testing.T) {
	oldPolicy := Policy{
		ID: 61, Name: "old state", Enabled: true, TargetServers: []string{"srv-old"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	newPolicy := oldPolicy
	newPolicy.Name = "new state"
	newPolicy.TargetServers = []string{"srv-new"}
	newPolicy.UpdatedAt = "2026-01-05T03:30:00Z"

	policies := []Policy{oldPolicy}
	inventory := []servers.Server{{Name: "srv-old"}}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return cloneRecoveryPolicies(policies), nil }
	deps.SnapshotServers = func() []servers.Server { return cloneRecoveryServers(inventory) }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	storedFingerprint, err := service.schedulerRecoveryStateFingerprint()
	if err != nil {
		t.Fatalf("schedulerRecoveryStateFingerprint() error = %v", err)
	}

	watermark := time.Date(2026, 1, 5, 2, 59, 0, 0, time.UTC)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
		LoadStateFingerprint: func() (string, bool, error) {
			// Simulate an administration mutation committing after the recovery
			// tick captured and fingerprinted its state but before reconstruction.
			policies = []Policy{newPolicy}
			inventory = []servers.Server{{Name: "srv-new"}}
			return storedFingerprint, true, nil
		},
		SaveStateFingerprint: func(string) error { return nil },
	}
	now := time.Date(2026, 1, 5, 4, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want one historical row from captured state", handled)
	}
	if handled[0].Policy.ID != oldPolicy.ID || handled[0].Policy.Name != oldPolicy.Name || handled[0].Server.Name != "srv-old" || handled[0].Outcome != RunReasonSchedulerMissed {
		t.Fatalf("handled[0] = %+v, want captured old policy/server state", handled[0])
	}
}

func TestProcessMissedDueSlotReconcilesActiveRolloutWaveCompetitor(t *testing.T) {
	high := Policy{
		ID: 62, Name: "high rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 63, Name: "low immediate", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:05",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	slot := origin.Add(5 * time.Minute)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	persisted := []Run{{
		ID: 901, PolicyID: high.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunInterrupted, Reason: RunReasonRestart, JobID: "job-canary",
	}}
	var handled []ScheduledRunRequest
	var reconciled int
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
	deps.ReconcileRun = func(run Run) (Run, error) {
		reconciled++
		run.Status = RunSucceeded
		run.Reason = ""
		run.FinishedAt = origin.Add(4 * time.Minute).Format(DefaultTimestampLayout)
		return run, nil
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
	if reconciled != 1 {
		t.Fatalf("reconciled calls = %d, want 1 restart-interrupted canary reconciliation", reconciled)
	}
	if len(handled) != 1 {
		t.Fatalf("handled = %+v, want only selected lower-priority history row", handled)
	}
	if handled[0].Policy.ID != low.ID || handled[0].Server.Name != "srv-b" || handled[0].Outcome != RunReasonSuperseded {
		t.Fatalf("handled[0] = %+v, want low policy superseded by reconciled high rollout wave", handled[0])
	}
}

func TestProcessMissedDueSlotDoesNotUseFutureRolloutSuccessForHistoricalCompetition(t *testing.T) {
	high := Policy{
		ID: 64, Name: "future-success rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 65, Name: "historically unopposed", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:05",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	slot := origin.Add(5 * time.Minute)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	persisted := []Run{{
		ID: 902, PolicyID: high.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSucceeded,
		FinishedAt: origin.Add(10 * time.Minute).Format(DefaultTimestampLayout),
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
		t.Fatalf("handled = %+v, want only lower-priority historical row", handled)
	}
	if handled[0].Policy.ID != low.ID || handled[0].Outcome != RunReasonSchedulerMissed {
		t.Fatalf("handled[0] = %+v, want scheduler_missed because rollout canary finished after recovered slot", handled[0])
	}
}
