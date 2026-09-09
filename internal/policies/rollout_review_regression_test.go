package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestRegressionFailedCanaryInventoryChange(t *testing.T) {
	for _, change := range []string{"unchanged-control", "remove-tag", "rename"} {
		t.Run(change, func(t *testing.T) {
			origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
			policy := Policy{ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5, CreatedAt: origin.Add(-24 * time.Hour).Format(DefaultTimestampLayout)}
			inventory := []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.SnapshotServers = func() []servers.Server { return inventory }
			deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) {
				return []Run{{PolicyID: 91, ServerName: "srv-a", ScheduledForUTC: origin.Format(DefaultTimestampLayout), Status: RunFailed}}, nil
			}
			var handled []ScheduledRunRequest
			deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
				handled = append(handled, req)
				return ScheduledRunResult{Handled: true, Inserted: true}
			}
			if change == "remove-tag" {
				inventory[0].Tags = nil
			}
			if change == "rename" {
				inventory[0].Name = "srv-z"
			}
			if err := NewService(deps).ProcessDueSlot(ScheduleRequest{Now: origin.Add(5 * time.Minute)}); err != nil {
				t.Fatal(err)
			}
			for _, req := range handled {
				t.Logf("dispatch server=%s outcome=%q occurrence=%s", req.Server.Name, req.Outcome, req.ScheduledForUTC)
			}
			if change == "unchanged-control" {
				if len(handled) != 1 || handled[0].Outcome != RunReasonRolloutGate {
					t.Fatalf("control failed: %+v", handled)
				}
			} else {
				for _, req := range handled {
					if req.Outcome == "" {
						t.Errorf("failed canary was bypassed: launched %s", req.Server.Name)
					}
				}
			}
		})
	}
}

func TestRegressionNextRunForDelayedWave(t *testing.T) {
	origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	policy := Policy{ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 10, CreatedAt: origin.Add(-24 * time.Hour).Format(DefaultTimestampLayout)}
	inventory := []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
	runs := []Run{{PolicyID: 91, ServerName: "srv-a", ScheduledForUTC: origin.Format(DefaultTimestampLayout), Status: RunSucceeded}}
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.SnapshotServers = func() []servers.Server { return inventory }
	deps.ListRuns = func(int) ([]Run, error) { return runs, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return runs, nil }
	var launched []ScheduledRunRequest
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		launched = append(launched, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	svc := NewService(deps)
	projection, err := svc.ProjectSchedule(ScheduleProjectionRequest{Now: origin.Add(2 * time.Minute), Servers: inventory})
	if err != nil {
		t.Fatal(err)
	}
	next := projection.Servers["srv-b"].NextRun
	if err := svc.ProcessDueSlot(ScheduleRequest{Now: origin.Add(10 * time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if len(launched) != 1 || launched[0].Server.Name != "srv-b" || launched[0].Outcome != "" {
		t.Fatalf("fixture did not release wave: %+v", launched)
	}
	t.Logf("03:02 displayed next=%s; actual dispatch 03:10 same day", next.ScheduledForUTC)
	displayed, err := time.Parse(DefaultTimestampLayout, next.ScheduledForUTC)
	if err != nil {
		t.Fatal(err)
	}
	if displayed.After(origin.Add(10 * time.Minute)) {
		t.Fatal("next-run projection hides imminent delayed wave until tomorrow")
	}
}
