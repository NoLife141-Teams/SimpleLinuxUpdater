package main

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func testUpdateServiceDeps(t *testing.T) UpdateServiceDeps {
	t.Helper()
	return UpdateServiceDeps{
		BuildAuthMethods: func(Server) ([]ssh.AuthMethod, error) {
			return []ssh.AuthMethod{ssh.Password("secret")}, nil
		},
		HostKeyCallback: func() (ssh.HostKeyCallback, error) {
			return ssh.InsecureIgnoreHostKey(), nil
		},
		DialSSHWithRetry: func(Server, *ssh.ClientConfig, RetryPolicy, string, *int) (sshConnection, error) {
			return &fakeSSHConnection{}, nil
		},
		RunSSHOperationWithRetry: func(_ Server, _ *ssh.ClientConfig, _ *sshConnection, _ RetryPolicy, _ string, _ string, attempts *int, operation func() error) error {
			if attempts != nil {
				(*attempts)++
			}
			return operation()
		},
		RunSSHCommandWithTimeout: func(sshConnection, string, io.Reader, time.Duration) (string, string, error) {
			t.Fatalf("RunSSHCommandWithTimeout test hook must be overridden")
			return "", "", nil
		},
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
		RunUpdatePrechecks: func(sshConnection) updatePrecheckSummary {
			return updatePrecheckSummary{AllPassed: true, Results: []updatePrecheckResult{{Name: "apt", Passed: true, Details: "ok"}}}
		},
		RunPostUpdateHealthChecks: func(sshConnection, PostUpdateCheckConfig, map[string]struct{}) updatePostcheckSummary {
			return updatePostcheckSummary{AllPassed: true}
		},
		ListFailedSystemdUnits: func(sshConnection) ([]string, string, error) {
			return nil, "", nil
		},
		CollectServerFacts: func(server Server, sshConnection sshConnection, timeout time.Duration) serverFactsRecord {
			return serverFactsRecord{ServerName: server.Name}
		},
		SaveServerFacts: func(serverFactsRecord) error {
			return nil
		},
		DiscoverPackages: func(sshConnection, time.Duration) (PackageDiscoveryOutcome, error) {
			return PackageDiscoveryOutcome{}, nil
		},
		QueryPackageCVEs: func(sshConnection, string) ([]string, error) {
			return nil, nil
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		UpdatePolicyRun: func(int64, updatePolicyRunUpdate) error {
			return nil
		},
	}
}

func TestUpdateServiceSetupSSHUsesInjectedDependencies(t *testing.T) {
	server := Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}
	var builtAuth, builtHostKey, dialed bool
	deps := testUpdateServiceDeps(t)
	deps.BuildAuthMethods = func(got Server) ([]ssh.AuthMethod, error) {
		builtAuth = true
		if got.Name != server.Name {
			t.Fatalf("BuildAuthMethods server = %q, want %q", got.Name, server.Name)
		}
		return []ssh.AuthMethod{ssh.Password("secret")}, nil
	}
	deps.HostKeyCallback = func() (ssh.HostKeyCallback, error) {
		builtHostKey = true
		return ssh.InsecureIgnoreHostKey(), nil
	}
	deps.DialSSHWithRetry = func(got Server, config *ssh.ClientConfig, _ RetryPolicy, opName string, attempts *int) (sshConnection, error) {
		dialed = true
		if got.Name != server.Name {
			t.Fatalf("DialSSHWithRetry server = %q, want %q", got.Name, server.Name)
		}
		if config.User != server.User {
			t.Fatalf("ssh config user = %q, want %q", config.User, server.User)
		}
		if opName != "update.ssh_dial" {
			t.Fatalf("opName = %q, want update.ssh_dial", opName)
		}
		if attempts == nil {
			t.Fatalf("attempts pointer = nil")
		}
		*attempts = 1
		return &fakeSSHConnection{}, nil
	}

	runner := &withActorRunner{
		service: defaultUpdateService(),
		server:  server,
		policy:  loadRetryPolicyFromEnv(),
	}
	runner.service = NewUpdateService(deps)

	if !runner.setupSSH("update.ssh_dial") {
		t.Fatalf("setupSSH() = false, want true")
	}
	if !builtAuth || !builtHostKey || !dialed {
		t.Fatalf("setupSSH did not use all injected hooks: auth=%v hostkey=%v dial=%v", builtAuth, builtHostKey, dialed)
	}
	if runner.client == nil {
		t.Fatalf("runner.client = nil")
	}
	if runner.sshDialAttempts != 1 {
		t.Fatalf("sshDialAttempts = %d, want 1", runner.sshDialAttempts)
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
	deps.BuildAuthMethods = func(Server) ([]ssh.AuthMethod, error) {
		return nil, errors.New("missing credentials")
	}
	deps.DialSSHWithRetry = func(Server, *ssh.ClientConfig, RetryPolicy, string, *int) (sshConnection, error) {
		t.Fatalf("DialSSHWithRetry should not be called after auth setup failure")
		return nil, nil
	}
	runner := &withActorRunner{
		service: NewUpdateService(deps),
		server:  server,
		policy:  loadRetryPolicyFromEnv(),
	}

	if runner.setupSSH("update.ssh_dial") {
		t.Fatalf("setupSSH() = true, want false")
	}
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
	deps.RunSSHCommandWithTimeout = func(_ sshConnection, cmd string, _ io.Reader, _ time.Duration) (string, string, error) {
		command = cmd
		return "removed packages", "", nil
	}
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
	deps.RunSSHCommandWithTimeout = func(_ sshConnection, cmd string, _ io.Reader, _ time.Duration) (string, string, error) {
		if cmd != aptUpdateCmd {
			t.Fatalf("command = %q, want apt update command", cmd)
		}
		return "apt updated", "", nil
	}
	deps.DiscoverPackages = func(sshConnection, time.Duration) (PackageDiscoveryOutcome, error) {
		return PackageDiscoveryOutcome{
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
		}, nil
	}
	deps.QueryPackageCVEs = func(_ sshConnection, pkg string) ([]string, error) {
		if pkg != "openssl" {
			t.Fatalf("CVE package = %q, want openssl", pkg)
		}
		return []string{"CVE-2026-0001"}, nil
	}
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
