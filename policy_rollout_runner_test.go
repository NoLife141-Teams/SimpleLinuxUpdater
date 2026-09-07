package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"
	scheduledrunspkg "debian-updater/internal/scheduledruns"
	updatespkg "debian-updater/internal/updates"
)

func TestRolloutSchedulerRunsOnlyCurrentOriginWithAuditMetadata(t *testing.T) {
	server := Server{Name: "canary", Host: "example.org", Port: 22, User: "root", Pass: "test-only", Tags: []string{"prod"}}
	deps, policy, _, jm := newScheduledRunLifecycleTestDeps(t, "rollout-origin-runner.db", server, "idle")
	origin := time.Date(2026, 9, 7, 3, 0, 0, 0, time.UTC)
	policy.Enabled = true
	policy.TargetTag = "prod"
	policy.ExecutionMode = updatePolicyExecutionScanOnly
	policy.CadenceKind = updatePolicyCadenceDaily
	policy.TimeLocal = "03:00"
	policy.RolloutMode = policypkg.RolloutCanaryWaves
	policy.CanaryCount, policy.WaveSize, policy.WaveDelayMinutes = 1, 1, 5
	policy.CreatedAt = origin.Add(-time.Minute).Format(jobTimestampLayout)
	deps.JobTimestampNow = func() string { return origin.Format(jobTimestampLayout) }
	var runnerCalls int
	deps.StartJobRunner = func(_ string, run func(), _ ...func()) {
		runnerCalls++
		run()
	}
	deps.UpdateService = NewUpdateService(UpdateServiceDeps{
		ServerState:       deps.ServerState,
		CurrentJobManager: func() *JobManager { return jm },
		JobTimestampNow:   deps.JobTimestampNow,
		HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				return HostCommandResult{Attempts: 1}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) updatespkg.PrecheckSummary {
				return updatespkg.PrecheckSummary{AllPassed: true}
			},
			DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				return HostPackageDiscoveryResult{Attempts: 1}, nil
			},
		}),
	})
	repo := defaultPolicyRepository()
	service := NewPolicyService(PolicyServiceDeps{
		ListPolicies:        func() ([]UpdatePolicy, error) { return []UpdatePolicy{policy}, nil },
		LoadOverrides:       repo.LoadAllOverrides,
		LoadGlobalBlackouts: repo.LoadGlobalBlackouts,
		ListRolloutRuns:     repo.ListRolloutRuns,
		SnapshotServers:     func() []Server { return []Server{server} },
		CurrentLocation:     func() *time.Location { return time.UTC },
		HandleScheduledRun:  scheduledrunspkg.New(deps).HandleScheduledRun,
	})
	if err := service.ProcessDue(origin.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if runnerCalls != 0 {
		t.Fatal("new rollout dispatched its pre-creation occurrence")
	}
	if err := service.ProcessDue(origin); err != nil {
		t.Fatal(err)
	}
	if runnerCalls != 1 {
		t.Fatalf("runner calls = %d, want one current canary", runnerCalls)
	}
	run, err := repo.FindRun(policy.ID, server.Name, origin.Format(jobTimestampLayout))
	if err != nil || run.Status != updatePolicyRunSucceeded || run.JobID == "" {
		t.Fatalf("current run = %+v, %v; want completed scheduled scan", run, err)
	}
	job, err := jm.GetJob(run.JobID)
	if err != nil {
		t.Fatal(err)
	}
	var meta scheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		t.Fatal(err)
	}
	if meta.PolicyID != policy.ID || meta.ScheduledFor != run.ScheduledForUTC {
		t.Fatalf("job metadata = %+v, want current policy and occurrence", meta)
	}
	events, err := deps.AuditService.List(AuditListFilter{Action: "schedule.run.started", TargetName: server.Name})
	if err != nil || events.Total != 1 {
		t.Fatalf("started audit events = %+v, %v; want one current occurrence", events, err)
	}
	var auditMeta map[string]any
	if err := json.Unmarshal([]byte(events.Items[0].MetaJSON), &auditMeta); err != nil {
		t.Fatal(err)
	}
	if auditMeta["scheduled_for_utc"] != run.ScheduledForUTC || auditMeta["policy_id"] != float64(policy.ID) || auditMeta["job_id"] != run.JobID {
		t.Fatalf("audit metadata = %+v, want exact current run identity", auditMeta)
	}
}
