package main

import (
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

func TestRestoredAPTProofFencesAdmissionUntilFreshResume(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
		Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
	})
	admin := app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("operator", testPasswordStrong, "operator"); err != nil {
		t.Fatal(err)
	}
	operator := identityLogin(t, app, "operator")
	inventory := app.Deps.ServerInventoryService
	hosts := make([]Server, 0, 2)
	for _, name := range []string{"fresh", "expired"} {
		server, err := inventory.Create(Server{Name: name, Host: name + ".example.test", User: "root"})
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, server)
		if _, err := app.Deps.DB().Exec("INSERT INTO restore_apt_validation(identity) VALUES (?)", server.Identity); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	var dials []string
	fingerprint := "SHA256:validated"
	app.Deps.RestoreAPTValidation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		dials = append(dials, req.Server.Name)
		if err := req.VerifyHostKey(fingerprint); err != nil {
			return nil, err
		}
		if req.OnConnectedHostKey != nil {
			req.OnConnectedHostKey(fingerprint)
		}
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string { return "healthy" }}, nil
	})
	body := func(server Server) gin.H { return gin.H{"name": server.Name, "identity": server.Identity} }
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, operator, "", body(hosts[0])); res.Code != 403 {
		t.Fatalf("operator APT probe = %d, %s", res.Code, res.Body.String())
	}
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", gin.H{"name": "fresh", "identity": hosts[1].Identity}); res.Code != 409 {
		t.Fatalf("probe with stale selection = %d, %s", res.Code, res.Body.String())
	}
	if len(dials) != 0 {
		t.Fatalf("denied requests connected to %v", dials)
	}
	for _, index := range []int{1, 0} {
		res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", body(hosts[index]))
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"state":"healthy"`) || strings.Contains(res.Body.String(), fingerprint) {
			t.Fatalf("probe = %d, %s", res.Code, res.Body.String())
		}
		if index == 1 {
			clock.Add(int64(restoreAPTProofLifetime/time.Second) + 1)
		}
	}
	res := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", gin.H{"confirmation": "RESUME"})
	if res.Code != 200 {
		t.Fatalf("resume = %d, %s", res.Code, res.Body.String())
	}
	v := app.Deps.RestoreAPTValidation
	if got, err := v.authorize(t.Context(), hosts[0]); err != nil || got != fingerprint {
		t.Fatalf("fresh proof = %q, %v", got, err)
	}
	if _, err := v.authorize(t.Context(), hosts[1]); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("expired proof error = %v", err)
	}
	changed := hosts[0]
	changed.Host = "changed.example.test"
	if _, err := v.authorize(t.Context(), changed); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("proof still admitted a changed endpoint: %v", err)
	}
	fingerprint = "SHA256:different"
	if _, err := guardRestoredAPTSession(v).Open(t.Context(), HostMaintenanceSessionRequest{Server: hosts[0], PackageMaintenance: true}); err == nil {
		t.Fatal("maintenance session used a different SSH key than the validated key")
	}
	if _, err := v.authorize(t.Context(), hosts[0]); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("key change did not revoke the proof: %v", err)
	}
	run := scheduledRunLifecycleFromComposedApp(app.Deps).HandleScheduledRun(policypkg.ScheduledRunRequest{
		Policy: policypkg.Policy{ID: 1, Name: "After restore", ExecutionMode: policypkg.ExecutionAutoApply},
		Server: hosts[1], ScheduledForUTC: time.Unix(clock.Load(), 0).UTC().Format(time.RFC3339Nano),
	})
	if run.Err != nil || run.RunID == 0 || run.JobID != "" {
		t.Fatalf("scheduled run = %+v", run)
	}
	stored, err := app.Deps.PolicyRepository.GetRun(run.RunID)
	if err != nil || stored.Status != policypkg.RunSkipped || !strings.Contains(stored.Summary, "APT validation is required") {
		t.Fatalf("scheduled denial = %+v, %v", stored, err)
	}
}

func TestRestoredAPTInventoryKeepsProofBoundAcrossTargetChanges(t *testing.T) {
	app := newIsolatedTestApp(t)
	inventory := app.Deps.ServerInventoryService
	restored, err := inventory.Create(Server{Name: "recovered", Host: "recovered.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	validation := app.Deps.RestoreAPTValidation
	if isolated, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil || !isolated {
		t.Fatalf("prepare restored inventory = %t, %v", isolated, err)
	}

	const fingerprint = "SHA256:restored"
	if _, err := app.Deps.DB().Exec(`UPDATE restore_apt_validation
		SET host_key=?,result='healthy',approved=1 WHERE identity=?`, fingerprint, restored.Identity); err != nil {
		t.Fatal(err)
	}
	if key, err := validation.authorize(t.Context(), restored); err != nil || key != fingerprint {
		t.Fatalf("original proof = %q, %v", key, err)
	}
	moved := restored
	moved.Host = "moved.example.test"
	if _, err := inventory.Update(restored.Name, moved); err != nil {
		t.Fatal(err)
	}
	if _, err := inventory.Update(restored.Name, restored); err != nil {
		t.Fatal(err)
	}
	if _, err := validation.authorize(t.Context(), restored); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("returning to the old endpoint yielded %v, want revalidation", err)
	}
	if err := inventory.Delete(restored.Name); err != nil {
		t.Fatal(err)
	}
	if isolated, err := validation.prepare(t.Context(), app.Deps.DB(), false); err != nil || !isolated {
		t.Fatalf("second restore omitting the protected endpoint = %t, %v", isolated, err)
	}
	replacement, err := inventory.Create(Server{Name: "replacement", Host: restored.Host, User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := validation.authorize(t.Context(), replacement); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("recreated endpoint yielded %v, want revalidation", err)
	}
}

func TestSameKeyRestoreIntoEmptyInventoryRequiresAPTValidation(t *testing.T) {
	app := newIsolatedTestApp(t)
	staged, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "staged.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	if err := ensureSchema(staged); err != nil {
		t.Fatal(err)
	}
	repo := serverpkg.SQLiteRepository{DB: func() *sql.DB { return staged }}
	servers := []Server{{Name: "archived", Identity: "restored-identity", Host: "archived.example.test", User: "root"}}
	if err := repo.Save(servers, nil); err != nil {
		t.Fatal(err)
	}
	isolated, err := app.Deps.RestoreAPTValidation.prepare(t.Context(), staged, false)
	if err != nil || !isolated {
		t.Fatalf("prepare same-key restore into empty inventory = %t, %v", isolated, err)
	}
	recovery := &restoreAPTValidation{db: func() *sql.DB { return staged }}
	if _, err := recovery.authorize(t.Context(), servers[0]); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("restored package admission = %v, want revalidation", err)
	}
}

func TestRestoredAPTCheckReservesHostUntilObservationIsSaved(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "srv", Host: "srv.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	validation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		// Simulate a scheduled action claiming the host while SSH is open.
		if _, _, err := app.Deps.ServerState.BeginActionWithOptions(req.Server.Name, "updating", serverpkg.ActionAdmissionOptions{PackageMutation: true}); !errors.Is(err, serverpkg.ErrActionInProgress) {
			t.Fatalf("concurrent maintenance admission = %v, want host busy", err)
		}
		req.OnConnectedHostKey("SHA256:restored")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string { return "healthy" }}, nil
	})
	rec := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", gin.H{"name": server.Name, "identity": server.Identity})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"approved"`) {
		t.Fatalf("restore validation = %d, %s", rec.Code, rec.Body.String())
	}
	if active, _ := app.Deps.ServerState.ActionStatusInProgress(server.Name); active {
		t.Fatal("completed validation left the host busy")
	}
	// Another check can complete after admission reads the previous proof.
	started := false
	_, _, err = app.Deps.ServerState.BeginActionWithOptions(server.Name, "updating", serverpkg.ActionAdmissionOptions{
		PackageMutation: true,
		ValidatePackageMutation: func(Server) error {
			started = true
			rec := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", gin.H{"name": server.Name, "identity": server.Identity})
			if rec.Code != 200 {
				t.Fatalf("concurrent check = %d, %s", rec.Code, rec.Body.String())
			}
			return nil
		},
	})
	if !started || !errors.Is(err, serverpkg.ErrActionInProgress) {
		t.Fatalf("admission after newer check = %v, want retry", err)
	}
}

func TestRestoreAPTHealthKeepsArchivedReconciliationVisible(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "srv", Host: "srv.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	app.Deps.ServerState.Lock()
	status := app.Deps.ServerState.StatusMap()[server.Name]
	status.APTReconciliationPending = true
	status.APTReconciliationJobID = "archived-incident"
	app.Deps.ServerState.Unlock()
	validation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		req.OnConnectedHostKey("SHA256:approved")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string { return "healthy" }}, nil
	})
	rec := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", gin.H{"name": server.Name, "identity": server.Identity})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"reconciliation_required"`) {
		t.Fatalf("restore validation hid the archived incident: %d, %s", rec.Code, rec.Body.String())
	}
	facts := validation.hostFacts(t.Context(), app.Deps.ServerState, app.Deps.Now(), false)
	if len(facts) != 1 || facts[0].State != "reconciliation_required" {
		t.Fatalf("installation facts hide the archived incident: %+v", facts)
	}
}

func TestCancelledRestoreAPTRecheckRevokesAcceptedProof(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "srv", Host: "srv.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.DB().Exec(`UPDATE restore_apt_validation SET host_key='SHA256:approved',result='healthy',approved=1 WHERE identity=?`, server.Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := validation.authorize(t.Context(), server); err != nil {
		t.Fatalf("authorize prior check: %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	validation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		req.OnConnectedHostKey("SHA256:approved")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string {
			cancel()
			return "unavailable"
		}}, nil
	})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", restoreAPTValidationRoute,
		strings.NewReader(`{"name":"srv","identity":"`+server.Identity+`"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	markSameOriginAuthRequest(req)
	app.Handler.ServeHTTP(rec, req)
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("check never reached the host: %d, %s", rec.Code, rec.Body.String())
	}
	if _, err := validation.authorize(t.Context(), server); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("cancelled check reused prior permission: %v", err)
	}
}

func TestRestoreAPTProofRevokedWhenHostKeyChangesDuringReconnect(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "srv", Host: "srv.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		t.Fatal(err)
	}
	seed := [ed25519.SeedSize]byte{1}
	changedKey, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(seed[:]).Public())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.DB().Exec(`UPDATE restore_apt_validation SET host_key=?,result='healthy',approved=1 WHERE identity=?`,
		ssh.FingerprintSHA256(key), server.Identity); err != nil {
		t.Fatal(err)
	}
	dials := 0
	validation.sessions = updatespkg.NewProductionHostMaintenanceSessionFactory(updatespkg.ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback: func() (ssh.HostKeyCallback, error) {
			return func(string, net.Addr, ssh.PublicKey) error { return nil }, nil
		},
		DialSSH: func(_ context.Context, _ Server, config *ssh.ClientConfig) (sshConnection, error) {
			dials++
			presentedKey := key
			if dials != 1 {
				presentedKey = changedKey
			}
			if err := config.HostKeyCallback("srv.example.test", nil, presentedKey); err != nil {
				return nil, err
			}
			return &scriptedSSHConnection{}, nil
		},
		RunCommand: func(context.Context, sshConnection, string, updatespkg.HostCommandEffect, io.Reader, time.Duration) (string, string, error) {
			return "", "", errors.New("connection reset by peer")
		},
		Sleep: func(time.Duration) {},
	})
	session, err := guardRestoredAPTSession(validation).Open(t.Context(), HostMaintenanceSessionRequest{
		Server: server, PackageMaintenance: true, RetryPolicy: RetryPolicy{MaxAttempts: 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if _, err := session.RunCommand(t.Context(), updatespkg.HostCommandRequest{
		Operation: "inspect", Command: "inspect", Effect: updatespkg.HostCommandEffectReadOnly,
	}); !errors.Is(err, errRestoredAPTKeyChanged) {
		t.Fatalf("reconnect = %v, want changed key rejection", err)
	}
	if _, err := validation.authorize(t.Context(), server); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("reconnect retained prior permission: %v", err)
	}
}
