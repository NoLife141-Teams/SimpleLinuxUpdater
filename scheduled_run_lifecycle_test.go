package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	updatespkg "debian-updater/internal/updates"
)

func newScheduledRunLifecycleTestDeps(t *testing.T, dbName string, server Server, status string) (AppDeps, UpdatePolicy, UpdatePolicyRun, *JobManager) {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), dbName)
	prepareUpdatePolicyTestState(t, dbFile)

	state := newServerState()
	state.Lock()
	state.SetServers([]Server{server})
	state.SetStatusMap(map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: status, Tags: server.Tags, Logs: "previous logs"},
	})
	state.Unlock()

	jm := newJobManagerWithRuntime(getDB(), nil, state, func() bool { return false })
	policy := UpdatePolicy{
		ID:                     101,
		Name:                   "Scheduled lifecycle policy",
		ExecutionMode:          updatePolicyExecutionApprovalRequired,
		PackageScope:           updatePolicyPackageScopeSecurity,
		UpgradeMode:            updatePolicyUpgradeModeStandard,
		ApprovalTimeoutMinutes: defaultScheduledApprovalTimeoutMinutes,
	}
	run, inserted, err := createUpdatePolicyRun(UpdatePolicyRun{
		PolicyID:        policy.ID,
		PolicyName:      policy.Name,
		ServerName:      server.Name,
		ScheduledForUTC: "2026-07-05T14:00:00Z",
		ExecutionMode:   policy.ExecutionMode,
		PackageScope:    policy.PackageScope,
		UpgradeMode:     policy.UpgradeMode,
		Status:          updatePolicyRunQueued,
		Summary:         "Queued",
		ResultJSON:      "{}",
	})
	if err != nil || !inserted {
		t.Fatalf("createUpdatePolicyRun() = (%+v, %t, %v), want inserted", run, inserted, err)
	}

	deps := AppDeps{
		ServerState:       state,
		CurrentJobManager: func() *JobManager { return jm },
		JobTimestampNow:   func() string { return "2026-07-05T14:00:01Z" },
		LoadRetryPolicy: func() RetryPolicy {
			return RetryPolicy{MaxAttempts: 4, BaseDelay: time.Second, MaxDelay: 5 * time.Second, JitterPct: 7}
		},
		StartJobRunner:                  func(string, func()) {},
		StartScheduledRunReconciliation: func(int64, string) {},
	}
	deps = deps.withDefaults()
	return deps, policy, run, jm
}

func getScheduledLifecycleRun(t *testing.T, deps AppDeps, runID int64) UpdatePolicyRun {
	t.Helper()
	run, err := deps.PolicyRepository.GetRun(runID)
	if err != nil {
		t.Fatalf("GetRun(%d) unexpected error: %v", runID, err)
	}
	return run
}

func TestScheduledRunLifecycleHandleScheduledRunDuplicateNoops(t *testing.T) {
	server := Server{Name: "srv-duplicate", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, run, jm := newScheduledRunLifecycleTestDeps(t, "scheduled-run-duplicate.db", server, "idle")

	result := newScheduledRunLifecycle(deps).HandleScheduledRun(PolicyScheduledRunRequest{
		Policy:          policy,
		Server:          server,
		ScheduledForUTC: run.ScheduledForUTC,
	})
	if !result.Handled || result.Inserted || result.RunID != run.ID || result.Status != updatePolicyRunQueued {
		t.Fatalf("HandleScheduledRun duplicate = %+v, want handled existing queued run", result)
	}
	if job, _ := jm.FindLatestActiveJobByServerAndKind(server.Name, jobKindUpdate); job != nil {
		t.Fatalf("latest active update job = %+v; want no job dispatched for duplicate run", job)
	}
}

func TestScheduledRunLifecycleHandleScheduledRunRecordsSkippedCandidate(t *testing.T) {
	server := Server{Name: "srv-skipped-candidate", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, _, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-skipped-candidate.db", server, "idle")
	scheduledForUTC := "2026-07-05T14:01:00Z"

	result := newScheduledRunLifecycle(deps).HandleScheduledRun(PolicyScheduledRunRequest{
		Policy:          policy,
		Server:          server,
		ScheduledForUTC: scheduledForUTC,
		Outcome:         updatePolicyRunReasonBlackout,
	})
	if !result.Handled || !result.Inserted || result.Status != updatePolicyRunSkipped {
		t.Fatalf("HandleScheduledRun skipped = %+v, want inserted skipped run", result)
	}
	run, err := deps.PolicyRepository.FindRun(policy.ID, server.Name, scheduledForUTC)
	if err != nil {
		t.Fatalf("FindRun(skipped) unexpected error: %v", err)
	}
	if run.Status != updatePolicyRunSkipped || run.Reason != updatePolicyRunReasonBlackout || run.Summary != "Scheduled run skipped due to blackout window" {
		t.Fatalf("skipped run = %+v, want blackout skipped summary", run)
	}
	audits, err := deps.AuditService.List(AuditListFilter{Action: "schedule.run.skipped", TargetName: server.Name})
	if err != nil {
		t.Fatalf("List skipped audits unexpected error: %v", err)
	}
	if audits.Total != 1 || audits.Items[0].Status != "ignored" {
		t.Fatalf("skipped audits = %+v, want one ignored audit", audits.Items)
	}

	duplicate := newScheduledRunLifecycle(deps).HandleScheduledRun(PolicyScheduledRunRequest{
		Policy:          policy,
		Server:          server,
		ScheduledForUTC: scheduledForUTC,
		Outcome:         updatePolicyRunReasonBlackout,
	})
	if !duplicate.Handled || duplicate.Inserted || duplicate.RunID != run.ID {
		t.Fatalf("HandleScheduledRun duplicate skipped = %+v, want existing run no-op", duplicate)
	}
	audits, err = deps.AuditService.List(AuditListFilter{Action: "schedule.run.skipped", TargetName: server.Name})
	if err != nil {
		t.Fatalf("List skipped audits after duplicate unexpected error: %v", err)
	}
	if audits.Total != 1 {
		t.Fatalf("skipped audit total after duplicate = %d, want 1", audits.Total)
	}
}

func TestScheduledRunLifecycleExclusiveMaintenanceSkip(t *testing.T) {
	server := Server{Name: "srv-maintenance-barrier", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-maintenance.db", server, "idle")
	exclusive, decision := deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	defer exclusive.Close()

	newScheduledRunLifecycle(deps).Execute(run, policy, server)

	current := getScheduledLifecycleRun(t, deps, run.ID)
	if current.Status != updatePolicyRunSkipped || current.Reason != updatePolicyRunReasonMaintenance {
		t.Fatalf("run = status %q reason %q, want skipped/maintenance", current.Status, current.Reason)
	}
	if current.Summary != "Maintenance mode active; scheduled run skipped" {
		t.Fatalf("summary = %q, want maintenance run skip", current.Summary)
	}
}

func TestScheduledRunLifecycleMissingAndBusyServer(t *testing.T) {
	server := Server{Name: "srv-missing-busy", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}

	t.Run("missing server", func(t *testing.T) {
		deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-missing.db", server, "idle")
		deps.ServerState.Lock()
		deps.ServerState.SetStatusMap(map[string]*ServerStatus{})
		deps.ServerState.Unlock()

		newScheduledRunLifecycle(deps).Execute(run, policy, server)

		current := getScheduledLifecycleRun(t, deps, run.ID)
		if current.Status != updatePolicyRunFailed || current.Reason != updatePolicyRunReasonMissing {
			t.Fatalf("run = status %q reason %q, want failed/missing", current.Status, current.Reason)
		}
		if current.Summary != "Server unavailable for scheduled update" {
			t.Fatalf("summary = %q, want unavailable update", current.Summary)
		}
	})

	t.Run("busy server", func(t *testing.T) {
		deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-busy.db", server, "updating")

		newScheduledRunLifecycle(deps).Execute(run, policy, server)

		current := getScheduledLifecycleRun(t, deps, run.ID)
		if current.Status != updatePolicyRunSkipped || current.Reason != updatePolicyRunReasonBusy {
			t.Fatalf("run = status %q reason %q, want skipped/busy", current.Status, current.Reason)
		}
		if current.Summary != "Server busy; scheduled update skipped" {
			t.Fatalf("summary = %q, want busy update skip", current.Summary)
		}
	})
}

func TestScheduledRunLifecycleUpdateJobCreationSuccessLoadsRetryOnce(t *testing.T) {
	server := Server{Name: "srv-update-created", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, run, jm := newScheduledRunLifecycleTestDeps(t, "scheduled-run-update-created.db", server, "idle")

	var retryLoads int32
	var runnerJobID string
	var reconciledRunID int64
	var reconciledJobID string
	deps.LoadRetryPolicy = func() RetryPolicy {
		atomic.AddInt32(&retryLoads, 1)
		return RetryPolicy{MaxAttempts: 3, BaseDelay: 2 * time.Second, MaxDelay: 9 * time.Second, JitterPct: 11}
	}
	deps.StartJobRunner = func(jobID string, run func()) {
		runnerJobID = jobID
	}
	deps.StartScheduledRunReconciliation = func(runID int64, jobID string) {
		reconciledRunID = runID
		reconciledJobID = jobID
	}

	newScheduledRunLifecycle(deps).Execute(run, policy, server)

	current := getScheduledLifecycleRun(t, deps, run.ID)
	if current.Status != updatePolicyRunRunning {
		t.Fatalf("run status = %q, want running", current.Status)
	}
	if strings.TrimSpace(current.JobID) == "" {
		t.Fatalf("run job_id is empty")
	}
	if runnerJobID != current.JobID {
		t.Fatalf("runner job_id = %q, want %q", runnerJobID, current.JobID)
	}
	if reconciledRunID != run.ID || reconciledJobID != current.JobID {
		t.Fatalf("reconciliation = run %d job %q, want run %d job %q", reconciledRunID, reconciledJobID, run.ID, current.JobID)
	}
	if got := atomic.LoadInt32(&retryLoads); got != 1 {
		t.Fatalf("retry loads = %d, want 1", got)
	}
	job, err := jm.GetJob(current.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q) unexpected error: %v", current.JobID, err)
	}
	if job.Kind != jobKindUpdate {
		t.Fatalf("job kind = %q, want %q", job.Kind, jobKindUpdate)
	}
	if !strings.Contains(job.RetryPolicyJSON, `"MaxAttempts":3`) {
		t.Fatalf("retry policy json = %s, want MaxAttempts 3", job.RetryPolicyJSON)
	}
	var meta scheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		t.Fatalf("unmarshal job meta: %v", err)
	}
	if meta.Trigger != "scheduled" || meta.PolicyID != policy.ID || meta.ScheduledFor != run.ScheduledForUTC {
		t.Fatalf("job meta = %+v, want scheduled policy/run metadata", meta)
	}
}

func TestScheduledRunLifecycleScanJobCreationSuccessLoadsRetryOnce(t *testing.T) {
	server := Server{Name: "srv-scan-created", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"scan"}}
	deps, policy, run, jm := newScheduledRunLifecycleTestDeps(t, "scheduled-run-scan-created.db", server, "idle")
	policy.ExecutionMode = updatePolicyExecutionScanOnly
	run.ExecutionMode = updatePolicyExecutionScanOnly

	var retryLoads int32
	var runnerJobID string
	deps.LoadRetryPolicy = func() RetryPolicy {
		atomic.AddInt32(&retryLoads, 1)
		return RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 3 * time.Second, JitterPct: 5}
	}
	deps.StartJobRunner = func(jobID string, run func()) {
		runnerJobID = jobID
	}

	newScheduledRunLifecycle(deps).Execute(run, policy, server)

	current := getScheduledLifecycleRun(t, deps, run.ID)
	if current.Status != updatePolicyRunRunning {
		t.Fatalf("run status = %q, want running", current.Status)
	}
	if runnerJobID != current.JobID {
		t.Fatalf("runner job_id = %q, want %q", runnerJobID, current.JobID)
	}
	if got := atomic.LoadInt32(&retryLoads); got != 1 {
		t.Fatalf("retry loads = %d, want 1", got)
	}
	job, err := jm.GetJob(current.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q) unexpected error: %v", current.JobID, err)
	}
	if job.Kind != jobKindScheduledScan {
		t.Fatalf("job kind = %q, want %q", job.Kind, jobKindScheduledScan)
	}
	if job.Summary != "Scheduled scan queued" {
		t.Fatalf("job summary = %q, want queued summary", job.Summary)
	}
}

func TestScheduledRunLifecycleJobCreationFailureRollsBackStatus(t *testing.T) {
	server := Server{Name: "srv-create-fails", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-create-fails.db", server, "idle")
	deps.CurrentJobManager = func() *JobManager { return nil }

	newScheduledRunLifecycle(deps).Execute(run, policy, server)

	current := getScheduledLifecycleRun(t, deps, run.ID)
	if current.Status != updatePolicyRunFailed || current.Reason != updatePolicyRunReasonPersistence {
		t.Fatalf("run = status %q reason %q, want failed/persistence", current.Status, current.Reason)
	}
	if current.Summary != "Failed to create scheduled update job" {
		t.Fatalf("summary = %q, want update creation failure", current.Summary)
	}
	status := deps.ServerState.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "idle" || status.Logs != "previous logs" {
		t.Fatalf("runtime status = %+v, want previous idle snapshot", status)
	}
}

func TestScheduledRunLifecycleScanOnlyRestoresRuntimeStatus(t *testing.T) {
	server := Server{Name: "srv-scan-restore", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"scan"}}
	deps, policy, run, jm := newScheduledRunLifecycleTestDeps(t, "scheduled-run-scan-restore.db", server, "idle")
	policy.ExecutionMode = updatePolicyExecutionScanOnly
	run.ExecutionMode = updatePolicyExecutionScanOnly
	pending := []PendingUpdate{{Package: "openssl", Security: true, CVEState: "ready", CVEs: []string{"CVE-2026-1001"}}}
	upgradable := []string{"openssl"}
	deps.UpdateService = NewUpdateService(UpdateServiceDeps{
		ServerState:       deps.ServerState,
		CurrentJobManager: func() *JobManager { return jm },
		HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				return HostCommandResult{Attempts: 1}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) updatespkg.PrecheckSummary {
				return updatespkg.PrecheckSummary{AllPassed: true}
			},
			DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{
					PendingPackageCount:  len(upgradable),
					SecurityPackageCount: 1,
					PendingUpdates:       clonePendingUpdates(pending),
					Upgradable:           append([]string(nil), upgradable...),
					UpgradePlan:          UpgradePlan{StandardSecurityCount: 1, TotalSecurityCount: 1},
				}, Attempts: 1}, nil
			},
			QueryPackageCVEsFunc: func(context.Context, string) ([]string, error) {
				return []string{"CVE-2026-1001"}, nil
			},
		}),
		UpdatePolicyRun: func(int64, updatePolicyRunUpdate) error {
			t.Fatalf("UpdatePolicyRun called from scheduled scan worker; reconciliation should update run state")
			return nil
		},
		AuditWithActor:  func(string, string, string, string, string, string, string, map[string]any) {},
		JobTimestampNow: deps.JobTimestampNow,
	})
	deps.StartJobRunner = func(_ string, run func()) {
		run()
	}
	deps.StartScheduledRunReconciliation = func(runID int64, jobID string) {
		job, err := jm.GetJob(jobID)
		if err != nil {
			t.Fatalf("GetJob(%q) during reconciliation unexpected error: %v", jobID, err)
		}
		newScheduledRunLifecycle(deps).updatePolicyRunFromJobRecord(runID, job)
	}

	newScheduledRunLifecycle(deps).Execute(run, policy, server)

	current := getScheduledLifecycleRun(t, deps, run.ID)
	if current.Status != updatePolicyRunSucceeded {
		t.Fatalf("run status = %q, want succeeded", current.Status)
	}
	if !strings.Contains(current.ResultJSON, "openssl") {
		t.Fatalf("result_json = %s, want openssl discovery", current.ResultJSON)
	}
	status := deps.ServerState.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "idle" || status.Logs != "previous logs" {
		t.Fatalf("runtime status = %+v, want restored previous idle snapshot", status)
	}
}

func TestScheduledRunLifecycleReconcileMapsJobStatusAndCopiesDiscovery(t *testing.T) {
	server := Server{Name: "srv-reconcile", Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
	deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-reconcile.db", server, "idle")
	lifecycle := newScheduledRunLifecycle(deps)

	tests := []struct {
		name       string
		jobStatus  string
		wantStatus string
		wantReason string
	}{
		{"queued", jobStatusQueued, updatePolicyRunQueued, ""},
		{"running", jobStatusRunning, updatePolicyRunRunning, ""},
		{"waiting approval", jobStatusWaitingApproval, updatePolicyRunWaitingApproval, ""},
		{"succeeded", jobStatusSucceeded, updatePolicyRunSucceeded, ""},
		{"failed", jobStatusFailed, updatePolicyRunFailed, updatePolicyRunFailed},
		{"cancelled", jobStatusCancelled, updatePolicyRunCancelled, updatePolicyRunCancelled},
		{"interrupted", jobStatusInterrupted, updatePolicyRunInterrupted, updatePolicyRunInterrupted},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := lifecycle.buildScheduledJobMeta(policy, run.ScheduledForUTC)
			if tt.jobStatus == jobStatusSucceeded {
				meta.Discovery = &scheduledJobDiscovery{
					PendingPackageCount:  1,
					SecurityPackageCount: 1,
					Upgradable:           []string{"openssl"},
					PendingUpdates:       []PendingUpdate{{Package: "openssl", Security: true}},
				}
			}
			lifecycle.updatePolicyRunFromJobRecord(run.ID, JobRecord{
				ID:         "job-" + strings.ReplaceAll(tt.name, " ", "-"),
				Status:     tt.jobStatus,
				Summary:    "job summary",
				StartedAt:  "2026-07-05T14:00:02Z",
				FinishedAt: "2026-07-05T14:00:03Z",
				MetaJSON:   marshalJobJSON(meta),
			})

			current := getScheduledLifecycleRun(t, deps, run.ID)
			if current.Status != tt.wantStatus {
				t.Fatalf("status = %q, want %q", current.Status, tt.wantStatus)
			}
			if current.Reason != tt.wantReason {
				t.Fatalf("reason = %q, want %q", current.Reason, tt.wantReason)
			}
			if tt.jobStatus == jobStatusSucceeded && !strings.Contains(current.ResultJSON, "openssl") {
				t.Fatalf("result_json = %s, want discovery copied from job meta", current.ResultJSON)
			}
		})
	}
}

func TestScheduledRunLifecycleReconcileScheduledScanTerminalAudits(t *testing.T) {
	tests := []struct {
		name        string
		jobStatus   string
		auditAction string
		auditStatus string
		errorText   string
	}{
		{name: "success", jobStatus: jobStatusSucceeded, auditAction: "schedule.run.completed", auditStatus: "success"},
		{name: "failure", jobStatus: jobStatusFailed, auditAction: "schedule.run.failed", auditStatus: "failure", errorText: "dial failed"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := Server{Name: "srv-scan-audit-" + tt.name, Host: "example.org", Port: 22, User: "root", Pass: "pw", Tags: []string{"prod"}}
			deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "scheduled-run-scan-audit-"+tt.name+".db", server, "idle")
			lifecycle := newScheduledRunLifecycle(deps)
			meta := lifecycle.buildScheduledJobMeta(policy, run.ScheduledForUTC)
			if tt.jobStatus == jobStatusSucceeded {
				meta.Discovery = &scheduledJobDiscovery{
					PendingPackageCount:  2,
					SecurityPackageCount: 1,
					Upgradable:           []string{"openssl", "curl"},
					PendingUpdates:       []PendingUpdate{{Package: "openssl", Security: true}},
				}
			}
			meta.Error = tt.errorText
			job := JobRecord{
				ID:         "job-scan-audit-" + tt.name,
				Kind:       jobKindScheduledScan,
				ServerName: server.Name,
				Status:     tt.jobStatus,
				Summary:    "job summary",
				StartedAt:  "2026-07-05T14:00:02Z",
				FinishedAt: "2026-07-05T14:00:03Z",
				MetaJSON:   marshalJobJSON(meta),
			}

			lifecycle.updatePolicyRunFromJobRecord(run.ID, job)

			audits, err := deps.AuditService.List(AuditListFilter{Action: tt.auditAction, TargetName: server.Name})
			if err != nil {
				t.Fatalf("List(%s) unexpected error: %v", tt.auditAction, err)
			}
			if audits.Total != 1 || audits.Items[0].Status != tt.auditStatus {
				t.Fatalf("audits = %+v, want one %s audit", audits.Items, tt.auditStatus)
			}
			if tt.jobStatus == jobStatusSucceeded && !strings.Contains(audits.Items[0].MetaJSON, `"pending_package_count":2`) {
				t.Fatalf("success audit meta = %s, want discovery counts", audits.Items[0].MetaJSON)
			}
			if tt.errorText != "" && !strings.Contains(audits.Items[0].MetaJSON, tt.errorText) {
				t.Fatalf("failure audit meta = %s, want error text %q", audits.Items[0].MetaJSON, tt.errorText)
			}

			lifecycle.updatePolicyRunFromJobRecord(run.ID, job)
			audits, err = deps.AuditService.List(AuditListFilter{Action: tt.auditAction, TargetName: server.Name})
			if err != nil {
				t.Fatalf("List(%s) after duplicate unexpected error: %v", tt.auditAction, err)
			}
			if audits.Total != 1 {
				t.Fatalf("audit total after duplicate reconcile = %d, want 1", audits.Total)
			}
		})
	}
}
