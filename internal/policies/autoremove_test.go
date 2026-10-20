package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestAutoremovePolicyNormalizesPersistsAndKeepsIndependentSchedule(t *testing.T) {
	policy := Policy{
		Name: "Weekly cleanup", Enabled: true, TargetTag: "prod",
		ExecutionMode: " AUTOREMOVE ", PackageScope: "security", UpgradeMode: "full",
		CadenceKind: CadenceWeekly, Weekdays: []string{"sun"}, TimeLocal: "04:30",
		ApprovalTimeoutMinutes: 90,
	}
	if err := NewService(testServiceDeps()).NormalizePolicy(&policy); err != nil {
		t.Fatal(err)
	}
	if policy.ExecutionMode != ExecutionAutoremove || policy.PackageScope != PackageScopeFull || policy.UpgradeMode != UpgradeModeStandard || policy.ApprovalTimeoutMinutes != 0 {
		t.Fatalf("cleanup retained update-only settings: %+v", policy)
	}
	repo, _ := newTestRepository(t)
	saved, err := repo.CreatePolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.GetPolicy(saved.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ExecutionMode != ExecutionAutoremove || loaded.CadenceKind != CadenceWeekly || loaded.TimeLocal != "04:30" || len(loaded.Weekdays) != 1 || loaded.Weekdays[0] != "sun" {
		t.Fatalf("cleanup schedule did not round trip: %+v", loaded)
	}
	// No update-only fields are needed to submit a cleanup policy through the API.
	policy.PackageScope, policy.UpgradeMode = "", ""
	if err := NewService(testServiceDeps()).NormalizePolicy(&policy); err != nil {
		t.Fatal(err)
	}
}

func TestAutoremoveDoesNotSupersedeUpdateOrScanPolicies(t *testing.T) {
	svc := NewService(testServiceDeps())
	cleanup := ScheduledCandidate{Policy: Policy{ID: 1, ExecutionMode: ExecutionAutoremove, PackageScope: PackageScopeFull}}
	for _, mode := range []string{ExecutionApprovalRequired, ExecutionAutoApply, ExecutionScanOnly} {
		other := ScheduledCandidate{Policy: Policy{ID: 99, ExecutionMode: mode, PackageScope: PackageScopeSecurity}}
		if !svc.ComparePolicyCandidates(other, cleanup) {
			t.Fatalf("%s must take priority over coincident cleanup", mode)
		}
	}
}

func TestWeeklyAutoremoveDispatchRespectsDayTimeAndBlackouts(t *testing.T) {
	for _, tc := range []struct {
		name        string
		now         time.Time
		blackout    bool
		wantCount   int
		wantOutcome string
	}{
		{"before time", time.Date(2026, 10, 25, 4, 29, 0, 0, time.UTC), false, 0, ""},
		{"wrong day", time.Date(2026, 10, 26, 4, 30, 0, 0, time.UTC), false, 0, ""},
		{"due", time.Date(2026, 10, 25, 4, 30, 0, 0, time.UTC), false, 1, ""},
		{"blackout", time.Date(2026, 10, 25, 4, 30, 0, 0, time.UTC), true, 1, RunReasonBlackout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := Policy{ID: 1, Name: "Weekly cleanup", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionAutoremove,
				PackageScope: PackageScopeFull, CadenceKind: CadenceWeekly, Weekdays: []string{"sun"}, TimeLocal: "04:30",
				CreatedAt: "2026-10-01T00:00:00Z"}
			if tc.blackout {
				policy.PolicyBlackouts = []BlackoutWindow{{Weekdays: []string{"sun"}, StartTime: "04:00", EndTime: "05:00"}}
			}
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "prod-host", Tags: []string{"prod"}}} }
			var requests []ScheduledRunRequest
			deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
				requests = append(requests, req)
				return ScheduledRunResult{Handled: true, Inserted: true}
			}
			if err := NewService(deps).ProcessDueSlot(ScheduleRequest{Now: tc.now}); err != nil {
				t.Fatal(err)
			}
			if len(requests) != tc.wantCount {
				t.Fatalf("dispatched %d runs, want %d", len(requests), tc.wantCount)
			}
			if tc.wantCount > 0 && (requests[0].Outcome != tc.wantOutcome || requests[0].Policy.ExecutionMode != ExecutionAutoremove) {
				t.Fatalf("unexpected cleanup dispatch: %+v", requests[0])
			}
		})
	}
}
