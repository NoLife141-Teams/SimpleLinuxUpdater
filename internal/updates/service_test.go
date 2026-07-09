package updates

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

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

func TestServiceDepsDefaultQueryPackageCVEsIsSafeNoop(t *testing.T) {
	deps := ServiceDeps{}.withDefaults()
	cves, err := deps.QueryPackageCVEs(fakeConnection{}, "openssl")
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
				BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) {
					return nil, nil
				},
				HostKeyCallback: func() (ssh.HostKeyCallback, error) {
					return ssh.InsecureIgnoreHostKey(), nil
				},
				CurrentJobManager: func() *jobs.Manager {
					return nil
				},
				DialSSHWithRetry: func(servers.Server, *ssh.ClientConfig, RetryPolicy, string, *int) (SSHConnection, error) {
					return fakeConnection{}, nil
				},
				RunSSHOperationWithRetry: func(_ servers.Server, _ *ssh.ClientConfig, _ *SSHConnection, _ RetryPolicy, _ string, _ string, _ *int, operation func() error) error {
					return operation()
				},
				RunSSHCommandWithTimeout: func(_ SSHConnection, cmd string, _ io.Reader, _ time.Duration) (string, string, error) {
					commands = append(commands, cmd)
					return "", "", nil
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
				RunUpdatePrechecks: func(SSHConnection) PrecheckSummary {
					return PrecheckSummary{AllPassed: true}
				},
				ListFailedSystemdUnits: func(SSHConnection) ([]string, string, error) {
					return nil, "", nil
				},
				DiscoverPackages: func(SSHConnection, time.Duration) (PackageDiscoveryOutcome, error) {
					return newPackageDiscoveryOutcome(tc.pending, tc.upgradable, tc.plan), nil
				},
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				CollectServerFacts: func(servers.Server, SSHConnection, time.Duration) ServerFactsRecord {
					return ServerFactsRecord{}
				},
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
				BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) {
					return nil, nil
				},
				HostKeyCallback: func() (ssh.HostKeyCallback, error) {
					return ssh.InsecureIgnoreHostKey(), nil
				},
				CurrentJobManager: func() *jobs.Manager {
					return nil
				},
				DialSSHWithRetry: func(servers.Server, *ssh.ClientConfig, RetryPolicy, string, *int) (SSHConnection, error) {
					return fakeConnection{}, nil
				},
				RunSSHOperationWithRetry: func(_ servers.Server, _ *ssh.ClientConfig, _ *SSHConnection, _ RetryPolicy, _ string, _ string, _ *int, operation func() error) error {
					return operation()
				},
				RunSSHCommandWithTimeout: func(_ SSHConnection, cmd string, _ io.Reader, _ time.Duration) (string, string, error) {
					commands = append(commands, cmd)
					return "", "", nil
				},
				LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig {
					return PostUpdateCheckConfig{Enabled: false}
				},
				LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
					return ScheduledJobBehavior{ApprovalTimeout: 2 * time.Second}
				},
				RunUpdatePrechecks: func(SSHConnection) PrecheckSummary {
					return PrecheckSummary{AllPassed: true}
				},
				ListFailedSystemdUnits: func(SSHConnection) ([]string, string, error) {
					return nil, "", nil
				},
				DiscoverPackages: func(SSHConnection, time.Duration) (PackageDiscoveryOutcome, error) {
					return newPackageDiscoveryOutcome(tc.pending, []string{"linux-image-amd64"}, tc.plan), nil
				},
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				CollectServerFacts: func(servers.Server, SSHConnection, time.Duration) ServerFactsRecord {
					return ServerFactsRecord{}
				},
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
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSHWithRetry: func(servers.Server, *ssh.ClientConfig, RetryPolicy, string, *int) (SSHConnection, error) {
			return fakeConnection{}, nil
		},
		RunSSHOperationWithRetry: func(_ servers.Server, _ *ssh.ClientConfig, _ *SSHConnection, _ RetryPolicy, _ string, _ string, _ *int, operation func() error) error {
			return operation()
		},
		RunSSHCommandWithTimeout: func(SSHConnection, string, io.Reader, time.Duration) (string, string, error) {
			return "", "", nil
		},
		CurrentJobManager: func() *jobs.Manager { return jm },
		AuditWithActor: func(_, _, action, _, _, _, _ string, _ map[string]any) {
			auditActions = append(auditActions, action)
		},
		RunUpdatePrechecks: func(SSHConnection) PrecheckSummary {
			return PrecheckSummary{AllPassed: true}
		},
		DiscoverPackages: func(SSHConnection, time.Duration) (PackageDiscoveryOutcome, error) {
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
			return newPackageDiscoveryOutcome(pending, upgradable, plan), nil
		},
		QueryPackageCVEs: func(_ SSHConnection, pkg string) ([]string, error) {
			if pkg == "linux-image-amd64" {
				return nil, errors.New("changelog unavailable")
			}
			return []string{"CVE-2026-0001"}, nil
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
