package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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

func TestCancelledRestoreAPTBatchRevokesPriorProofAndRejectsConcurrentRecheck(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	var targets []restoreAPTCheckTarget
	var servers []Server
	for _, name := range []string{"approved", "pending"} {
		server, err := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: name + ".example.test", User: "root"})
		if err != nil {
			t.Fatal(err)
		}
		servers = append(servers, server)
		targets = append(targets, restoreAPTCheckTarget{Name: server.Name, Identity: server.Identity})
	}
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.DB().Exec(`UPDATE restore_apt_validation SET host_key='SHA256:approved',result='healthy',approved=1 WHERE identity=?`, servers[0].Identity); err != nil {
		t.Fatal(err)
	}
	if _, err := validation.authorize(t.Context(), servers[0]); err != nil {
		t.Fatalf("authorize prior check: %v", err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	entered := make(chan string, len(targets))
	validation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		req.OnConnectedHostKey("SHA256:approved")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(probeCtx context.Context) string {
			entered <- req.Server.Name
			<-probeCtx.Done()
			return "healthy"
		}}, nil
	})
	body, err := json.Marshal(gin.H{"targets": targets})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", restoreAPTValidationRoute, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(admin)
	markSameOriginAuthRequest(req)
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		app.Handler.ServeHTTP(httptest.NewRecorder(), req)
	}()
	wait := time.After(5 * time.Second)
	for range targets {
		select {
		case <-entered:
		case <-wait:
			t.Fatal("batch did not reach both hosts")
		}
	}
	if rec := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", targets[0]); rec.Code != http.StatusConflict {
		t.Errorf("concurrent check of same host: %d, %s", rec.Code, rec.Body)
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("batch did not release its lease on cancellation")
	}
	for _, server := range servers {
		if _, err := validation.authorize(t.Context(), server); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
			t.Errorf("cancelled check authorized %s: %v", server.Name, err)
		}
	}
}

func TestRestoredAPTBatchChecksSelectedHostsWithBoundedConcurrency(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	loggedIn := identityLogin(t, app, "admin")
	sm := app.Deps.CurrentSessionManager()
	staleSession, err := sm.Load(t.Context(), loggedIn.Value)
	if err != nil {
		t.Fatal(err)
	}
	sm.Put(staleSession, "auth_reauthenticated_at", time.Now().Add(-6*time.Minute).Unix())
	if _, _, err := sm.Commit(staleSession); err != nil {
		t.Fatal(err)
	}
	var selected []restoreAPTCheckTarget
	var servers []Server
	for _, name := range []string{"healthy", "offline", "changed-key", "locked", "moved", "later", "stale", "not-selected"} {
		server, err := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: name + ".example.test", User: "root"})
		if err != nil {
			t.Fatal(err)
		}
		servers = append(servers, server)
		if name != "not-selected" {
			selected = append(selected, restoreAPTCheckTarget{Name: name, Identity: server.Identity})
		}
	}
	selected[6].Identity = "older-identity"
	validation := app.Deps.RestoreAPTValidation
	if _, err := validation.prepare(t.Context(), app.Deps.DB(), true); err != nil {
		t.Fatal(err)
	}
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	var active, peak atomic.Int32
	entered := make(chan string, len(servers))
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	validation.sessions = HostMaintenanceSessionFactoryFunc(func(ctx context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		n := active.Add(1)
		for max := peak.Load(); n > max && !peak.CompareAndSwap(max, n); max = peak.Load() {
		}
		defer active.Add(-1)
		entered <- req.Server.Name
		select {
		case <-unblock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		switch req.Server.Name {
		case "offline":
			return nil, errors.New("offline")
		case "changed-key":
			return nil, errRestoredAPTKeyChanged
		}
		req.OnConnectedHostKey("SHA256:fixture")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string {
			if req.Server.Name == "locked" {
				return "locks_unverified"
			}
			return "healthy"
		}}, nil
	})
	post := func(cookie *http.Cookie, targets []restoreAPTCheckTarget) *httptest.ResponseRecorder {
		return identityRequest(app, "POST", restoreAPTValidationRoute, cookie, "", gin.H{"targets": targets})
	}
	if rec := post(loggedIn, selected); rec.Code != http.StatusUnauthorized {
		t.Fatalf("without recent reauthentication: %d, %s", rec.Code, rec.Body)
	}
	if rec := post(admin, []restoreAPTCheckTarget{selected[0], selected[0]}); rec.Code != http.StatusBadRequest {
		t.Fatalf("duplicate selection: %d, %s", rec.Code, rec.Body)
	}
	overLimit := make([]restoreAPTCheckTarget, restoreAPTCheckBatchLimit+1)
	if rec := post(admin, overLimit); rec.Code != http.StatusBadRequest {
		t.Fatalf("oversize selection: %d, %s", rec.Code, rec.Body)
	}

	responses := make(chan *httptest.ResponseRecorder, 1)
	go func() { responses <- post(admin, selected) }()
	limit := time.After(5 * time.Second)
	for range restoreAPTCheckConcurrency {
		select {
		case <-entered:
		case <-limit:
			t.Fatal("concurrent probes never reached SSH")
		}
	}
	var inProgress int
	var displayedTargetRevision string
	for _, fact := range validation.hostFacts(t.Context(), app.Deps.ServerState, validation.now(), true) {
		if fact.Name == "moved" {
			displayedTargetRevision = fact.TargetRevision
		}
		if fact.Checking {
			inProgress++
		}
	}
	if inProgress != restoreAPTCheckConcurrency {
		t.Errorf("reported %d active checks", inProgress)
	}
	if displayedTargetRevision == "" {
		t.Fatal("installation health did not expose the selection's target revision")
	}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", gin.H{"confirmation": "RESUME"}); rec.Code != http.StatusServiceUnavailable {
		t.Errorf("resume during active batch: %d, %s", rec.Code, rec.Body)
	}
	moved := servers[4]
	moved.Host = "new-target.example.test"
	updated, err := app.Deps.ServerInventoryService.Update(moved.Name, moved)
	if err != nil {
		t.Fatal(err)
	}
	servers[4] = updated
	close(unblock)
	rec := <-responses
	var response struct{ Results []restoreAPTCheckResult }
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || rec.Code != http.StatusOK || len(response.Results) != len(selected) {
		t.Fatalf("batch result: %d, %s (%v)", rec.Code, rec.Body, err)
	}
	if peak.Load() != restoreAPTCheckConcurrency || len(entered) != 1 {
		t.Fatalf("dialed outside selection or concurrency limit: peak %d, extra dials %d", peak.Load(), len(entered))
	}
	for i, expected := range []string{"healthy", "unreachable", "host_key_unverified", "locks_unverified", "", "healthy", ""} {
		got := response.Results[i]
		if got.Name != selected[i].Name || got.Identity != selected[i].Identity || got.State != expected || (got.Error != "") != (expected == "") {
			t.Errorf("result %d = %+v, want state %q", i, got, expected)
		}
	}
	stale := restoreAPTCheckTarget{Name: updated.Name, Identity: updated.Identity, TargetRevision: displayedTargetRevision}
	staleResponse := post(admin, []restoreAPTCheckTarget{stale})
	if staleResponse.Code != http.StatusOK || !strings.Contains(staleResponse.Body.String(), "SSH target changed") || len(entered) != 1 {
		t.Fatalf("stale displayed target could be probed: %d, %s", staleResponse.Code, staleResponse.Body)
	}
	if strings.Contains(rec.Body.String(), "SHA256:") {
		t.Fatal("batch response exposed host key evidence")
	}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", gin.H{"confirmation": "RESUME"}); rec.Code != http.StatusOK {
		t.Fatalf("explicit resume after batch: %d, %s", rec.Code, rec.Body)
	}
	for index, server := range servers {
		_, err := validation.authorize(t.Context(), server)
		if (err == nil) != (index == 0 || index == 5) {
			t.Errorf("post-resume admission for %s = %v", server.Name, err)
		}
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
