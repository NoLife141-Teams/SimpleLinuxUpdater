package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestDisabledServersExcludedFromPolicyMatchingAndSchedule(t *testing.T) {
	now := time.Date(2026, 9, 10, 1, 0, 0, 0, time.UTC)
	server := servers.Server{Name: "paused", Tags: []string{"prod"}, Disabled: true}
	deps := testServiceDeps()
	policy := Policy{ID: 1, Name: "daily", Enabled: true, CadenceKind: CadenceDaily, TimeLocal: "03:00"}
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.ListRuns = func(int) ([]Run, error) {
		return []Run{{PolicyID: 1, ServerName: server.Name, Status: RunQueued, ScheduledForUTC: now.Add(time.Hour).Format(DefaultTimestampLayout)}}, nil
	}
	svc := NewService(deps)
	for _, target := range []Policy{
		{Enabled: true}, {Enabled: true, TargetServers: []string{server.Name}},
		{Enabled: true, TargetTag: "prod"}, {Enabled: true, IncludeTags: []string{"prod"}},
	} {
		if svc.PolicyMatchesServer(target, server, MatchContext{}) {
			t.Fatalf("disabled server matched %+v", target)
		}
		enabled := server
		enabled.Disabled = false
		if !svc.PolicyMatchesServer(target, enabled, MatchContext{}) {
			t.Fatalf("enabled server did not match %+v", target)
		}
	}
	projection, err := svc.ProjectSchedule(ScheduleProjectionRequest{Now: now, Servers: []servers.Server{server}})
	if err != nil || projection.Servers[server.Name].NextRun.ScheduledForUTC != "" {
		t.Fatalf("disabled schedule = %+v, %v", projection, err)
	}
	server.Disabled = false
	projection, err = svc.ProjectSchedule(ScheduleProjectionRequest{Now: now, Servers: []servers.Server{server}})
	if err != nil || projection.Servers[server.Name].NextRun.ScheduledForUTC == "" {
		t.Fatalf("enabled schedule = %+v, %v", projection, err)
	}
}

func TestSchedulerRecoveryFingerprintIncludesServerAvailability(t *testing.T) {
	snapshot := schedulerRecoverySnapshot{Servers: []servers.Server{{Name: "srv", Tags: []string{"prod"}}}}
	before, err := snapshot.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	snapshot.Servers[0].Disabled = true
	after, err := snapshot.fingerprint()
	if err != nil {
		t.Fatal(err)
	}
	if before == after {
		t.Fatal("availability change did not invalidate scheduler recovery snapshot")
	}
}

func TestPolicyPreviewExcludesDisabledServersFromMatchesAndOccurrences(t *testing.T) {
	for _, policyEnabled := range []bool{true, false} {
		deps := testServiceDeps()
		inventory := []servers.Server{{Name: "active"}, {Name: "paused", Disabled: true}}
		deps.SnapshotServers = func() []servers.Server { return inventory }
		service := NewService(deps)
		policy := Policy{Name: "preview", Enabled: policyEnabled, TargetServers: []string{"active", "paused"},
			PackageScope: PackageScopeFull, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily,
			TimeLocal: "03:00", RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 1}
		preview, err := service.PreviewPolicy(policy)
		if err != nil || len(preview.ValidationErrors) != 0 {
			t.Fatalf("preview failed: %+v, %v", preview, err)
		}
		if len(preview.MatchedServers) != 1 || preview.MatchedServers[0].Name != "active" || preview.MatchedServers[0].RolloutStage != "canary" {
			t.Fatalf("disabled server affected rollout matches: %+v", preview.MatchedServers)
		}
		if len(preview.ExcludedServers) != 1 || preview.ExcludedServers[0].Name != "paused" || preview.ExcludedServers[0].Reason != servers.MaintenanceReadinessDisabled {
			t.Fatalf("disabled exclusion missing: %+v", preview.ExcludedServers)
		}
		if len(preview.UpcomingOccurrences) == 0 || preview.UpcomingOccurrences[0].MatchedServerCount != 1 {
			t.Fatalf("occurrence count includes disabled server: %+v", preview.UpcomingOccurrences)
		}
		inventory[1].Disabled = false
		preview, err = service.PreviewPolicy(policy)
		if err != nil || len(preview.MatchedServers) != 2 || len(preview.ExcludedServers) != 0 {
			t.Fatalf("reenabled server did not return to preview: %+v, %v", preview, err)
		}
	}
}
