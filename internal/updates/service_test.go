package updates

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"

	_ "modernc.org/sqlite"
)

const liveOutputTestTimeout = 5 * time.Second

func testHostMaintenanceSessionFactory(session *HostMaintenanceSessionFuncs) HostMaintenanceSessionFactory {
	return HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return session, nil
	})
}

type fakeSession struct{}

func (fakeSession) SetStdin(io.Reader)  {}
func (fakeSession) SetStdout(io.Writer) {}
func (fakeSession) SetStderr(io.Writer) {}
func (fakeSession) Run(string) error    { return nil }
func (fakeSession) Close() error        { return nil }

type fakeConnection struct{}

func (fakeConnection) NewSession() (SSHSessionRunner, error) { return fakeSession{}, nil }
func (fakeConnection) Close() error                          { return nil }

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestApplyPostcheckPolicyOwnsTerminalClassification(t *testing.T) {
	results := []PrecheckResult{
		{Name: PostcheckNameFailedUnits, Details: "new failure"},
		{Name: PostcheckNameRebootNeeded, Details: "restart required"},
	}
	cfg := PostUpdateCheckConfig{BlockOnFailedUnits: true, RebootRequiredWarning: true}
	summary := applyPostcheckPolicy(results, cfg, func(name string, _ PostUpdateCheckConfig) bool {
		return name == PostcheckNameFailedUnits
	})
	if summary.AllPassed || summary.FailedCheck != PostcheckNameFailedUnits || summary.Warnings != 1 {
		t.Fatalf("applyPostcheckPolicy() = %+v", summary)
	}
	if !reflect.DeepEqual(summary.Results, results) {
		t.Fatalf("Results = %+v, want %+v", summary.Results, results)
	}
}

func testState() (*servers.State, map[string]*servers.ServerStatus) {
	mu := &sync.Mutex{}
	inventory := []servers.Server{{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}}
	statuses := map[string]*servers.ServerStatus{
		"srv": {Name: "srv", Status: "pending_approval", PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true}}},
	}
	return servers.NewState(mu, &inventory, &statuses, nil), statuses
}

func TestServiceApproveCancelUsesInjectedServerState(t *testing.T) {
	state, statuses := testState()
	service := NewService(ServiceDeps{ServerState: state})

	exists, approved := service.ApprovePendingUpdate("srv", "security")
	if !exists || !approved || statuses["srv"].Status != "approved" || statuses["srv"].ApprovalScope != "security" {
		t.Fatalf("ApprovePendingUpdate() exists=%t approved=%t status=%+v", exists, approved, statuses["srv"])
	}

	statuses["srv"].Status = "pending_approval"
	statuses["srv"].Logs = "pending"
	exists, cancelled := service.CancelPendingUpdate("srv")
	if !exists || !cancelled || statuses["srv"].Status != "cancelled" || statuses["srv"].Logs != "" || len(statuses["srv"].PendingUpdates) != 0 {
		t.Fatalf("CancelPendingUpdate() exists=%t cancelled=%t status=%+v", exists, cancelled, statuses["srv"])
	}
}

func TestRunRebootJobVerifiesSSHRecoveryAndUptimeReset(t *testing.T) {
	server := servers.Server{Name: "srv-reboot", Host: "example.org", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "done"}}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	rebootRequired := false
	opens := 0
	commandSent := false
	saved := false
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			opens++
			if opens == 1 {
				return &HostMaintenanceSessionFuncs{
					CollectServerFactsFunc: func(context.Context) ServerFactsRecord {
						return ServerFactsRecord{ServerName: server.Name, UptimeSeconds: 7200, RunningKernelVersion: "6.12.1"}
					},
					RunCommandFunc: func(_ context.Context, command HostCommandRequest) (HostCommandResult, error) {
						if command.Command != ControlledRebootCmd || command.ReplayPolicy != ReplayNever {
							t.Fatalf("reboot command = %+v", command)
						}
						commandSent = true
						return HostCommandResult{Attempts: 1}, nil
					},
				}, nil
			}
			if req.RetryPolicy.MaxAttempts != 1 || req.DialOperation != "reboot.verify.ssh_dial" {
				t.Fatalf("verification request = %+v", req)
			}
			return &HostMaintenanceSessionFuncs{CollectServerFactsFunc: func(context.Context) ServerFactsRecord {
				return ServerFactsRecord{ServerName: server.Name, UptimeSeconds: 15, RunningKernelVersion: "6.12.2", RebootRequired: &rebootRequired}
			}}, nil
		}),
		CurrentJobManager: func() *jobs.Manager { return nil },
		AuditWithActor:    func(string, string, string, string, string, string, string, map[string]any) {},
		Sleep:             func(time.Duration) {},
		SaveServerFacts: func(facts ServerFactsRecord) error {
			saved = facts.UptimeSeconds == 15
			return nil
		},
	})
	service.RunRebootJob(RebootRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 2 * time.Second}})

	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "done" || !strings.Contains(status.Logs, "Reboot verified") {
		t.Fatalf("final reboot status = %+v", status)
	}
	if opens != 2 || !commandSent || !saved {
		t.Fatalf("opens=%d commandSent=%t saved=%t, want 2/true/true", opens, commandSent, saved)
	}
}

func TestRunRebootJobDoesNotRebootWithoutUptimeBaseline(t *testing.T) {
	server := servers.Server{Name: "srv-no-baseline", Host: "example.org", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "done"}}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	commandSent := false
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
			CollectServerFactsFunc: func(context.Context) ServerFactsRecord { return ServerFactsRecord{} },
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				commandSent = true
				return HostCommandResult{}, nil
			},
		}),
		CurrentJobManager: func() *jobs.Manager { return nil },
		AuditWithActor:    func(string, string, string, string, string, string, string, map[string]any) {},
		Sleep:             func(time.Duration) {},
	})
	service.RunRebootJob(RebootRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})

	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "error" || !strings.Contains(status.Logs, "reboot was not sent") || commandSent {
		t.Fatalf("final reboot status = %+v commandSent=%t", status, commandSent)
	}
}

func TestUpdatePendingPackageVulnerabilityAssessmentPreservesMultiarchSelectors(t *testing.T) {
	state, statuses := testState()
	statuses["srv"].PendingUpdates = []servers.PendingUpdate{
		{Package: "openssl", InstallPackage: "openssl:amd64", CVEState: "pending"},
		{Package: "openssl", InstallPackage: "openssl:i386", CVEState: "pending"},
	}
	service := NewService(ServiceDeps{ServerState: state})

	for _, update := range []servers.PendingUpdate{
		{
			Package:        "openssl",
			InstallPackage: "openssl:amd64",
			CVEState:       "ready",
			CVEFindings:    []servers.VulnerabilityFinding{{ID: "CVE-2026-1001"}},
		},
		{
			Package:        "openssl",
			InstallPackage: "openssl:i386",
			CVEState:       "ready",
			CVEFindings:    []servers.VulnerabilityFinding{{ID: "CVE-2026-1002"}},
		},
	} {
		if !service.updatePendingPackageVulnerabilityAssessment("srv", update) {
			t.Fatalf("updatePendingPackageVulnerabilityAssessment(%q) = false", update.InstallPackage)
		}
	}

	got := map[string]string{}
	for _, update := range statuses["srv"].PendingUpdates {
		if len(update.CVEFindings) != 1 {
			t.Fatalf("pending update = %+v, want one finding", update)
		}
		got[update.InstallPackage] = update.CVEFindings[0].ID
	}
	want := map[string]string{
		"openssl:amd64": "CVE-2026-1001",
		"openssl:i386":  "CVE-2026-1002",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("multiarch findings = %#v, want %#v", got, want)
	}
}

func TestHostMaintenanceSessionFuncsDefaultQueryPackageCVEsIsSafeNoop(t *testing.T) {
	session := &HostMaintenanceSessionFuncs{}
	cves, err := session.QueryPackageCVEs(context.Background(), "openssl")
	if err != nil {
		t.Fatalf("default QueryPackageCVEs() error = %v", err)
	}
	if len(cves) != 0 {
		t.Fatalf("default QueryPackageCVEs() = %#v, want empty CVE list", cves)
	}
}

func TestRunUpdateJobApprovalScopesUseExpectedAptCommand(t *testing.T) {
	tests := []struct {
		name       string
		scope      string
		pending    []servers.PendingUpdate
		upgradable []string
		plan       servers.UpgradePlan
		wantCmd    string
		manual     bool
	}{
		{
			name:       "standard approval",
			scope:      "all",
			pending:    []servers.PendingUpdate{{Package: "openssl", Raw: "Inst openssl"}},
			upgradable: []string{"openssl"},
			plan:       servers.UpgradePlan{StandardPackageCount: 1, FullUpgradePackageCount: 1},
			wantCmd:    AptUpgradeCmd,
		},
		{
			name:  "full approval",
			scope: "full_upgrade",
			pending: []servers.PendingUpdate{
				{Package: "openssl", Raw: "Inst openssl"},
				{Package: "linux-image-amd64", KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			upgradable: []string{"openssl", "linux-image-amd64"},
			plan: servers.UpgradePlan{
				FullUpgradePlanAvailable: true,
				StandardPackageCount:     1,
				KeptBackPackageCount:     1,
				FullUpgradePackageCount:  2,
				FullUpgradeNewPackages:   []string{"linux-image-6.1.0-39-amd64"},
			},
			wantCmd: AptFullUpgradeCmd,
		},
		{
			name:  "standard security approval",
			scope: "security",
			pending: []servers.PendingUpdate{
				{Package: "openssl", Security: true, Raw: "Inst openssl"},
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			upgradable: []string{"openssl", "linux-image-amd64"},
			plan: servers.UpgradePlan{
				StandardPackageCount:    1,
				KeptBackPackageCount:    1,
				StandardSecurityCount:   1,
				TotalSecurityCount:      2,
				FullUpgradePackageCount: 2,
			},
			wantCmd: BuildSelectedUpgradeCmd([]string{"openssl"}),
		},
		{
			name:  "kept-back security approval",
			scope: "security_kept_back",
			pending: []servers.PendingUpdate{
				{Package: "openssl", Security: true, Raw: "Inst openssl"},
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			upgradable: []string{"openssl", "linux-image-amd64"},
			plan: servers.UpgradePlan{
				StandardPackageCount:          1,
				KeptBackPackageCount:          1,
				StandardSecurityCount:         1,
				TotalSecurityCount:            2,
				FullUpgradePackageCount:       2,
				FullUpgradeNewPackages:        []string{"linux-image-6.1.0-39-amd64"},
				KeptBackSecurityPlanAvailable: true,
				KeptBackSecurityPackageCount:  1,
				KeptBackSecurityNewPackages:   []string{"linux-image-6.1.0-39-amd64"},
			},
			wantCmd: BuildSelectedInstallCmd([]string{"linux-image-amd64"}),
			manual:  true,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mu := &sync.Mutex{}
			server := servers.Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{
				server.Name: {Name: server.Name, Status: "idle"},
			}
			var commands []string
			deps := ServiceDeps{
				ServerState: servers.NewState(mu, &inventory, &statuses, nil),
				HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
					RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
						commands = append(commands, req.Command)
						return HostCommandResult{Attempts: 1}, nil
					},
					RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
						return PrecheckSummary{AllPassed: true}
					},
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						return HostPackageDiscoveryResult{Outcome: newPackageDiscoveryOutcome(tc.pending, tc.upgradable, tc.plan), Attempts: 1}, nil
					},
				}),
				CurrentJobManager: func() *jobs.Manager {
					return nil
				},
				LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig {
					return PostUpdateCheckConfig{Enabled: false}
				},
				LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
					autoApproveScope := tc.scope
					if tc.manual {
						autoApproveScope = ""
					}
					return ScheduledJobBehavior{ApprovalTimeout: 2 * time.Second, AutoApproveScope: autoApproveScope}
				},
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				SaveServerFacts: func(ServerFactsRecord) error {
					return nil
				},
				AuditWithActor: func(_, _, _, _, _, _, _ string, _ map[string]any) {},
			}

			service := NewService(deps)
			if tc.manual {
				go func() {
					deadline := time.Now().Add(time.Second)
					for time.Now().Before(deadline) {
						status := deps.ServerState.CurrentStatusSnapshot(server.Name)
						if status != nil && status.Status == "pending_approval" {
							service.ApprovePendingUpdate(server.Name, tc.scope)
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}

			service.RunUpdateJob(UpdateRunRequest{
				Server: server,
				Actor:  "tester",
				Policy: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			})

			status := deps.ServerState.CurrentStatusSnapshot(server.Name)
			if status == nil || status.Status != "done" {
				t.Fatalf("final status = %+v, want done", status)
			}
			if !containsString(commands, tc.wantCmd) {
				t.Fatalf("commands = %#v, want %q", commands, tc.wantCmd)
			}
		})
	}
}

func TestRunUpdateJobAbortsBeforeAptMutationWhenPhasePersistenceFails(t *testing.T) {
	server := servers.Server{Name: "srv-phase-failure", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "phase-failure-jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	jobID := "phase-failure-job"
	jm := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{NewID: func() string { return jobID }})
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindUpdate,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
		Phase:      jobs.PhaseDial,
	}); err != nil {
		t.Fatalf("create update job: %v", err)
	}
	if _, err := db.Exec(`
		CREATE TRIGGER fail_apt_upgrade_phase
		BEFORE UPDATE ON jobs
		WHEN NEW.phase = 'apt_upgrade'
		BEGIN
			SELECT RAISE(FAIL, 'phase persistence unavailable');
		END
	`); err != nil {
		t.Fatalf("create phase failure trigger: %v", err)
	}

	var commands []string
	var auditStatus, auditErrorClass string
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				commands = append(commands, req.Command)
				return HostCommandResult{Attempts: 1}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
				return PrecheckSummary{AllPassed: true}
			},
			DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				return HostPackageDiscoveryResult{
					Outcome: newPackageDiscoveryOutcome(
						[]servers.PendingUpdate{{Package: "openssl", Raw: "Inst openssl"}},
						[]string{"openssl"},
						servers.UpgradePlan{StandardPackageCount: 1, FullUpgradePackageCount: 1},
					),
					Attempts: 1,
				}, nil
			},
		}),
		CurrentJobManager:         func() *jobs.Manager { return jm },
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
		LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
			return ScheduledJobBehavior{ApprovalTimeout: time.Second, AutoApproveScope: ApprovalScopeAll}
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		SaveServerFacts:              func(ServerFactsRecord) error { return nil },
		AuditWithActor: func(_, _, _, _, _, status, _ string, meta map[string]any) {
			auditStatus = status
			auditErrorClass, _ = meta["last_error_class"].(string)
		},
	})

	service.RunUpdateJob(UpdateRunRequest{
		Server: server,
		Actor:  "tester",
		Policy: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		JobID:  jobID,
	})

	if containsString(commands, AptUpgradeCmd) {
		t.Fatalf("commands = %#v, APT upgrade must not run without a durable mutation phase", commands)
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "error" || !strings.Contains(status.Logs, "command aborted before execution") {
		t.Fatalf("runtime status = %+v, want persistence failure before mutation", status)
	}
	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get update job: %v", err)
	}
	if job.Status != jobs.StatusFailed || job.Phase != jobs.PhaseComplete || job.ErrorClass != "persistence" {
		t.Fatalf("job after phase failure = %+v, want terminal persistence failure", job)
	}
	if auditStatus != "failure" || auditErrorClass != "persistence" {
		t.Fatalf("audit = status %q error class %q, want failure/persistence", auditStatus, auditErrorClass)
	}
}

func TestRunUpdateJobPublishesAptUpgradeOutputBeforeCommandCompletes(t *testing.T) {
	server := servers.Server{Name: "srv-live-output", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "live-output-jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	notifications := make(chan string, 64)
	jobID := "live-output-job"
	jm := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		NewID: func() string { return jobID },
		Notify: func(reason string) {
			notifications <- reason
		},
	})
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindUpdate,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create update job: %v", err)
	}
	<-notifications
	outputSent := make(chan struct{})
	releaseUpgrade := make(chan struct{})
	runDone := make(chan struct{})
	const liveLine = "Unpacking openssl (3.0.0)\n"

	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Operation != "update.apt_upgrade" {
					return HostCommandResult{Attempts: 1}, nil
				}
				if req.OnOutput == nil {
					return HostCommandResult{}, errors.New("apt upgrade output callback is missing")
				}
				if req.OnAttemptComplete == nil {
					return HostCommandResult{}, errors.New("apt upgrade attempt completion callback is missing")
				}
			drainNotifications:
				for {
					select {
					case <-notifications:
					default:
						break drainNotifications
					}
				}
				req.OnOutput(HostCommandOutput{Stream: HostCommandStdout, Data: liveLine})
				close(outputSent)
				<-releaseUpgrade
				req.OnAttemptComplete()
				return HostCommandResult{Stdout: liveLine, Attempts: 1}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
				return PrecheckSummary{AllPassed: true}
			},
			DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				return HostPackageDiscoveryResult{
					Outcome: newPackageDiscoveryOutcome(
						[]servers.PendingUpdate{{Package: "openssl", Raw: "Inst openssl"}},
						[]string{"openssl"},
						servers.UpgradePlan{StandardPackageCount: 1, FullUpgradePackageCount: 1},
					),
					Attempts: 1,
				}, nil
			},
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig {
			return PostUpdateCheckConfig{Enabled: false}
		},
		LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
			return ScheduledJobBehavior{ApprovalTimeout: time.Minute, AutoApproveScope: ApprovalScopeAll}
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		SaveServerFacts:              func(ServerFactsRecord) error { return nil },
		AuditWithActor:               func(_, _, _, _, _, _, _ string, _ map[string]any) {},
	})

	go func() {
		defer close(runDone)
		service.RunUpdateJob(UpdateRunRequest{
			Server: server,
			Actor:  "tester",
			JobID:  jobID,
			Policy: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		})
	}()

	select {
	case <-outputSent:
	case <-time.After(liveOutputTestTimeout):
		t.Fatal("apt upgrade did not emit output")
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || !strings.Contains(status.Logs, liveLine) {
		t.Fatalf("logs before command completion = %q, want live line %q", status.Logs, liveLine)
	}
	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get live update job: %v", err)
	}
	if !strings.Contains(job.LogsText, liveLine) {
		t.Fatalf("persisted logs before command completion = %q, want live line %q", job.LogsText, liveLine)
	}
	select {
	case reason := <-notifications:
		if reason != "job.log" {
			t.Fatalf("live output notification = %q, want job.log", reason)
		}
	default:
		t.Fatal("live output did not publish a dashboard job update")
	}
	select {
	case <-runDone:
		t.Fatal("update completed before the apt upgrade command was released")
	default:
	}

	close(releaseUpgrade)
	select {
	case <-runDone:
	case <-time.After(liveOutputTestTimeout):
		t.Fatal("update did not finish after releasing apt upgrade")
	}
	status = state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "done" {
		t.Fatalf("final status = %+v, want done", status)
	}
	if got := strings.Count(status.Logs, liveLine); got != 1 {
		t.Fatalf("final live line count = %d, want 1 in logs %q", got, status.Logs)
	}
	job, err = jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get completed update job: %v", err)
	}
	if job.Status != jobs.StatusSucceeded || strings.Count(job.LogsText, liveLine) != 1 {
		t.Fatalf("completed job = %+v, want succeeded with one live line", job)
	}
}

func TestRunAutoremoveJobPublishesOutputBeforeCommandCompletes(t *testing.T) {
	server := servers.Server{Name: "srv-live-autoremove", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "live-autoremove-jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	notifications := make(chan string, 64)
	jobID := "live-autoremove-job"
	jm := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		NewID: func() string { return jobID },
		Notify: func(reason string) {
			notifications <- reason
		},
	})
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindAutoremove,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create autoremove job: %v", err)
	}
	<-notifications
	outputSent := make(chan struct{})
	releaseAutoremove := make(chan struct{})
	runDone := make(chan struct{})
	const liveLine = "Removing linux-image-old (1.0)\n"

	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Operation != "autoremove.command" {
					return HostCommandResult{}, errors.New("unexpected autoremove operation")
				}
				if req.OnOutput == nil {
					return HostCommandResult{}, errors.New("autoremove output callback is missing")
				}
				if req.OnAttemptComplete == nil {
					return HostCommandResult{}, errors.New("autoremove attempt completion callback is missing")
				}
			drainNotifications:
				for {
					select {
					case <-notifications:
					default:
						break drainNotifications
					}
				}
				req.OnOutput(HostCommandOutput{Stream: HostCommandStdout, Data: liveLine})
				close(outputSent)
				<-releaseAutoremove
				req.OnAttemptComplete()
				return HostCommandResult{Stdout: liveLine, Attempts: 1}, nil
			},
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		AuditWithActor:    func(_, _, _, _, _, _, _ string, _ map[string]any) {},
	})

	go func() {
		defer close(runDone)
		service.RunAutoremoveJob(AutoremoveRunRequest{
			Server: server,
			Actor:  "tester",
			JobID:  jobID,
			Policy: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		})
	}()

	select {
	case <-outputSent:
	case <-time.After(liveOutputTestTimeout):
		t.Fatal("apt autoremove did not emit output")
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || !strings.Contains(status.Logs, liveLine) {
		t.Fatalf("logs before command completion = %q, want live line %q", status.Logs, liveLine)
	}
	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get live autoremove job: %v", err)
	}
	if !strings.Contains(job.LogsText, liveLine) {
		t.Fatalf("persisted logs before command completion = %q, want live line %q", job.LogsText, liveLine)
	}
	select {
	case reason := <-notifications:
		if reason != "job.log" {
			t.Fatalf("live output notification = %q, want job.log", reason)
		}
	default:
		t.Fatal("live autoremove output did not publish a dashboard job update")
	}
	select {
	case <-runDone:
		t.Fatal("autoremove completed before the command was released")
	default:
	}

	close(releaseAutoremove)
	select {
	case <-runDone:
	case <-time.After(liveOutputTestTimeout):
		t.Fatal("autoremove did not finish after releasing the command")
	}
	status = state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "done" {
		t.Fatalf("final status = %+v, want done", status)
	}
	if got := strings.Count(status.Logs, liveLine); got != 1 {
		t.Fatalf("final live line count = %d, want 1 in logs %q", got, status.Logs)
	}
	job, err = jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get completed autoremove job: %v", err)
	}
	if job.Status != jobs.StatusSucceeded || strings.Count(job.LogsText, liveLine) != 1 {
		t.Fatalf("completed job = %+v, want succeeded with one live line", job)
	}
}

func TestLiveCommandLogSinkBatchesRapidOutput(t *testing.T) {
	server := servers.Server{Name: "srv-live-batch"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "upgrading", Logs: "Running apt upgrade..."},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	now := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	service := NewService(ServiceDeps{
		ServerState:       state,
		CurrentJobManager: func() *jobs.Manager { return nil },
		Now:               func() time.Time { return now },
	})
	sink := newLiveCommandLogSink(&withActorRunner{service: service, server: server})

	sink.Handle(HostCommandOutput{Stream: HostCommandStdout, Data: "first\n"})
	if got := state.CurrentStatusLogs(server.Name); !strings.HasSuffix(got, "\nfirst\n") {
		t.Fatalf("first output was not flushed immediately: %q", got)
	}

	sink.Handle(HostCommandOutput{Stream: HostCommandStdout, Data: "second\n"})
	if got := state.CurrentStatusLogs(server.Name); strings.Contains(got, "second") {
		t.Fatalf("rapid output was not batched: %q", got)
	}

	now = now.Add(liveCommandLogFlushInterval)
	sink.Handle(HostCommandOutput{Stream: HostCommandStderr, Data: "third\n"})
	if got := state.CurrentStatusLogs(server.Name); !strings.HasSuffix(got, "second\nthird\n") {
		t.Fatalf("batched output was not flushed after interval: %q", got)
	}
}

func TestLiveCommandLogSinkFlushesTrailingOutputWithoutAnotherChunk(t *testing.T) {
	server := servers.Server{Name: "srv-live-trailing"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "upgrading", Logs: "Running apt upgrade..."},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	service := NewService(ServiceDeps{
		ServerState:       state,
		CurrentJobManager: func() *jobs.Manager { return nil },
	})
	sink := newLiveCommandLogSink(&withActorRunner{service: service, server: server})
	sink.Handle(HostCommandOutput{Stream: HostCommandStdout, Data: "first\n"})
	sink.Handle(HostCommandOutput{Stream: HostCommandStdout, Data: "trailing\n"})

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(state.CurrentStatusLogs(server.Name), "trailing\n") {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("trailing output was not flushed after %s: %q", liveCommandLogFlushInterval, state.CurrentStatusLogs(server.Name))
}

func TestRunUpdateJobGuardsRemovalApprovalsInRunner(t *testing.T) {
	selectedKeptBackCmd := BuildSelectedInstallCmd([]string{"linux-image-amd64"})
	tests := []struct {
		name       string
		scope      string
		pending    []servers.PendingUpdate
		plan       servers.UpgradePlan
		confirm    bool
		wantStatus string
		wantCmd    string
	}{
		{
			name:  "full upgrade removals require recorded confirmation",
			scope: "full_upgrade",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				FullUpgradePlanAvailable:   true,
				KeptBackPackageCount:       1,
				FullUpgradePackageCount:    1,
				FullUpgradeRemovedPackages: []string{"obsolete-kernel"},
			},
			wantStatus: "error",
			wantCmd:    "",
		},
		{
			name:  "full upgrade requires successful simulation",
			scope: "full_upgrade",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				KeptBackPackageCount:    1,
				FullUpgradePackageCount: 1,
			},
			confirm:    true,
			wantStatus: "error",
			wantCmd:    "",
		},
		{
			name:  "full upgrade removals run after recorded confirmation",
			scope: "full_upgrade",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				FullUpgradePlanAvailable:   true,
				KeptBackPackageCount:       1,
				FullUpgradePackageCount:    1,
				FullUpgradeRemovedPackages: []string{"obsolete-kernel"},
			},
			confirm:    true,
			wantStatus: "done",
			wantCmd:    AptFullUpgradeCmd,
		},
		{
			name:  "kept-back security requires targeted simulation",
			scope: "security_kept_back",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				KeptBackPackageCount:         1,
				TotalSecurityCount:           1,
				FullUpgradePackageCount:      1,
				KeptBackSecurityPackageCount: 1,
			},
			wantStatus: "error",
			wantCmd:    "",
		},
		{
			name:  "kept-back security removals require recorded confirmation",
			scope: "security_kept_back",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				KeptBackPackageCount:            1,
				TotalSecurityCount:              1,
				FullUpgradePackageCount:         1,
				KeptBackSecurityPlanAvailable:   true,
				KeptBackSecurityPackageCount:    1,
				KeptBackSecurityRemovedPackages: []string{"obsolete-kernel"},
			},
			wantStatus: "error",
			wantCmd:    "",
		},
		{
			name:  "kept-back security removals run after recorded confirmation",
			scope: "security_kept_back",
			pending: []servers.PendingUpdate{
				{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
			},
			plan: servers.UpgradePlan{
				KeptBackPackageCount:            1,
				TotalSecurityCount:              1,
				FullUpgradePackageCount:         1,
				KeptBackSecurityPlanAvailable:   true,
				KeptBackSecurityPackageCount:    1,
				KeptBackSecurityRemovedPackages: []string{"obsolete-kernel"},
			},
			confirm:    true,
			wantStatus: "done",
			wantCmd:    selectedKeptBackCmd,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			mu := &sync.Mutex{}
			server := servers.Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{
				server.Name: {Name: server.Name, Status: "idle"},
			}
			var commands []string
			deps := ServiceDeps{
				ServerState: servers.NewState(mu, &inventory, &statuses, nil),
				HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
					RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
						commands = append(commands, req.Command)
						return HostCommandResult{Attempts: 1}, nil
					},
					RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
						return PrecheckSummary{AllPassed: true}
					},
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						return HostPackageDiscoveryResult{Outcome: newPackageDiscoveryOutcome(tc.pending, []string{"linux-image-amd64"}, tc.plan), Attempts: 1}, nil
					},
				}),
				CurrentJobManager: func() *jobs.Manager {
					return nil
				},
				LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig {
					return PostUpdateCheckConfig{Enabled: false}
				},
				LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
					return ScheduledJobBehavior{ApprovalTimeout: 2 * time.Second}
				},
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				SaveServerFacts: func(ServerFactsRecord) error {
					return nil
				},
				AuditWithActor: func(_, _, _, _, _, _, _ string, _ map[string]any) {},
			}

			service := NewService(deps)
			go func() {
				deadline := time.Now().Add(time.Second)
				for time.Now().Before(deadline) {
					status := deps.ServerState.CurrentStatusSnapshot(server.Name)
					if status != nil && status.Status == "pending_approval" {
						if tc.confirm {
							service.ApprovePendingUpdateWithOptions(server.Name, tc.scope, servers.ApprovalOptions{ConfirmRemovals: true})
						} else {
							service.ApprovePendingUpdate(server.Name, tc.scope)
						}
						return
					}
					time.Sleep(10 * time.Millisecond)
				}
			}()

			service.RunUpdateJob(UpdateRunRequest{
				Server: server,
				Actor:  "tester",
				Policy: RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
			})

			status := deps.ServerState.CurrentStatusSnapshot(server.Name)
			if status == nil || status.Status != tc.wantStatus {
				t.Fatalf("final status = %+v, want %s", status, tc.wantStatus)
			}
			if tc.wantCmd != "" {
				if !containsString(commands, tc.wantCmd) {
					t.Fatalf("commands = %#v, want %q", commands, tc.wantCmd)
				}
				return
			}
			if containsString(commands, AptFullUpgradeCmd) || containsString(commands, selectedKeptBackCmd) {
				t.Fatalf("commands = %#v, should not run removal-risk upgrade command", commands)
			}
		})
	}
}

func TestRunUpdateJobReleasesHostSessionDuringManualApproval(t *testing.T) {
	tests := []struct {
		name      string
		resolve   func(*Service, string)
		wantOpens int
		wantFinal string
	}{
		{
			name: "approval reopens a fresh session",
			resolve: func(service *Service, serverName string) {
				service.ApprovePendingUpdate(serverName, ApprovalScopeAll)
			},
			wantOpens: 2,
			wantFinal: "done",
		},
		{
			name: "cancellation does not reopen",
			resolve: func(service *Service, serverName string) {
				service.CancelPendingUpdate(serverName)
			},
			wantOpens: 1,
			wantFinal: "idle",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := servers.Server{Name: "srv", User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "idle"}}
			state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
			opens := 0
			closes := []int{0, 0}
			var service *Service
			factory := HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
				index := opens
				opens++
				return &HostMaintenanceSessionFuncs{
					RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
						return HostCommandResult{Stdout: req.Operation, Attempts: 1}, nil
					},
					RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
						return PrecheckSummary{AllPassed: true}
					},
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						outcome := newPackageDiscoveryOutcome(
							[]servers.PendingUpdate{{Package: "openssl", CVEState: "ready"}},
							[]string{"openssl"},
							servers.UpgradePlan{StandardPackageCount: 1},
						)
						return HostPackageDiscoveryResult{Outcome: outcome, Attempts: 1}, nil
					},
					CollectServerFactsFunc: func(context.Context) ServerFactsRecord {
						return ServerFactsRecord{ServerName: server.Name}
					},
					CloseFunc: func() error {
						closes[index]++
						return nil
					},
				}, nil
			})
			deps := ServiceDeps{
				ServerState:                  state,
				HostMaintenanceSessions:      factory,
				CurrentJobManager:            func() *jobs.Manager { return nil },
				StartJobRunner:               func(string, func()) {},
				AuditWithActor:               func(string, string, string, string, string, string, string, map[string]any) {},
				LoadPostUpdateCheckConfig:    func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
				LoadScheduledJobBehavior:     func(string) ScheduledJobBehavior { return ScheduledJobBehavior{ApprovalTimeout: time.Minute} },
				SaveServerFacts:              func(ServerFactsRecord) error { return nil },
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
			}
			deps.WaitForApprovalPoll = func() {
				if closes[0] != 1 {
					t.Fatalf("discovery session close count before approval poll = %d, want 1", closes[0])
				}
				tc.resolve(service, server.Name)
			}
			service = NewService(deps)
			service.RunUpdateJob(UpdateRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})

			if opens != tc.wantOpens {
				t.Fatalf("session opens = %d, want %d", opens, tc.wantOpens)
			}
			status := state.CurrentStatusSnapshot(server.Name)
			if status == nil || status.Status != tc.wantFinal {
				t.Fatalf("final status = %+v, want %s", status, tc.wantFinal)
			}
		})
	}
}

func TestRunUpdateJobApprovalTimeoutDoesNotReopenHostSession(t *testing.T) {
	server := servers.Server{Name: "srv-timeout", User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "idle"}}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	now := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	opens := 0
	closed := 0
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			opens++
			return &HostMaintenanceSessionFuncs{
				RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
					return HostCommandResult{Attempts: 1}, nil
				},
				RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} },
				DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
					outcome := newPackageDiscoveryOutcome([]servers.PendingUpdate{{Package: "openssl", CVEState: "ready"}}, []string{"openssl"}, servers.UpgradePlan{StandardPackageCount: 1})
					return HostPackageDiscoveryResult{Outcome: outcome, Attempts: 1}, nil
				},
				CloseFunc: func() error { closed++; return nil },
			}, nil
		}),
		CurrentJobManager:            func() *jobs.Manager { return nil },
		StartJobRunner:               func(string, func()) {},
		AuditWithActor:               func(string, string, string, string, string, string, string, map[string]any) {},
		Now:                          func() time.Time { return now },
		LoadPostUpdateCheckConfig:    func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
		LoadScheduledJobBehavior:     func(string) ScheduledJobBehavior { return ScheduledJobBehavior{ApprovalTimeout: time.Minute} },
		WaitForApprovalPoll:          func() { now = now.Add(2 * time.Minute) },
		SaveServerFacts:              func(ServerFactsRecord) error { return nil },
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
	})
	service.RunUpdateJob(UpdateRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})
	if opens != 1 || closed != 1 {
		t.Fatalf("session opens/closes = %d/%d, want 1/1", opens, closed)
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "idle" {
		t.Fatalf("final status = %+v, want idle", status)
	}
}

func TestRunScheduledScanJobRecordsCVEResultOnJob(t *testing.T) {
	var auditActions []string
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	jobID := "scheduled-scan-job"
	jm := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		Now:   func() time.Time { return time.Date(2026, 5, 18, 12, 0, 0, 0, time.UTC) },
		NewID: func() string { return jobID },
	})
	policy := policies.Policy{ID: 7, Name: "daily", ExecutionMode: policies.ExecutionScanOnly, PackageScope: policies.PackageScopeSecurity}
	scheduledForUTC := "2026-05-18T12:00:00.000000000Z"
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindScheduledScan,
		ServerName: "srv",
		Actor:      "system",
		Status:     jobs.StatusQueued,
		MetaJSON:   jobs.MarshalJSON(BuildScheduledJobMeta(policy, scheduledForUTC)),
	}); err != nil {
		t.Fatalf("create scheduled scan job: %v", err)
	}
	deps := ServiceDeps{
		VulnerabilityScanner: VulnerabilityScannerFunc(func(_ context.Context, _ HostMaintenanceSession, updates []servers.PendingUpdate) ([]servers.PendingUpdate, error) {
			scanned := servers.ClonePendingUpdates(updates)
			for i := range scanned {
				if scanned[i].Package == "linux-image-amd64" {
					scanned[i].CVEState = "unavailable"
					scanned[i].CVEs = []string{}
					continue
				}
				scanned[i].CVEState = "ready"
				scanned[i].CVEs = []string{"CVE-2026-0001"}
				scanned[i].CVEFindings = []servers.VulnerabilityFinding{{
					ID:          "CVE-2026-0001",
					Disposition: VulnerabilityDispositionFixedByCandidate,
				}}
			}
			return scanned, nil
		}),
		HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				return HostCommandResult{Attempts: 1}, nil
			},
			RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary {
				return PrecheckSummary{AllPassed: true}
			},
			DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				pending := []servers.PendingUpdate{
					{Package: "openssl", Security: true, Raw: "Inst openssl"},
					{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true, Raw: "linux-image-amd64/stable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]"},
				}
				upgradable := []string{"openssl", "linux-image-amd64"}
				plan := servers.UpgradePlan{
					StandardPackageCount:       1,
					KeptBackPackageCount:       1,
					StandardSecurityCount:      1,
					TotalSecurityCount:         2,
					FullUpgradePackageCount:    2,
					FullUpgradeNewPackages:     []string{"linux-image-6.1.0-39-amd64"},
					FullUpgradeRemovedPackages: nil,
				}
				return HostPackageDiscoveryResult{Outcome: newPackageDiscoveryOutcome(pending, upgradable, plan), Attempts: 1}, nil
			},
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		AuditWithActor: func(_, _, action, _, _, _, _ string, _ map[string]any) {
			auditActions = append(auditActions, action)
		},
		UpdatePolicyRun: func(_ int64, update policies.RunUpdate) error {
			t.Fatalf("UpdatePolicyRun called from scheduled scan worker: %+v", update)
			return nil
		},
	}

	NewService(deps).RunScheduledScanJob(ScheduledScanRunRequest{
		RunID:           42,
		JobID:           jobID,
		ScheduledForUTC: scheduledForUTC,
		Server:          servers.Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"},
		Policy:          policy,
		RetryPolicy:     RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})

	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", jobID, err)
	}
	if job.Status != jobs.StatusSucceeded {
		t.Fatalf("job status = %q, want %q", job.Status, jobs.StatusSucceeded)
	}
	if len(auditActions) != 0 {
		t.Fatalf("auditActions=%v, want scheduled scan worker to leave audit to reconciliation", auditActions)
	}
	var meta ScheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		t.Fatalf("job meta JSON unmarshal error = %v", err)
	}
	if meta.Discovery == nil {
		t.Fatalf("job meta discovery = nil, want scan discovery")
	}
	discovery := *meta.Discovery
	if discovery.PendingPackageCount != 2 || discovery.SecurityPackageCount != 2 {
		t.Fatalf("discovery counts = pending %d security %d, want total pending/security including kept-back package", discovery.PendingPackageCount, discovery.SecurityPackageCount)
	}
	if discovery.UpgradePlan.StandardSecurityCount != 1 || discovery.UpgradePlan.TotalSecurityCount != 2 || discovery.UpgradePlan.KeptBackPackageCount != 1 {
		t.Fatalf("upgrade plan = %+v, want split standard/total security counts", discovery.UpgradePlan)
	}
	if got := SecurityPackagesFromPendingUpdates(discovery.PendingUpdates); !reflect.DeepEqual(got, []string{"openssl"}) {
		t.Fatalf("SecurityPackagesFromPendingUpdates() = %#v, want kept-back package excluded from standard security action", got)
	}
	states := make(map[string]servers.PendingUpdate, len(discovery.PendingUpdates))
	for _, update := range discovery.PendingUpdates {
		states[update.Package] = update
	}
	if got := states["openssl"]; got.CVEState != "ready" || !reflect.DeepEqual(got.CVEs, []string{"CVE-2026-0001"}) {
		t.Fatalf("openssl CVE result = %+v, want ready CVE data", got)
	}
	if got := states["linux-image-amd64"]; got.CVEState != "unavailable" || len(got.CVEs) != 0 {
		t.Fatalf("linux-image CVE result = %+v, want warning-only unavailable state", got)
	}
}
