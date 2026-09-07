package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessDueRolloutRequiresEffectivePersistedOrigin(t *testing.T) {
	origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		name       string
		now        time.Time
		createdAt  time.Time
		updatedAt  time.Time
		hasCanary  bool
		canaryAt   time.Time
		wantServer string
	}{
		{name: "new policy before first occurrence", now: origin.Add(-time.Minute), createdAt: origin.Add(-2 * time.Minute)},
		{name: "unstarted occurrence is not dispatched late", now: origin.Add(10 * time.Minute), createdAt: origin.Add(-24 * time.Hour)},
		{name: "current occurrence starts normally", now: origin, createdAt: origin.Add(-time.Minute), wantServer: "srv-a"},
		{name: "persisted canary waits for wave release", now: origin.Add(4 * time.Minute), createdAt: origin.Add(-24 * time.Hour), hasCanary: true},
		{name: "persisted canary releases next wave", now: origin.Add(5 * time.Minute), createdAt: origin.Add(-24 * time.Hour), hasCanary: true, wantServer: "srv-b"},
		{name: "edited policy cannot reuse old origin", now: origin.Add(5 * time.Minute), createdAt: origin.Add(-24 * time.Hour), updatedAt: origin.Add(time.Minute), hasCanary: true},
		{name: "pre-creation history cannot authorize work", now: origin.Add(5 * time.Minute), createdAt: origin.Add(time.Minute), hasCanary: true},
		{name: "policy created during origin minute continues", now: origin.Add(5 * time.Minute), createdAt: origin.Add(10 * time.Second), hasCanary: true, canaryAt: origin.Add(20 * time.Second), wantServer: "srv-b"},
		{name: "admission may persist after origin minute", now: origin.Add(5 * time.Minute), createdAt: origin.Add(50 * time.Second), hasCanary: true, canaryAt: origin.Add(61 * time.Second), wantServer: "srv-b"},
		{name: "same-minute edit cannot reuse earlier admission", now: origin.Add(5 * time.Minute), createdAt: origin.Add(-time.Minute), updatedAt: origin.Add(30 * time.Second), hasCanary: true, canaryAt: origin.Add(20 * time.Second)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			policy := Policy{
				ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod",
				ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity,
				CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves,
				CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
				CreatedAt: tt.createdAt.Format(DefaultTimestampLayout),
			}
			if !tt.updatedAt.IsZero() {
				policy.UpdatedAt = tt.updatedAt.Format(DefaultTimestampLayout)
			}
			var handled []ScheduledRunRequest
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.SnapshotServers = func() []servers.Server {
				return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
			}
			deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) {
				if !tt.hasCanary {
					return nil, nil
				}
				run := Run{PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: origin.Format(DefaultTimestampLayout), Status: RunSucceeded}
				if !tt.canaryAt.IsZero() {
					run.CreatedAt = tt.canaryAt.Format(DefaultTimestampLayout)
				}
				return []Run{run}, nil
			}
			deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
				handled = append(handled, req)
				return ScheduledRunResult{Handled: true, Inserted: true}
			}
			// An initial durable checkpoint exercises the same current-tick path
			// used at startup, without manufacturing a historical recovery gap.
			store := SchedulerWatermarkStore{
				Load: func() (time.Time, bool, error) { return tt.now, true, nil },
				Save: func(time.Time) error { return nil },
			}
			if err := NewService(deps).ProcessDueWithRecovery(tt.now, store); err != nil {
				t.Fatal(err)
			}
			if tt.wantServer == "" {
				if len(handled) != 0 {
					t.Fatalf("unexpected rollout history or dispatch: %+v", handled)
				}
				return
			}
			if len(handled) != 1 || handled[0].Server.Name != tt.wantServer || handled[0].Outcome != "" || handled[0].ScheduledForUTC != origin.Format(DefaultTimestampLayout) {
				t.Fatalf("handled = %+v, want current occurrence for %s", handled, tt.wantServer)
			}
		})
	}
}
