package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryRebasesWhenSchedulerStateChanged(t *testing.T) {
	policy := Policy{
		ID: 51, Name: "inventory-sensitive", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	inventory := []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.SnapshotServers = func() []servers.Server { return append([]servers.Server(nil), inventory...) }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	storedFingerprint, err := service.schedulerRecoveryStateFingerprint()
	if err != nil {
		t.Fatalf("schedulerRecoveryStateFingerprint() error = %v", err)
	}

	// The inventory changes after the durable watermark. Historical matching is
	// now unknowable, so recovery must rebase rather than apply the current tag
	// state to an old 03:00 occurrence.
	inventory = append(inventory, servers.Server{Name: "srv-b", Tags: []string{"prod"}})
	watermark := time.Date(2026, 1, 4, 2, 59, 0, 0, time.UTC)
	var savedFingerprint string
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
		LoadStateFingerprint: func() (string, bool, error) {
			return storedFingerprint, true, nil
		},
		SaveStateFingerprint: func(value string) error {
			savedFingerprint = value
			return nil
		},
	}
	now := time.Date(2026, 1, 4, 3, 5, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 0 {
		t.Fatalf("handled = %+v, want no historical rows after scheduler state change", handled)
	}
	if want := now.UTC().Truncate(time.Minute); !watermark.Equal(want) {
		t.Fatalf("watermark = %v, want rebased %v", watermark, want)
	}
	if savedFingerprint == "" || savedFingerprint == storedFingerprint {
		t.Fatalf("saved fingerprint = %q, want new scheduler state fingerprint", savedFingerprint)
	}
}

func TestProcessMissedDueSlotResumesMarkedRecoveryAfterBlackoutRow(t *testing.T) {
	policy := Policy{
		ID: 52, Name: "marked recovery", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionAutoApply,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	scheduled := CanonicalScheduledForUTC(slot, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	existing := []Run{{
		PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSkipped, Reason: RunReasonBlackout,
	}}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return append([]Run(nil), existing...), nil }
	deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) {
		return []BlackoutWindow{{Weekdays: []string{"mon"}, StartTime: "02:00", EndTime: "04:00"}}, nil
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
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		HasRecoveryScope: func(policyID int64, scheduledForUTC string) (bool, error) {
			return policyID == policy.ID && scheduledForUTC == scheduled, nil
		},
		MarkRecoveryScope: func(int64, string) error {
			t.Fatal("existing marked recovery scope should not be marked again")
			return nil
		},
	}
	if err := service.processMissedDueSlotWithStore(slot, RunReasonSchedulerMissed, map[int64]struct{}{policy.ID: {}}, store); err != nil {
		t.Fatalf("processMissedDueSlotWithStore() error = %v", err)
	}
	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want two missing recovery rows", handled)
	}
	for _, req := range handled {
		if req.Server.Name == "srv-a" || req.Outcome != RunReasonBlackout {
			t.Fatalf("handled request = %+v, want missing marked recovery targets as blackout", req)
		}
	}
}

func TestProcessMissedDueSlotIncludesActiveRolloutWaveCompetitor(t *testing.T) {
	high := Policy{
		ID: 53, Name: "high rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 54, Name: "low immediate", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:05",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	slot := origin.Add(5 * time.Minute)
	scheduled := CanonicalScheduledForUTC(origin, DefaultTimestampLayout, func() *time.Location { return time.UTC })
	persisted := []Run{{
		PolicyID: high.ID, ServerName: "srv-a", ScheduledForUTC: scheduled,
		Status: RunSucceeded,
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
		t.Fatalf("handled = %+v, want only selected lower-priority history row", handled)
	}
	if handled[0].Policy.ID != low.ID || handled[0].Server.Name != "srv-b" || handled[0].Outcome != RunReasonSuperseded {
		t.Fatalf("handled[0] = %+v, want low policy superseded by eligible high rollout wave", handled[0])
	}
}
