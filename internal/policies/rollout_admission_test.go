package policies

import (
	"fmt"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestDelayedWaveRespectsCurrentBlackout(t *testing.T) {
	for _, scope := range []string{"global", "policy"} {
		t.Run(scope, func(t *testing.T) {
			origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
			policy := Policy{ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5, CreatedAt: origin.Add(-24 * time.Hour).Format(DefaultTimestampLayout)}
			windows := []BlackoutWindow{{Weekdays: []string{"mon"}, StartTime: "03:04", EndTime: "04:00"}}
			if scope == "policy" {
				policy.PolicyBlackouts = windows
			}
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.SnapshotServers = func() []servers.Server {
				return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
			}
			if scope == "global" {
				deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) { return windows, nil }
			}
			deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) {
				return []Run{{PolicyID: 91, ServerName: "srv-a", ScheduledForUTC: origin.Format(DefaultTimestampLayout), Status: RunSucceeded}}, nil
			}
			var handled []ScheduledRunRequest
			deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
				handled = append(handled, req)
				return ScheduledRunResult{Handled: true, Inserted: true}
			}
			svc := NewService(deps)
			now := origin.Add(5 * time.Minute)
			if !svc.BlackoutApplies(now, windows) {
				t.Fatal("fixture not in blackout")
			}
			if err := svc.ProcessDueSlot(ScheduleRequest{Now: now}); err != nil {
				t.Fatal(err)
			}
			if len(handled) != 1 || handled[0].Server.Name != "srv-b" || handled[0].Outcome != RunReasonBlackout {
				t.Fatalf("unexpected dispatch: %+v", handled)
			}
			if handled[0].ScheduledForUTC != origin.Format(DefaultTimestampLayout) {
				t.Fatal("blackout changed occurrence identity")
			}
		})
	}
}

func TestDailyRolloutContinuesPastNextOccurrence(t *testing.T) {
	for _, config := range []struct{ delay, count int }{{1440, 2}, {720, 3}, {1440, 4}} {
		t.Run(fmt.Sprintf("delay-%d-servers-%d", config.delay, config.count), func(t *testing.T) {
			origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
			policy := Policy{ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod", ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: config.delay, CreatedAt: origin.Add(-24 * time.Hour).Format(DefaultTimestampLayout)}
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.SnapshotServers = func() []servers.Server {
				inventory := make([]servers.Server, config.count)
				for i := range inventory {
					inventory[i] = servers.Server{Name: fmt.Sprintf("srv-%c", 'a'+i), Tags: []string{"prod"}}
				}
				return inventory
			}
			var runs []Run
			deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
				var out []Run
				for _, r := range runs {
					for _, sc := range scopes {
						if sc.PolicyID == r.PolicyID && sc.ScheduledForUTC == r.ScheduledForUTC {
							out = append(out, r)
						}
					}
				}
				return out, nil
			}
			deps.ListRolloutOrigins = func([]int64) ([]RolloutRunScope, error) {
				origins := []RolloutRunScope{}
				seen := map[string]bool{}
				for _, run := range runs {
					if !seen[run.ScheduledForUTC] {
						origins = append(origins, RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: run.ScheduledForUTC})
						seen[run.ScheduledForUTC] = true
					}
				}
				return origins, nil
			}
			waveRuns := map[string]int{}
			deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
				waveRuns[req.Server.Name]++
				runs = append(runs, Run{PolicyID: policy.ID, ServerName: req.Server.Name, ScheduledForUTC: req.ScheduledForUTC, Status: RunSucceeded})
				return ScheduledRunResult{Handled: true, Inserted: true}
			}
			svc := NewService(deps)
			if err := svc.NormalizePolicy(&policy); err != nil {
				t.Fatalf("fixture rejected: %v", err)
			}
			for minute := 0; minute <= 3*24*60; minute++ {
				if err := svc.ProcessDueSlot(ScheduleRequest{Now: origin.Add(time.Duration(minute) * time.Minute)}); err != nil {
					t.Fatal(err)
				}
			}
			if waveRuns["srv-a"] != 4 {
				t.Fatalf("daily canary runs=%d, want 4", waveRuns["srv-a"])
			}
			for _, server := range deps.SnapshotServers() {
				if waveRuns[server.Name] == 0 {
					t.Fatalf("%s never dispatched: %+v", server.Name, runs)
				}
			}

		})
	}
}
