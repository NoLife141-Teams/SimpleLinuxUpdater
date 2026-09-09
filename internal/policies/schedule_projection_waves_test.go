package policies

import (
	"errors"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestWaveScheduleProjectionUsesReleaseAndCompleteHistory(t *testing.T) {
	origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name      string
		now       time.Time
		status    string
		want      time.Time
		wantWait  bool
		removeTag bool
	}{
		{"future wave", origin.Add(-time.Minute), "", origin.Add(10 * time.Minute), false, false},
		{"successful canary", origin.Add(2 * time.Minute), RunSucceeded, origin.Add(10 * time.Minute), false, false},
		{"waiting canary", origin.Add(2 * time.Minute), RunRunning, origin.Add(10 * time.Minute), true, false},
		{"overdue waiting canary", origin.Add(20 * time.Minute), RunRunning, origin.Add(20 * time.Minute), true, false},
		{"failed canary", origin.Add(2 * time.Minute), RunFailed, origin.Add(24*time.Hour + 10*time.Minute), false, false},
		{"removed failed canary", origin.Add(2 * time.Minute), RunFailed, origin.Add(24 * time.Hour), false, true},
		{"unstarted past origin", origin.Add(2 * time.Minute), "", origin.Add(24*time.Hour + 10*time.Minute), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			policy := Policy{ID: 91, Name: "rollout", Enabled: true, TargetTag: "prod", CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 10, CreatedAt: origin.Add(-24 * time.Hour).Format(DefaultTimestampLayout)}
			inventory := []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}, {Name: "srv-b", Tags: []string{"prod"}}}
			if tc.removeTag {
				inventory[0].Tags = nil
			}
			deps := testServiceDeps()
			deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
			deps.ListRuns = func(int) ([]Run, error) { return nil, nil } // Recent UI history deliberately excludes the canary.
			deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
				if tc.status == "" {
					return nil, nil
				}
				for _, scope := range scopes {
					if scope.PolicyID == policy.ID && scope.ScheduledForUTC == origin.Format(DefaultTimestampLayout) {
						return []Run{{PolicyID: policy.ID, ServerName: "srv-a", ScheduledForUTC: scope.ScheduledForUTC, Status: tc.status}}, nil
					}
				}
				t.Fatal("missing origin-scoped history request")
				return nil, nil
			}
			deps.ReconcileRun = func(Run) (Run, error) { t.Fatal("read projection mutated a run"); return Run{}, nil }
			projection, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{Now: tc.now, Servers: inventory, RunLimit: 1})
			if err != nil {
				t.Fatal(err)
			}
			next := projection.Servers["srv-b"].NextRun
			if next.ScheduledForUTC != tc.want.Format(DefaultTimestampLayout) {
				t.Fatalf("next = %+v, want %s", next, tc.want)
			}
			if tc.status == RunSucceeded && (next.Reason == "rollout_waiting" || strings.Contains(next.Summary, "preceding batch")) {
				t.Fatalf("resolved predecessor still presented as waiting: %+v", next)
			}
			if tc.wantWait && !strings.Contains(next.Summary, "waiting for preceding batch") {
				t.Fatalf("missing wait caveat: %+v", next)
			}
		})
	}
}

func TestWaveScheduleProjectionCrossesMidnightAndOverlappingOrigins(t *testing.T) {
	for _, delay := range []int{10, 1500} {
		origin := time.Date(2026, 9, 7, 23, 55, 0, 0, time.UTC)
		policy := Policy{ID: 1, Enabled: true, TargetTag: "prod", CadenceKind: CadenceDaily, TimeLocal: "23:55", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: delay, CreatedAt: origin.Add(-time.Hour).Format(DefaultTimestampLayout)}
		inventory := []servers.Server{{Name: "a", Tags: []string{"prod"}}, {Name: "b", Tags: []string{"prod"}}}
		want := origin.Add(time.Duration(delay) * time.Minute)
		deps := testServiceDeps()
		deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
		deps.ListRolloutOrigins = func([]RolloutOriginRange) ([]RolloutRunScope, error) {
			return []RolloutRunScope{{PolicyID: 1, ScheduledForUTC: origin.Format(DefaultTimestampLayout)}}, nil
		}
		deps.ListRolloutRuns = func(scopes []RolloutRunScope) ([]Run, error) {
			var runs []Run
			for _, scope := range scopes {
				runs = append(runs, Run{PolicyID: 1, ServerName: "a", ScheduledForUTC: scope.ScheduledForUTC, Status: RunSucceeded})
			}
			return runs, nil
		}
		projection, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{Now: want.Add(-time.Minute), Servers: inventory})
		if err != nil {
			t.Fatal(err)
		}
		if next := projection.Servers["b"].NextRun; next.ScheduledForUTC != want.Format(DefaultTimestampLayout) {
			t.Fatalf("delay %d: next = %+v, want %s", delay, next, want)
		}
	}
}

func TestWaveScheduleProjectionFailsWhenGateHistoryUnavailable(t *testing.T) {
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{ID: 1, Enabled: true, TargetTag: "prod", CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves}}, nil
	}
	want := errors.New("history unavailable")
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return nil, want }
	_, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{Now: time.Date(2026, 9, 7, 3, 2, 0, 0, time.UTC), Servers: []servers.Server{{Name: "a", Tags: []string{"prod"}}}})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want history failure", err)
	}
}

func TestWaveScheduleProjectionOmitsBlackoutPredecessors(t *testing.T) {
	for _, window := range []BlackoutWindow{
		{Weekdays: []string{"mon"}, StartTime: "03:00", EndTime: "03:05"},
		{Weekdays: []string{"mon"}, StartTime: "03:10", EndTime: "03:15"},
	} {
		origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
		policy := Policy{ID: 1, Enabled: true, TargetTag: "prod", CadenceKind: CadenceDaily, TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 10, CreatedAt: origin.Add(-time.Hour).Format(DefaultTimestampLayout), PolicyBlackouts: []BlackoutWindow{window}}
		deps := testServiceDeps()
		deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
		deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return nil, nil }
		projection, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{Now: origin.Add(-time.Minute), Servers: []servers.Server{{Name: "a", Tags: []string{"prod"}}, {Name: "b", Tags: []string{"prod"}}}})
		if err != nil {
			t.Fatal(err)
		}
		want := origin.Add(24*time.Hour + 10*time.Minute).Format(DefaultTimestampLayout)
		if next := projection.Servers["b"].NextRun; next.ScheduledForUTC != want {
			t.Fatalf("blackout %v: next=%+v want %s", window, next, want)
		}
	}
}
