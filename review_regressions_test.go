package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	jobspkg "debian-updater/internal/jobs"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

func TestReviewSSHHandshakeHonorsConfiguredTimeout(t *testing.T) {
	for _, mode := range []string{"maintenance", "host_key_scan"} {
		t.Run(mode, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			accepted := make(chan net.Conn, 1)
			go func() {
				c, e := listener.Accept()
				if e == nil {
					accepted <- c
				}
			}()
			host, rawPort, _ := net.SplitHostPort(listener.Addr().String())
			port, _ := strconv.Atoi(rawPort)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				var e error
				if mode == "host_key_scan" {
					_, e = serverpkg.ScanHostKey(host, port, 50*time.Millisecond)
				} else {
					_, e = dialRealSSHConnectionWithContext(ctx, Server{Host: host, Port: port}, &ssh.ClientConfig{User: "review", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 50 * time.Millisecond})
				}
				done <- e
			}()
			var peer net.Conn
			select {
			case c := <-accepted:
				peer = c
				defer c.Close()
			case <-time.After(time.Second):
				t.Fatal("no accepted connection")
			}
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("expected timeout")
				}
			case <-time.After(500 * time.Millisecond):
				cancel()
				_ = peer.Close() // Also release a regressed, non-cancellable host-key scan.
				<-done
				t.Fatal("SSH handshake still blocked at 10x the configured connect timeout")
			}
		})
	}
}

func TestReviewRevokedSessionCannotBeResurrectedByInflightRequest(t *testing.T) {
	for _, mode := range []string{"revoke", "clear_all", "clear_other", "password"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(sessionIdleTimeoutHoursEnv, "1")
			app := newIsolatedTestApp(t)
			cookie := app.authenticate(t)
			entered := make(chan struct{})
			release := make(chan struct{})
			app.Router.GET("/api/review-block", func(c *gin.Context) { close(entered); <-release; c.Status(http.StatusOK) })
			request := httptest.NewRequest(http.MethodGet, "/api/review-block", nil)
			request.AddCookie(cookie)
			done := make(chan struct{})
			go func() { app.Handler.ServeHTTP(httptest.NewRecorder(), request); close(done) }()
			<-entered
			sessions, err := app.Deps.AuthService.ListSessions("")
			if err != nil || len(sessions) != 1 {
				close(release)
				<-done
				t.Fatalf("sessions = %+v, err = %v", sessions, err)
			}
			revoked := false
			retained := 0
			switch mode {
			case "revoke":
				revoked, err = app.Deps.AuthService.RevokeSession(sessions[0].ID)
			case "clear_all":
				_, err = app.Deps.AuthService.ClearSessions()
				revoked = err == nil
			default:
				login := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+testPasswordStrong+`"}`))
				login.Header.Set("Content-Type", "application/json")
				markSameOriginAuthRequest(login)
				response := httptest.NewRecorder()
				app.Handler.ServeHTTP(response, login)
				if response.Code != http.StatusOK {
					close(release)
					<-done
					t.Fatalf("login: %d %s", response.Code, response.Body.String())
				}
				current := testSessionCookieFromRecorder(t, response)
				if mode == "password" {
					change := httptest.NewRequest(http.MethodPut, "/api/auth/password", strings.NewReader(`{"current_password":"`+testPasswordStrong+`","new_password":"AnotherStrongPass123","confirm_password":"AnotherStrongPass123","invalidate_other_sessions":true}`))
					change.AddCookie(current)
					change.Header.Set("Content-Type", "application/json")
					markSameOriginAuthRequest(change)
					response = httptest.NewRecorder()
					app.Handler.ServeHTTP(response, change)
					if response.Code != http.StatusOK {
						close(release)
						<-done
						t.Fatalf("password: %d %s", response.Code, response.Body.String())
					}
				} else {
					_, err = app.Deps.AuthService.ClearOtherSessions(current.Value)
				}
				retained = 1
				revoked = err == nil
			}
			if err != nil || !revoked {
				close(release)
				<-done
				t.Fatalf("revoke = %v, %v", revoked, err)
			}
			if count, _ := app.Deps.AuthService.CountSessions(); count != retained {
				t.Error("revocation did not delete the stored session")
			}
			close(release)
			<-done
			probe := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
			probe.AddCookie(cookie)
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, probe)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("revoked cookie still authenticates: HTTP %d; want 401", rec.Code)
			}
		})
	}
}

type reviewMutationConn struct {
	mu       sync.Mutex
	commands int
	output   string
	err      error
}
type reviewMutationSession struct {
	conn   *reviewMutationConn
	stdout io.Writer
}

func (c *reviewMutationConn) NewSession() (sshSessionRunner, error) {
	return &reviewMutationSession{conn: c}, nil
}
func (c *reviewMutationConn) Close() error             { return nil }
func (s *reviewMutationSession) SetStdin(io.Reader)    {}
func (s *reviewMutationSession) SetStdout(w io.Writer) { s.stdout = w }
func (s *reviewMutationSession) SetStderr(io.Writer)   {}
func (s *reviewMutationSession) Close() error          { return nil }
func (s *reviewMutationSession) Run(string) error {
	s.conn.mu.Lock()
	s.conn.commands++
	s.conn.mu.Unlock()
	output := s.conn.output
	if output == "" {
		output = "Removing old-package (1.0) ...\n"
	}
	_, _ = io.WriteString(s.stdout, output)
	return s.conn.err
}
func TestReviewAPTTransportLossRequiresReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"missing_exit_status", &ssh.ExitMissingError{}},
		{"connection_reset", errors.New("connection reset by peer")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "review-host", Host: "192.0.2.55", User: "root"})
			if err != nil {
				t.Fatal(err)
			}
			conn := &reviewMutationConn{err: tc.err}
			factory := newHostMaintenanceSessionFactory(
				func(Server) ([]ssh.AuthMethod, error) { return nil, nil },
				func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
				func(Server, *ssh.ClientConfig) (sshConnection, error) { return conn, nil },
			)
			var auditMeta map[string]any
			service := NewUpdateService(UpdateServiceDeps{
				HostMaintenanceSessions: factory, ServerState: app.Deps.ServerState,
				CurrentJobManager:  app.Deps.CurrentJobManager,
				LoadCommandTimeout: func() time.Duration { return time.Second },
				AuditWithActor:     func(_, _, _, _, _, _, _ string, meta map[string]any) { auditMeta = meta },
			})
			policy := RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}
			job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindAutoremove, server.Name, "review", "127.0.0.1", policy)
			if err != nil {
				t.Fatal(err)
			}
			service.RunAutoremoveJob(AutoremoveRunRequest{Server: server, Actor: "review", Policy: policy, JobID: job.ID})
			status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
			persisted, err := app.Deps.CurrentJobManager().GetJob(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if conn.commands != 1 {
				t.Errorf("package mutation was invoked %d times after transport loss; want 1", conn.commands)
			}
			if status.Status != "needs_reconciliation" || persisted.ErrorClass != "reconciliation_required" || auditMeta["last_error_class"] != "reconciliation_required" {
				t.Errorf("runtime=%s job=%s error_class=%s audit_class=%v; want reconciliation_required", status.Status, persisted.Status, persisted.ErrorClass, auditMeta["last_error_class"])
			}
		})
	}
}

func TestReviewFactoryOpenHonorsCallerCancellation(t *testing.T) {
	lifecycle, cancelLifecycle := context.WithCancel(context.Background())
	defer cancelLifecycle()
	restore := setApplicationMaintenanceContext(lifecycle)
	defer restore()
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	factory := newHostMaintenanceSessionFactory(
		func(Server) ([]ssh.AuthMethod, error) { return nil, nil },
		func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		func(Server, *ssh.ClientConfig) (sshConnection, error) {
			close(entered)
			<-release
			return nil, errors.New("review dial released")
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := factory.Open(ctx, HostMaintenanceSessionRequest{Server: Server{User: "root"}, RetryPolicy: RetryPolicy{MaxAttempts: 1}})
		done <- err
	}()
	<-entered
	cancel()
	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		cancelLifecycle()
		<-done
		t.Fatal("cancelling the caller did not stop Open; only cancelling the application lifecycle stopped it")
	}
}

func TestReviewEndpointChangeInvalidatesCollectedFacts(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "review-host", Host: "192.0.2.10", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	reboot := false
	err = app.Deps.HostHealthObservation.AcceptCollectedFacts(serverFactsRecord{
		ServerName: server.Name, CollectedAt: time.Now().UTC().Format(time.RFC3339),
		OSPrettyName: "OLD HOST OS", DiskStatus: "ok", DiskFreeKB: 10000000, DiskTotalKB: 20000000, AptStatus: "ok", RebootRequired: &reboot,
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Host = "192.0.2.20"
	if _, err := app.Deps.ServerInventoryService.Update(server.Name, server); err != nil {
		t.Fatal(err)
	}
	summary, err := app.Deps.ObservabilityService.BuildDashboardSummary("24h", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Servers) != 1 {
		t.Fatalf("server count %d", len(summary.Servers))
	}
	got := summary.Servers[0]
	if got.Health.OSPrettyName == "OLD HOST OS" && got.ApprovalTriage.FactsState == "fresh" {
		t.Fatalf("new endpoint still shows old OS=%q with facts_state=%q", got.Health.OSPrettyName, got.ApprovalTriage.FactsState)
	}
}

func TestReviewApprovalRevalidatesChangedRemovalPlan(t *testing.T) {
	for _, scope := range []string{"full_upgrade", "security_kept_back"} {
		for _, confirmedInitially := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/confirmed_%t", scope, confirmedInitially), func(t *testing.T) {
				app := newIsolatedTestApp(t)
				server, err := app.Deps.ServerInventoryService.Create(Server{Name: "review-host", Host: "192.0.2.30", User: "root"})
				if err != nil {
					t.Fatal(err)
				}
				discoveryCalls := 0
				remotePlanChanged := false
				mutations, approvals := 0, 0
				var auditMeta map[string]any
				session := &HostMaintenanceSessionFuncs{
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						discoveryCalls++
						plan := UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradePackageCount: 1, StandardPackageCount: 1, KeptBackSecurityPlanAvailable: true, KeptBackSecurityPackageCount: 1}
						if confirmedInitially {
							plan.FullUpgradeRemovedPackages = []string{"old-obsolete"}
							plan.KeptBackSecurityRemovedPackages = []string{"old-obsolete"}
						}
						if remotePlanChanged {
							plan.FullUpgradeRemovedPackages = []string{"application-service"}
							plan.KeptBackSecurityRemovedPackages = []string{"application-service"}
						}
						return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{
							PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{{Package: "openssl", CVEState: "done", Security: true, KeptBack: scope == "security_kept_back", RequiresFull: scope == "security_kept_back"}}, UpgradePlan: plan,
						}}, nil
					},
					RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
						if req.Effect == updatespkg.HostCommandEffectPackageStateMutation {
							mutations++
							if approvals != 2 || discoveryCalls != 3 {
								t.Errorf("mutation before renewed approval: approvals=%d discoveries=%d", approvals, discoveryCalls)
							}
						}
						return HostCommandResult{Attempts: 1}, nil
					},
				}
				var service *UpdateService
				service = NewUpdateService(UpdateServiceDeps{
					ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager,
					HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
						return session, nil
					}),
					LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
					LoadScheduledJobBehavior:  func(string) scheduledJobBehavior { return scheduledJobBehavior{ApprovalTimeout: time.Minute} },
					WaitForApprovalPollContext: func(context.Context) error {
						approvals++
						if approvals > 2 {
							t.Fatal("approval revalidation did not converge")
						}
						if approvals == 2 {
							status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
							if status.ApprovalConfirmRemovals || !slices.Equal(status.UpgradePlan.FullUpgradeRemovedPackages, []string{"application-service"}) {
								t.Fatalf("changed plan did not require fresh confirmation: %+v", status)
							}
						}
						remotePlanChanged = true
						exists, approved := service.ApprovePendingUpdateWithOptions(server.Name, scope, serverpkg.ApprovalOptions{ConfirmRemovals: approvals == 2 || confirmedInitially})
						if !exists || !approved {
							t.Fatalf("approval=%v/%v", exists, approved)
						}
						return nil
					},
					UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
					SaveServerFacts:              func(serverFactsRecord) error { return nil },
					AuditWithActor:               func(_, _, _, _, _, _, _ string, meta map[string]any) { auditMeta = meta },
				})
				job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "127.0.0.1", RetryPolicy{MaxAttempts: 1})
				if err != nil {
					t.Fatal(err)
				}
				service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Actor: "review", Policy: RetryPolicy{MaxAttempts: 1}})
				if mutations != 1 || approvals != 2 || discoveryCalls != 3 {
					t.Fatalf("mutations=%d approvals=%d discoveries=%d", mutations, approvals, discoveryCalls)
				}
				if auditMeta["approval_scope"] != scope || auditMeta["approved_package_count"] != 1 {
					t.Fatalf("audit does not describe accepted execution: %+v", auditMeta)
				}
				if got := app.Deps.ServerState.CurrentStatusSnapshot(server.Name).Status; got != "done" {
					t.Fatalf("status=%s", got)
				}
			})
		}
	}
}

func TestReviewOlderCompletionCannotReleaseNewAction(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "review-host", Host: "192.0.2.40", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	oldID := ""
	oldPublicationEntered := make(chan struct{})
	releaseOldPublication := make(chan struct{})
	jm := jobspkg.NewManager(jobspkg.NewSQLiteRepository(app.Deps.DB()), jobspkg.ManagerOptions{
		SyncRuntime: func(record JobRecord) {
			if record.ID == oldID && record.Status == jobStatusSucceeded {
				close(oldPublicationEntered)
				<-releaseOldPublication
			}
			syncServerStateFromJobRecord(app.Deps.ServerState, record)
		},
	})
	oldJob, err := jm.CreateJob(JobCreateParams{Kind: jobKindAutoremove, ServerName: server.Name, Status: jobStatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	oldID = oldJob.ID
	service := NewUpdateService(UpdateServiceDeps{
		ServerState: app.Deps.ServerState, CurrentJobManager: func() *JobManager { return jm },
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return &HostMaintenanceSessionFuncs{}, nil
		}),
		AuditWithActor: func(_, _, _, _, _, _, _ string, _ map[string]any) {},
	})
	done := make(chan struct{})
	go func() {
		service.RunAutoremoveJob(AutoremoveRunRequest{Server: server, JobID: oldJob.ID, Policy: RetryPolicy{MaxAttempts: 1}})
		close(done)
	}()
	<-oldPublicationEntered
	if _, err := app.Deps.ServerState.BeginPackageMutation(server.Name, "updating"); !errors.Is(err, serverpkg.ErrActionInProgress) {
		t.Errorf("completion must retain admission until publication finishes, got %v", err)
	}
	close(releaseOldPublication)
	<-done
	if _, err := app.Deps.ServerState.BeginPackageMutation(server.Name, "updating"); err != nil {
		t.Fatal(err)
	}
	newer, err := createServerActionJobWithStateAndManager(jm, app.Deps.ServerState, jobKindUpdate, server.Name, "review", "", RetryPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	running, phase := jobStatusRunning, jobPhasePrechecks
	if err := jm.Transition(newer.ID, JobTransitionIntent{Status: &running, Phase: &phase}); err != nil {
		close(releaseOldPublication)
		<-done
		t.Fatal(err)
	}
	before := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
	if before.JobID != newer.ID || before.Status != "updating" {
		t.Errorf("new action did not publish correctly: %+v", before)
	}
	oldRecord, err := jm.GetJob(oldJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	syncServerStateFromJobRecord(app.Deps.ServerState, oldRecord)
	after := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
	if after.JobID != newer.ID || after.Status != "updating" {
		t.Errorf("old completion overwrote newer running action: status=%s job=%s (old=%s new=%s)", after.Status, after.JobID, oldJob.ID, newer.ID)
	}
	if _, err := app.Deps.ServerState.BeginPackageMutation(server.Name, "autoremove"); err == nil {
		t.Error("a third package mutation was admitted while the newer update job is still running")
	}
}
