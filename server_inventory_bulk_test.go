package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	auditpkg "debian-updater/internal/audit"
	"debian-updater/internal/deferrals"
	healthpkg "debian-updater/internal/health"
	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	"debian-updater/internal/postchecks"
	serverpkg "debian-updater/internal/servers"
)

func bulkPreviewAPI(t *testing.T, app *testApp, cookie *http.Cookie, draft serverpkg.BulkDraft) serverpkg.BulkPreview {
	t.Helper()
	raw, _ := json.Marshal(draft)
	rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk/preview", raw)
	var p serverpkg.BulkPreview
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("preview %d %s", rec.Code, rec.Body.String())
	}
	return p
}
func TestBulkInventoryAPIThousandTargetsAuditAndNoSSH(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	rows := make([]serverpkg.ImportRow, 1000)
	names := make([]string, 1000)
	for i := range rows {
		names[i] = fmt.Sprintf("synthetic-%04d-%s", i, strings.Repeat("n", 90))
		rows[i] = serverpkg.ImportRow{ImportEntry: serverpkg.ImportEntry{Row: i + 1, Name: names[i], Host: fmt.Sprintf("bulk-%d.invalid", i), Port: 22, User: "root", Tags: []string{"old"}}}
	}
	commands := serverpkg.NewCommandService(app.Deps.ServerInventoryService)
	p, err := commands.PreviewImport(rows)
	if err != nil {
		t.Fatal(err)
	}
	selection := serverpkg.ImportSelection{BatchID: p.BatchID}
	for _, row := range rows {
		selection.Rows = append(selection.Rows, row.ImportEntry)
	}
	if result := commands.CommitImport(selection); result.Outcome != serverpkg.CommandOutcomeSuccess {
		t.Fatalf("seed %s", result.Error)
	}
	draft := serverpkg.BulkDraft{Names: names, AddTags: []string{" production ", "production"}, RemoveTags: []string{"old"}}
	preview := bulkPreviewAPI(t, app, cookie, draft)
	if !preview.CanConfirm || len(preview.Rows) != 1000 {
		t.Fatal("1000-target preview rejected")
	}
	raw, _ := json.Marshal(map[string]string{"batch_id": preview.BatchID})
	rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", raw)
	if rec.Code != 200 {
		t.Fatalf("commit %d %s", rec.Code, rec.Body.String())
	}
	if repeat := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", raw); repeat.Code != 409 {
		t.Fatal("duplicate batch accepted")
	}
	for _, s := range app.Deps.ServerState.CloneServers() {
		if !s.Disabled || len(s.Tags) != 1 || s.Tags[0] != "production" {
			t.Fatal("unexpected inventory mutation")
		}
	}
	loaded, err := newSchedulerRevisionServerRepository(app.Deps.DB, nil, nil).Load()
	if err != nil || len(loaded) != 1000 {
		t.Fatalf("durable inventory %d %v", len(loaded), err)
	}
	auditRows, err := app.Deps.DB().Query("SELECT meta_json FROM audit_events WHERE action = 'server.bulk' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer auditRows.Close()
	fragments := ""
	parts := 0
	for auditRows.Next() {
		var raw string
		if err := auditRows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		if len(raw) > auditpkg.MetaMaxLen {
			t.Fatal("audit truncated")
		}
		var meta struct {
			Fragment string `json:"changes_json_fragment"`
			Batch    string `json:"batch_id"`
		}
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		if meta.Batch != preview.BatchID {
			t.Fatal("lost batch identity")
		}
		fragments += meta.Fragment
		parts++
	}
	if err := auditRows.Err(); err != nil {
		t.Fatal(err)
	}
	var facts []struct {
		Name   string               `json:"name"`
		Before serverpkg.BulkValues `json:"before"`
		After  serverpkg.BulkValues `json:"after"`
	}
	if err := json.Unmarshal([]byte(fragments), &facts); err != nil {
		t.Fatal(err)
	}
	if len(facts) != 1000 || parts < 2 {
		t.Fatalf("audit targets=%d parts=%d", len(facts), parts)
	}
	for i, f := range facts {
		if f.Name != names[i] || len(f.Before.Tags) != 1 || f.After.Tags[0] != "production" {
			t.Fatal("lost audit before/after target")
		}
	}
}
func TestBulkInventoryAPIAtomicRollbackPolicyRevisionAndGuards(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, name := range []string{"a", "b"} {
		if _, err := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: name + ".invalid", User: "root", Disabled: true, Pass: "never-export-password", Key: "never-export-key"}); err != nil {
			t.Fatal(err)
		}
	}
	enable := false
	draft := serverpkg.BulkDraft{Names: []string{"a", "b"}, Disabled: &enable}
	// Availability is guarded even when a requested value is unchanged on a busy target.
	preview := bulkPreviewAPI(t, app, cookie, draft)
	app.Deps.ServerState.Lock()
	app.Deps.ServerState.StatusMap()["b"].ActionRunning = true
	app.Deps.ServerState.Unlock()
	commit := func(p serverpkg.BulkPreview) int {
		t.Helper()
		raw, _ := json.Marshal(map[string]string{"batch_id": p.BatchID})
		r := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", raw)
		if strings.Contains(r.Body.String(), "never-export") {
			t.Fatal("secret in response")
		}
		return r.Code
	}
	if commit(preview) != 409 {
		t.Fatal("busy target admitted")
	}
	app.Deps.ServerState.Lock()
	app.Deps.ServerState.StatusMap()["b"].ActionRunning = false
	app.Deps.ServerState.Unlock()
	preview = bulkPreviewAPI(t, app, cookie, draft)
	if _, err := app.Deps.DB().Exec("UPDATE update_policy_scheduler_state_revision SET revision = revision + 1 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if commit(preview) != 409 {
		t.Fatal("concurrent policy revision admitted")
	}
	preview = bulkPreviewAPI(t, app, cookie, draft)
	if _, err := app.Deps.DB().Exec(`CREATE TRIGGER bulk_test_fail BEFORE INSERT ON servers WHEN NEW.name='b' BEGIN SELECT RAISE(ABORT,'controlled failure'); END`); err != nil {
		t.Fatal(err)
	}
	if commit(preview) != 500 {
		t.Fatal("persistence failure not surfaced")
	}
	for _, s := range app.Deps.ServerState.CloneServers() {
		if !s.Disabled {
			t.Fatal("partial runtime availability")
		}
	}
	var enabled int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM servers WHERE disabled=0").Scan(&enabled); err != nil || enabled != 0 {
		t.Fatalf("partial database commit: %d %v", enabled, err)
	}
	if _, err := app.Deps.DB().Exec("DROP TRIGGER bulk_test_fail"); err != nil {
		t.Fatal(err)
	}
	preview = bulkPreviewAPI(t, app, cookie, draft)
	if commit(preview) != 200 {
		t.Fatal("enable failed")
	}
	for _, s := range app.Deps.ServerState.CloneServers() {
		if s.Disabled || s.Pass != "never-export-password" || s.Key != "never-export-key" {
			t.Fatal("credentials/availability changed unexpectedly")
		}
	}
	repo := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: app.Deps.DB})
	if _, err := repo.LoadSchedulerStateRevision(); err != nil {
		t.Fatal(err)
	}
}
func TestBulkInventoryAPIAuthenticationAndStrictScope(t *testing.T) {
	app := newIsolatedTestApp(t)
	for _, path := range []string{"/api/servers/bulk/preview", "/api/servers/bulk"} {
		if rec := importAPIRequest(app, nil, "POST", path, []byte(`{}`)); rec.Code != 401 {
			t.Fatalf("unauthenticated %s %d", path, rec.Code)
		}
	}
	cookie := app.authenticate(t)
	for _, raw := range []string{`{"names":["a"],"pass":"forbidden"}`, `{"names":["a"],"host":"forbidden"}`, `{"names":["a"],"key":"forbidden"}`, `{"names":["a"],"name":"forbidden"}`} {
		if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk/preview", []byte(raw)); rec.Code != 400 || strings.Contains(rec.Body.String(), "forbidden") {
			t.Fatalf("invalid scope %d %s", rec.Code, rec.Body.String())
		}
	}
}

func TestBulkInventoryPreservesAcceptedRolloutAndProjectsFutureMatchingAndBlackouts(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, name := range []string{"a", "b"} {
		if _, err := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: name + ".invalid", User: "root", Tags: []string{"prod"}}); err != nil {
			t.Fatal(err)
		}
	}
	repo := app.Deps.PolicyRepository
	policy, err := repo.CreatePolicy(UpdatePolicy{Name: "accepted rollout", Enabled: true, TargetTag: "prod", CadenceKind: policypkg.CadenceDaily, TimeLocal: "03:00", ExecutionMode: policypkg.ExecutionAutoApply, PackageScope: policypkg.PackageScopeSecurity, RolloutMode: policypkg.RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	occurrenceRepo, ok := repo.(policypkg.OccurrenceStore)
	if !ok {
		t.Fatal("occurrence store unavailable")
	}
	origin := app.Deps.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute).Format(policypkg.DefaultTimestampLayout)
	accepted, err := occurrenceRepo.AcceptOccurrence(policypkg.Occurrence{Policy: policy, Key: "accepted-before-bulk", ScheduledForUTC: origin, Batches: []policypkg.RolloutBatch{{Index: 0, Stage: "canary", Servers: []string{"a"}}, {Index: 1, Stage: "wave", ReleaseDelayMinutes: 5, Servers: []string{"b"}}}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := occurrenceRepo.ListOpenOccurrences()
	if err != nil {
		t.Fatal(err)
	}
	future, err := repo.CreatePolicy(UpdatePolicy{Name: "future tag schedule", Enabled: true, TargetTag: "future", CadenceKind: policypkg.CadenceDaily, TimeLocal: "01:00", ExecutionMode: policypkg.ExecutionAutoApply, PackageScope: policypkg.PackageScopeSecurity})
	if err != nil {
		t.Fatal(err)
	}
	preview := bulkPreviewAPI(t, app, cookie, serverpkg.BulkDraft{Names: []string{"a", "b"}, AddTags: []string{"future"}, RemoveTags: []string{"prod"}})
	for _, row := range preview.Rows {
		if len(row.BeforeSchedule) == 0 || len(row.AfterSchedule) == 0 {
			t.Fatal("missing canonical schedule facts")
		}
	}
	body, _ := json.Marshal(map[string]string{"batch_id": preview.BatchID})
	if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", body); rec.Code != 200 {
		t.Fatalf("commit %d %s", rec.Code, rec.Body.String())
	}
	after, err := occurrenceRepo.ListOpenOccurrences()
	if err != nil || !reflect.DeepEqual(before, after) || !reflect.DeepEqual(accepted.Batches, after[0].Batches) {
		t.Fatalf("accepted manifests changed: %v", err)
	}
	for _, server := range app.Deps.ServerState.CloneServers() {
		if !app.Deps.PolicyService.PolicyMatchesServer(future, server, policypkg.MatchContext{}) || app.Deps.PolicyService.PolicyMatchesServer(policy, server, policypkg.MatchContext{}) {
			t.Fatal("future matching ignored current tags")
		}
	}
	// No-run windows affect canonical schedule facts, while inventory activation
	// remains an administrative mutation and never admits a maintenance job.
	if _, err := repo.SaveGlobalBlackouts([]policypkg.BlackoutWindow{{Weekdays: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, StartTime: "00:00", EndTime: "12:00"}, {Weekdays: []string{"mon", "tue", "wed", "thu", "fri", "sat", "sun"}, StartTime: "12:00", EndTime: "00:00"}}); err != nil {
		t.Fatal(err)
	}
	disabled := true
	preview = bulkPreviewAPI(t, app, cookie, serverpkg.BulkDraft{Names: []string{"a", "b"}, Disabled: &disabled})
	var facts policypkg.ServerScheduleProjection
	if err := json.Unmarshal(preview.Rows[0].BeforeSchedule, &facts); err != nil || !facts.NoRun.Active {
		t.Fatalf("blackout missing from canonical projection: %+v %v", facts, err)
	}
	body, _ = json.Marshal(map[string]string{"batch_id": preview.BatchID})
	if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", body); rec.Code != 200 {
		t.Fatal("disable during blackout failed")
	}
	enabled := false
	preview = bulkPreviewAPI(t, app, cookie, serverpkg.BulkDraft{Names: []string{"a", "b"}, Disabled: &enabled})
	if err := json.Unmarshal(preview.Rows[0].AfterSchedule, &facts); err != nil || !facts.NoRun.Active {
		t.Fatal("activation preview bypassed blackout")
	}
	var jobs int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("bulk command created maintenance jobs: %d %v", jobs, err)
	}
}
func TestBulkInventoryUsesSameOriginAndRestoreMaintenanceGate(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	req := httptest.NewRequest("POST", "/api/servers/bulk/preview", strings.NewReader(`{"names":["a"],"add_tags":["x"]}`))
	req.AddCookie(cookie)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://foreign.invalid")
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-origin %d", rec.Code)
	}
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("restore lease unavailable")
	}
	defer lease.Close()
	if err := lease.Activate(context.Background(), maintenancepkg.OperationFacts{Actor: "test", Message: "controlled restore"}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/servers/bulk/preview", "/api/servers/bulk"} {
		if rec := importAPIRequest(app, cookie, "POST", path, []byte(`{}`)); rec.Code != 503 {
			t.Fatalf("restore admitted %s: %d", path, rec.Code)
		}
	}
}

func TestBulkInventoryPreservesAuxiliaryFactsAndTrust(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	now := app.Deps.Now().UTC()
	if _, err := app.Deps.ServerInventoryService.Create(Server{Name: "a", Host: "a.invalid", User: "root", Tags: []string{"old"}, Pass: "never-export-password", Key: "never-export-key"}); err != nil {
		t.Fatal(err)
	}
	if _, err := deferrals.Create(context.Background(), app.Deps.DB(), deferrals.Rule{ServerName: "a", Selector: "openssl", Reason: "controlled deferral", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}, "account:1", now); err != nil {
		t.Fatal(err)
	}
	config := postchecks.Config{Profiles: []postchecks.Profile{{ID: "web", Name: "Web", TargetServers: []string{"a"}, Checks: []postchecks.Check{{ID: "health", Name: "Health", Kind: "http_status", URL: "https://localhost/health", ExpectedStatus: 200, TimeoutSeconds: 10, IntervalSeconds: 1, Mode: "blocking"}}}}}
	if _, err := postchecks.Save(context.Background(), app.Deps.DB(), config); err != nil {
		t.Fatal(err)
	}
	if err := (healthpkg.SQLiteObservation{DB: app.Deps.DB}).AcceptCollectedFacts(healthpkg.CollectedFacts{ServerName: "a", CollectedAt: now.Format(time.RFC3339), OSPrettyName: "Disposable fixture"}); err != nil {
		t.Fatal(err)
	}
	policy, err := app.Deps.PolicyRepository.CreatePolicy(UpdatePolicy{Name: "preserved", TargetTag: "old"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.PolicyRepository.SetOverride(policy.ID, "a", true); err != nil {
		t.Fatal(err)
	}
	marker := []byte("# controlled disposable trust fixture\n")
	if err := os.WriteFile(app.KnownHostsPath, marker, 0600); err != nil {
		t.Fatal(err)
	}
	tables := []string{"package_deferrals", "package_deferral_snapshots", "application_postchecks", "server_facts", "server_health_snapshots", "update_policy_overrides", "update_policy_runs", "update_policy_occurrences"}
	snapshot := func() map[string]string {
		t.Helper()
		result := map[string]string{}
		for _, table := range tables {
			rows, err := app.Deps.DB().Query("SELECT * FROM " + table)
			if err != nil {
				t.Fatal(err)
			}
			columns, err := rows.Columns()
			if err != nil {
				t.Fatal(err)
			}
			values := [][]any{}
			for rows.Next() {
				row := make([]any, len(columns))
				args := make([]any, len(columns))
				for i := range row {
					args[i] = &row[i]
				}
				if err := rows.Scan(args...); err != nil {
					t.Fatal(err)
				}
				values = append(values, row)
			}
			if err := rows.Err(); err != nil {
				t.Fatal(err)
			}
			rows.Close()
			raw, _ := json.Marshal(values)
			result[table] = string(raw)
		}
		return result
	}
	before := snapshot()
	disabled := true
	preview := bulkPreviewAPI(t, app, cookie, serverpkg.BulkDraft{Names: []string{"a"}, AddTags: []string{"new"}, RemoveTags: []string{"old"}, Disabled: &disabled})
	raw, _ := json.Marshal(map[string]string{"batch_id": preview.BatchID})
	if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", raw); rec.Code != 200 {
		t.Fatalf("commit %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(before, snapshot()) {
		t.Fatal("bulk mutation changed preserved auxiliary facts")
	}
	trust, err := os.ReadFile(app.KnownHostsPath)
	if err != nil || string(trust) != string(marker) {
		t.Fatal("bulk mutation changed trust")
	}
	var actor string
	if err := app.Deps.DB().QueryRow("SELECT actor FROM audit_events WHERE action='server.bulk'").Scan(&actor); err != nil || actor != "account:1" {
		t.Fatalf("audit actor %q %v", actor, err)
	}
}

func TestBulkInventoryActualCommitFailureRollsBackDatabaseRuntimeAndRevision(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, name := range []string{"a", "b"} {
		if _, err := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: name + ".invalid", User: "root", Disabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	disabled := false
	draft := serverpkg.BulkDraft{Names: []string{"a", "b"}, Disabled: &disabled}
	preview := bulkPreviewAPI(t, app, cookie, draft)
	// A deferred FK violation permits every INSERT and the revision hook to
	// finish, then fails SQLite COMMIT itself. The test database is disposable.
	for _, statement := range []string{
		"PRAGMA foreign_keys=ON",
		"CREATE TABLE bulk_commit_guard (target TEXT REFERENCES servers(name) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER bulk_commit_failure AFTER INSERT ON servers BEGIN INSERT INTO bulk_commit_guard(target) VALUES ('missing'); END",
	} {
		if _, err := app.Deps.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	repository := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: app.Deps.DB})
	revision, err := repository.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatal(err)
	}
	before := app.Deps.ServerState.CloneServers()
	statuses := app.Deps.ServerState.CloneStatusMap()
	body, _ := json.Marshal(map[string]string{"batch_id": preview.BatchID})
	if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", body); rec.Code != 500 {
		t.Fatalf("COMMIT failure %d %s", rec.Code, rec.Body.String())
	}
	if !reflect.DeepEqual(before, app.Deps.ServerState.CloneServers()) || !reflect.DeepEqual(statuses, app.Deps.ServerState.CloneStatusMap()) {
		t.Fatal("COMMIT failure changed runtime")
	}
	stored, err := (serverpkg.SQLiteRepository{DB: app.Deps.DB, Decrypt: decryptSecret}).Load()
	if err != nil || !reflect.DeepEqual(before, stored) {
		t.Fatalf("COMMIT failure changed database: %v", err)
	}
	afterRevision, err := repository.LoadSchedulerStateRevision()
	if err != nil || afterRevision != revision {
		t.Fatalf("COMMIT failure retained generation increment: %d -> %d, %v", revision, afterRevision, err)
	}
	var guards int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM bulk_commit_guard").Scan(&guards); err != nil || guards != 0 {
		t.Fatal("COMMIT failure retained writes")
	}
	if _, err := app.Deps.DB().Exec("DROP TRIGGER bulk_commit_failure"); err != nil {
		t.Fatal(err)
	}
	fresh := bulkPreviewAPI(t, app, cookie, draft)
	body, _ = json.Marshal(map[string]string{"batch_id": fresh.BatchID})
	if rec := importAPIRequest(app, cookie, "POST", "/api/servers/bulk", body); rec.Code != 200 {
		t.Fatalf("connection not reusable after failed COMMIT: %d %s", rec.Code, rec.Body.String())
	}
}
