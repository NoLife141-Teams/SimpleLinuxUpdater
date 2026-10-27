package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	authpkg "debian-updater/internal/auth"
	installationhealthpkg "debian-updater/internal/installationhealth"
	maintenancepkg "debian-updater/internal/maintenance"
)

func TestInstallationProbesCurrentProviderLifecycleAndBudget(t *testing.T) {
	app := newIsolatedTestApp(t)
	// Embedded Gin compositions run probes from their no-route middleware too;
	// explicit success status must replace Gin's provisional 404, including HEAD.
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		for _, path := range []string{"/healthz", "/readyz"} {
			rec := httptest.NewRecorder()
			app.Router.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("embedded %s %s = %d %s", method, path, rec.Code, rec.Body.String())
			}
		}
	}
	current := app.Deps.DB()
	calls := 0
	deps := app.Deps
	deps.DB = func() *sql.DB { calls++; return current }
	handler := installationProbeHandler(deps, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("probe reached session/application handler") }))
	probe := func(path, state string, status int) {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Authorization", "Bearer secret-internal-url")
		req.AddCookie(&http.Cookie{Name: "session", Value: "expired"})
		handler.ServeHTTP(rec, req)
		if rec.Code != status || strings.TrimSpace(rec.Body.String()) != `{"status":"`+state+`"}` {
			t.Fatalf("probe %s: %d %s", path, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatal("probe cache/security headers missing")
		}
	}
	probe("/readyz", "ready", 200) // Before account setup.
	initialCalls := calls
	for _, phase := range []string{"initializing", "reloading"} {
		deps.InstallationHealth.setPhase(phase)
		probe("/readyz", phase, 503)
		probe("/healthz", "alive", 200)
	}
	if calls != initialCalls {
		t.Fatal("initialization/reload queried persistence")
	}
	deps.InstallationHealth.setPhase("ready")
	lease, decision := deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("exclusive lease")
	}
	if err := lease.Activate(context.Background(), maintenancepkg.OperationFacts{Actor: "SECRET", Message: "/private/database.sqlite"}); err != nil {
		t.Fatal(err)
	}
	probe("/readyz", "maintenance", 503)
	probe("/healthz", "alive", 200)
	if calls != initialCalls {
		t.Fatal("maintenance probe queried persistence")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	current = nil
	probe("/readyz", "unavailable", 503)
	current = app.Deps.DB()
	current.SetMaxOpenConns(1)
	conn, err := current.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	probe("/readyz", "unavailable", 503)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("probe exceeded budget: %s", elapsed)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	probe("/readyz", "ready", 200)
	// A replacement generation, not a captured handle, is checked on every probe.
	replacement, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	current = replacement
	probe("/readyz", "ready", 200)
	replacement.Close()
	probe("/readyz", "unavailable", 503)
	deps.InstallationHealth.setPhase("stopping")
	deps.InstallationHealth.setPhase("ready")
	probe("/readyz", "stopping", 503)
	probe("/healthz", "alive", 200)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("HEAD", "/healthz", nil))
	if rec.Code != 200 || rec.Body.Len() != 0 {
		t.Fatal("HEAD probe")
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/readyz", nil))
	if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Fatal("probe method")
	}
}

func TestInstallationDiagnosticsAndMetricsBoundaries(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	for _, role := range []string{"reader", "operator"} {
		a, err := app.Deps.AuthService.CreateAccount(role, testPasswordStrong, role)
		if err != nil {
			t.Fatal(err)
		}
		cookie := identityLogin(t, app, role)
		if rec := identityRequest(app, "GET", "/api/installation/health", cookie, "", nil); rec.Code != 403 {
			t.Fatalf("%s diagnostics = %d", role, rec.Code)
		}
		_, token, err := app.Deps.AuthService.CreatePersonalToken(a.ID, "test", authpkg.PersonalScopesForRole(role), time.Now().Add(time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if rec := identityRequest(app, "GET", "/api/installation/health", nil, token, nil); rec.Code != 401 {
			t.Fatalf("bearer diagnostics = %d", rec.Code)
		}
	}
	if rec := identityRequest(app, "GET", "/api/installation/health", nil, "", nil); rec.Code != 401 {
		t.Fatalf("anonymous diagnostics = %d", rec.Code)
	}
	rec := identityRequest(app, "GET", "/api/installation/health", admin, "", nil)
	if rec.Code != 200 || rec.Header().Get("Cache-Control") == "" {
		t.Fatalf("admin diagnostics = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), app.DBPath) || strings.Contains(rec.Body.String(), "webhook_url") {
		t.Fatal("diagnostic leaked private facts")
	}
	if rec := identityRequest(app, "GET", "/metrics", admin, "", nil); rec.Code == 200 {
		t.Fatal("cookie admitted metrics")
	}
	token, err := app.Deps.MetricsAccessCredential.Rotate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	rec = identityRequest(app, "GET", "/metrics", nil, token, nil)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "slu_installation_ready 1") || !strings.Contains(rec.Body.String(), "# HELP simplelinuxupdater_") {
		t.Fatalf("metrics = %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), token) || strings.Contains(rec.Body.String(), app.DBPath) {
		t.Fatal("metrics leaked private facts")
	}
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupExport)
	if !decision.Allowed {
		t.Fatal("maintenance lease")
	}
	defer lease.Close()
	if err := lease.Activate(context.Background(), maintenancepkg.OperationFacts{}); err != nil {
		t.Fatal(err)
	}

	for _, cookie := range []*http.Cookie{nil, admin} {
		response := identityRequest(app, "GET", "/api/installation/health", cookie, "", nil)
		if response.Code != 503 || strings.TrimSpace(response.Body.String()) != `{"status":"maintenance"}` {
			t.Fatalf("maintenance diagnostics %d %s", response.Code, response.Body.String())
		}
	}
	rec = identityRequest(app, "GET", "/metrics", nil, token, nil)
	if rec.Code != 503 || strings.TrimSpace(rec.Body.String()) != `{"status":"maintenance"}` {
		t.Fatalf("maintenance metrics %d %s", rec.Code, rec.Body.String())
	}

}

func TestInstallationMetricsBoundedVocabularyAndUnknowns(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	value := installationDiagnostics{CheckedAt: now, State: "ready", Workers: map[string]installationhealthpkg.WorkerSnapshot{"scheduler": {Enabled: true, LastTick: now, Errors: 2}, "secret/url/path": {Enabled: true}}}
	body := installationMetrics(value)
	if strings.Contains(body, "secret") || strings.Contains(body, "backup_verified_age_seconds") || strings.Contains(body, "notifications_pending ") || strings.Contains(body, "worker_last_progress_timestamp_seconds{") {
		t.Fatal(body)
	}
	if !strings.Contains(body, `worker="scheduler"`) || !strings.Contains(body, "slu_installation_worker_errors{worker=\"scheduler\"} 2") {
		t.Fatal(body)
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, "{") && !strings.Contains(line, `{worker="scheduler"}`) {
			t.Fatal("unbounded labels: " + line)
		}
	}
}

func TestInstallationReadOnlyMaintenanceProbeNeverRecovers(t *testing.T) {
	store := maintenancepkg.NewMemoryStore()
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: store})
	lease, _ := coordinator.TryExclusive(maintenancepkg.OperationBackupExport)
	if err := lease.Activate(context.Background(), maintenancepkg.OperationFacts{}); err != nil {
		t.Fatal(err)
	}
	store.SaveError = errors.New("private SQL path")
	if err := lease.Close(); err == nil {
		t.Fatal("expected pending release")
	}
	store.SaveError = nil
	for range 3 {
		lease, state, allowed := coordinator.TryProbe()
		if lease != nil || allowed || !state.Active {
			t.Fatal("probe repaired maintenance")
		}
	}
	state, err := store.Load(context.Background())
	if err != nil || !state.Active {
		t.Fatal("probe wrote maintenance persistence")
	}
}

func TestInstallationLocalProbeTargets(t *testing.T) {
	for _, tc := range []struct{ address, want string }{{"", "http://127.0.0.1:8080/readyz"}, {":9123", "http://127.0.0.1:9123/readyz"}, {"0.0.0.0:9123", "http://127.0.0.1:9123/readyz"}, {"[::]:9123", "http://[::1]:9123/readyz"}, {"[::1]:9123", "http://[::1]:9123/readyz"}, {"127.0.0.2:9123", "http://127.0.0.2:9123/readyz"}} {
		t.Run(tc.address, func(t *testing.T) {
			got, err := localProbeURL(func(string) string { return tc.address })
			if err != nil || got != tc.want {
				t.Fatalf("%s %v", got, err)
			}
		})
	}
}

func TestInstallationSnapshotCanonicalEvidenceClockAndCounts(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	app := newTestAppWithDeps(t, t.TempDir()+"/app.db", AppDeps{Now: func() time.Time { return now }})
	// Seed only this disposable installation's durable facts. The read path must
	// not wake delivery, decrypt a destination, or validate any backup destination.
	for _, state := range []string{"pending", "claimed", "retrying", "failed", "succeeded"} {
		_, err := app.Deps.DB().Exec(`INSERT INTO notification_outbox(id,destination,event_type,action,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?)`, state, "webhook", "test", "test", state, now.Format(time.RFC3339), now.Format(time.RFC3339))
		if err != nil {
			t.Fatal(err)
		}
	}
	verified := now.Add(-time.Hour)
	_, err := app.Deps.DB().Exec(`INSERT INTO audit_events(created_at,actor,client_ip,action,target_type,target_name,status,message,meta_json) VALUES(?,?,?,?,?,?,?,?,?)`, verified.Format(jobTimestampLayout), "operator", "", "backup.verify", "backup", "test", "success", "verified", `{"restore_ready":true,"archive_size_bytes":42}`)
	if err != nil {
		t.Fatal(err)
	}
	value := installationSnapshot(context.Background(), app.Deps)
	if !value.CheckedAt.Equal(now) || !value.Notifications.Available || value.Notifications.Pending != 2 || value.Notifications.Retrying != 1 || value.Notifications.Failed != 1 || value.Notifications.Enabled {
		t.Fatalf("canonical outbox facts: %+v", value)
	}
	if value.VerifiedBackupAgeSeconds == nil || *value.VerifiedBackupAgeSeconds != 3600 || value.LastVerifiedBackupAt == nil || !value.LastVerifiedBackupAt.Equal(verified) {
		t.Fatalf("canonical verification age: %+v", value)
	}
	if value.Storage.DBBytes == nil || value.Storage.WALBytes == nil {
		t.Fatal("missing local DB/WAL observations")
	}
	if len(value.Workers) != 7 {
		t.Fatalf("workers %+v", value.Workers)
	}
	if err := app.Deps.BackupAutomation.Tick(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	observed := app.Deps.BackupAutomation.health.Snapshot(false)
	if observed.Enabled || !observed.LastTick.Equal(now) || !observed.LastProgress.Equal(now) {
		t.Fatal(observed)
	}
	now = now.Add(time.Hour)
	if err := app.Deps.AuditService.Record("system", "", "backup.verify", "backup", "test", "failure", "rejected", nil); err != nil {
		t.Fatal(err)
	}
	value = installationSnapshot(context.Background(), app.Deps)
	if value.VerifiedBackupAgeSeconds == nil || *value.VerifiedBackupAgeSeconds != 7200 {
		t.Fatal("failed verification replaced previous success")
	}
}

func TestInstallationReloadReadinessTracksOwnerAndRetainedRecovery(t *testing.T) {
	app := newIsolatedTestApp(t)
	composition := newRuntimeComposition(app.Deps)
	composition.resetCaches = func() {}
	if err := composition.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := app.Deps.InstallationHealth.probe(context.Background(), app.Deps); got != "reloading" {
		t.Fatal(got)
	}
	if err := composition.ReloadRestoredState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := app.Deps.InstallationHealth.probe(context.Background(), app.Deps); got != "ready" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := composition.ReloadRestoredState(ctx); err == nil {
		t.Fatal("expected failed reload")
	}
	if got := app.Deps.InstallationHealth.Phase(); got != "reloading" {
		t.Fatal(got)
	}
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("lease")
	}
	if err := lease.Activate(context.Background(), maintenancepkg.OperationFacts{}); err != nil {
		t.Fatal(err)
	}
	if err := lease.RetainForRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := app.Deps.InstallationHealth.probe(context.Background(), app.Deps); got != "recovery_required" {
		t.Fatal(got)
	}
}

func TestInstallationProbeCommandUsesHTTPWithoutRedirectOrInstallation(t *testing.T) {
	var hits atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.URL.Path != "/readyz" {
			t.Errorf("wrong probe path %s", r.URL.Path)
		}
		w.WriteHeader(200)
	}))
	defer server.Close()
	env := func(string) string { return strings.TrimPrefix(server.URL, "http://") }
	if handled, code := runInstallationProbeCommand([]string{"healthcheck"}, env); !handled || code != 0 || hits.Load() != 1 {
		t.Fatalf("handled=%v code=%d hits=%d", handled, code, hits.Load())
	}
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, server.URL, http.StatusFound) }))
	defer redirected.Close()
	if _, code := runInstallationProbeCommand([]string{"healthcheck"}, func(string) string { return strings.TrimPrefix(redirected.URL, "http://") }); code != 1 || hits.Load() != 1 {
		t.Fatal("probe followed a redirect")
	}
	if handled, _ := runInstallationProbeCommand([]string{"other"}, env); handled {
		t.Fatal("claimed unrelated subcommand")
	}
	if _, code := runInstallationProbeCommand([]string{"healthcheck", "unexpected"}, env); code != 1 {
		t.Fatal("accepted extra args")
	}
	if _, code := runInstallationProbeCommand([]string{"healthcheck"}, func(string) string { return "invalid" }); code != 1 {
		t.Fatal("accepted malformed address")
	}
}
