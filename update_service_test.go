package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testHostMaintenanceFactory(session *HostMaintenanceSessionFuncs) HostMaintenanceSessionFactory {
	return HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return session, nil
	})
}

func testUpdateServiceDeps(t *testing.T) UpdateServiceDeps {
	t.Helper()
	return UpdateServiceDeps{
		HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				t.Fatalf("RunCommand test hook must be overridden")
				return HostCommandResult{}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) updatePrecheckSummary {
				return updatePrecheckSummary{AllPassed: true, Results: []updatePrecheckResult{{Name: "apt", Passed: true, Details: "ok"}}}
			},
			RunPostUpdateHealthChecksFunc: func(context.Context, PostUpdateCheckConfig, map[string]struct{}) updatePostcheckSummary {
				return updatePostcheckSummary{AllPassed: true}
			},
			CollectServerFactsFunc: func(context.Context) serverFactsRecord {
				return serverFactsRecord{}
			},
		}),
		CurrentJobManager: func() *JobManager {
			return nil
		},
		AuditWithActor: func(string, string, string, string, string, string, string, map[string]any) {},
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		},
		JobTimestampNow: func() string {
			return "2026-01-02T03:04:05Z"
		},
		LoadCommandTimeout: func() time.Duration {
			return time.Second
		},
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig {
			return PostUpdateCheckConfig{}
		},
		LoadScheduledJobBehavior: func(string) scheduledJobBehavior {
			return scheduledJobBehavior{ApprovalTimeout: time.Minute}
		},
		SaveServerFacts: func(serverFactsRecord) error {
			return nil
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		UpdatePolicyRun: func(int64, updatePolicyRunUpdate) error {
			return nil
		},
	}
}

func TestUpdateServiceUsesInjectedHostMaintenanceSession(t *testing.T) {
	server := Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}
	state := newServerState()
	state.SetServers([]Server{server})
	state.SetStatusMap(map[string]*ServerStatus{server.Name: {Name: server.Name, Status: "idle"}})
	opened := false
	deps := testUpdateServiceDeps(t)
	deps.ServerState = state
	deps.HostMaintenanceSessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		opened = true
		if req.Server.Name != server.Name || req.DialOperation != "autoremove.ssh_dial" {
			t.Fatalf("session request = %+v", req)
		}
		return &HostMaintenanceSessionFuncs{RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
			return HostCommandResult{Attempts: 1}, nil
		}}, nil
	})
	NewUpdateService(deps).RunAutoremoveJob(AutoremoveRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})
	if !opened {
		t.Fatal("Host Maintenance Session was not opened")
	}
}

func TestUpdateServiceSetupSSHAuthFailureSetsRuntimeError(t *testing.T) {
	server := Server{Name: "srv-auth-fail", Host: "127.0.0.1", Port: 22, User: "root"}
	mu.Lock()
	oldStatusMap := statusMap
	statusMap = map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: "updating"},
	}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		statusMap = oldStatusMap
		mu.Unlock()
	})

	deps := testUpdateServiceDeps(t)
	deps.HostMaintenanceSessions = HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageAuth, Err: errors.New("missing credentials")}
	})
	NewUpdateService(deps).RunAutoremoveJob(AutoremoveRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})
	mu.Lock()
	gotStatus := statusMap[server.Name].Status
	gotLogs := statusMap[server.Name].Logs
	mu.Unlock()
	if gotStatus != "error" {
		t.Fatalf("status = %q, want error", gotStatus)
	}
	if !strings.Contains(gotLogs, "Auth setup failed: missing credentials") {
		t.Fatalf("logs = %q, want auth failure", gotLogs)
	}
}

func TestUpdateServiceAutoremoveUsesCommandHookAndAuditsSuccess(t *testing.T) {
	server := Server{Name: "srv-autoremove", Host: "127.0.0.1", Port: 22, User: "root"}
	mu.Lock()
	oldStatusMap := statusMap
	statusMap = map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: "idle", Logs: "Starting Linux Updater..."},
	}
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		statusMap = oldStatusMap
		mu.Unlock()
	})

	var command string
	var auditStatus string
	deps := testUpdateServiceDeps(t)
	deps.HostMaintenanceSessions = testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			command = req.Command
			return HostCommandResult{Stdout: "removed packages", Attempts: 1}, nil
		},
	})
	deps.AuditWithActor = func(_, _, action, _, _, status, _ string, _ map[string]any) {
		if action == "autoremove.complete" {
			auditStatus = status
		}
	}

	NewUpdateService(deps).RunAutoremoveJob(AutoremoveRunRequest{
		Server:   server,
		Actor:    "tester",
		ClientIP: "127.0.0.1",
		Policy:   loadRetryPolicyFromEnv(),
	})

	if command != aptAutoremoveCmd {
		t.Fatalf("command = %q, want %q", command, aptAutoremoveCmd)
	}
	mu.Lock()
	got := cloneServerStatus(statusMap[server.Name])
	mu.Unlock()
	if got.Status != "done" {
		t.Fatalf("status = %q, want done", got.Status)
	}
	if !strings.Contains(got.Logs, "Autoremove completed.") {
		t.Fatalf("logs = %q, want autoremove completion", got.Logs)
	}
	if auditStatus != "success" {
		t.Fatalf("audit status = %q, want success", auditStatus)
	}
}

func TestUpdateServiceScheduledScanIncludesCVEResults(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureJobSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	jm := newJobManager(db)
	policy := UpdatePolicy{
		ID:            7,
		Name:          "nightly",
		ExecutionMode: updatePolicyExecutionScanOnly,
		PackageScope:  updatePolicyPackageScopeSecurity,
	}
	scheduledForUTC := "2026-01-02T03:04:00Z"
	job, err := jm.CreateJob(JobCreateParams{
		Kind:       jobKindScheduledScan,
		ServerName: "srv-scan",
		Actor:      "system",
		Status:     jobStatusQueued,
		MetaJSON:   marshalJobJSON(buildScheduledJobMeta(policy, scheduledForUTC)),
	})
	if err != nil {
		t.Fatalf("create scheduled scan job: %v", err)
	}
	deps := testUpdateServiceDeps(t)
	deps.CurrentJobManager = func() *JobManager { return jm }
	deps.HostMaintenanceSessions = testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			if req.Command != aptUpdateCmd {
				t.Fatalf("command = %q, want apt update command", req.Command)
			}
			return HostCommandResult{Stdout: "apt updated", Attempts: 1}, nil
		},
		RunUpdatePrechecksFunc: func(context.Context) updatePrecheckSummary {
			return updatePrecheckSummary{AllPassed: true}
		},
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{
				PendingPackageCount:  1,
				SecurityPackageCount: 1,
				PendingUpdates: []PendingUpdate{{
					Package:          "openssl",
					CurrentVersion:   "1.0",
					CandidateVersion: "1.1",
					Security:         true,
					CVEState:         "pending",
					Raw:              "openssl/now 1.1",
				}},
				Upgradable:  []string{"openssl/now 1.1"},
				UpgradePlan: UpgradePlan{StandardPackageCount: 1, StandardSecurityCount: 1, TotalSecurityCount: 1, FullUpgradePackageCount: 1},
			}, Attempts: 1}, nil
		},
		QueryPackageCVEsFunc: func(_ context.Context, pkg string) ([]string, error) {
			if pkg != "openssl" {
				t.Fatalf("CVE package = %q, want openssl", pkg)
			}
			return []string{"CVE-2026-0001"}, nil
		},
	})
	deps.UpdatePolicyRun = func(_ int64, update updatePolicyRunUpdate) error {
		t.Fatalf("UpdatePolicyRun called from scheduled scan worker: %+v", update)
		return nil
	}
	deps.AuditWithActor = func(_, _, action, _, _, _, _ string, _ map[string]any) {
		if strings.HasPrefix(action, "schedule.run.") {
			t.Fatalf("scheduled scan worker emitted %q; reconciliation should own scheduled run audit", action)
		}
	}

	NewUpdateService(deps).RunScheduledScanJob(ScheduledScanRunRequest{
		JobID:           job.ID,
		RunID:           42,
		ScheduledForUTC: scheduledForUTC,
		Server:          Server{Name: "srv-scan", Host: "127.0.0.1", Port: 22, User: "root"},
		Policy:          policy,
		RetryPolicy:     loadRetryPolicyFromEnv(),
	})

	job, err = jm.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", job.ID, err)
	}
	if job.Status != jobStatusSucceeded {
		t.Fatalf("job status = %q, want %q", job.Status, jobStatusSucceeded)
	}
	var meta scheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		t.Fatalf("job meta JSON unmarshal error = %v", err)
	}
	if meta.Discovery == nil || len(meta.Discovery.PendingUpdates) != 1 || !strings.Contains(strings.Join(meta.Discovery.PendingUpdates[0].CVEs, ","), "CVE-2026-0001") {
		t.Fatalf("job discovery = %+v, want CVE result", meta.Discovery)
	}
}
