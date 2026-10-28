package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

func publishedCatalogFixture(t *testing.T) (*testApp, *backupAutomation, *http.Cookie, ownedBackupArchive) {
	t.Helper()
	app, a, _ := newBackupAutomationTest(t)
	cookie := app.authenticate(t)
	if _, err := app.Deps.DB().Exec("INSERT INTO servers(name,host,port,user,pass_enc,key_enc,key_path,tags) VALUES('disposable-catalog-host','invalid.example',22,'root','','','','')"); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, a)
	if len(state.Archives) != 1 {
		t.Fatalf("archives=%d", len(state.Archives))
	}
	return app, a, cookie, state.Archives[0]
}
func catalogRequest(app *testApp, cookie *http.Cookie, id, operation string) *httptest.ResponseRecorder {
	return performContractRequest(app.Handler, http.MethodPost, "/api/backup/archives/"+id+"/"+operation, nil, cookie, true)
}
func catalogFacts(t *testing.T, app *testApp, cookie *http.Cookie) backupArchiveCatalog {
	t.Helper()
	response := performContractRequest(app.Handler, http.MethodGet, "/api/backup/archives", nil, cookie, false)
	if response.Code != 200 {
		t.Fatalf("catalog status=%d body=%s", response.Code, response.Body.String())
	}
	var result backupArchiveCatalog
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(response.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("catalog cached")
	}
	return result
}
func catalogProtectedState(t *testing.T, app *testApp) string {
	t.Helper()
	// Tracking/audit/session writes are expected; active inventory, identity,
	// other settings and files must not be replaced by the verification pipeline.
	queries := []string{"SELECT * FROM servers ORDER BY name", "SELECT * FROM auth_users ORDER BY id", "SELECT * FROM settings WHERE key != 'backup_automation_v1' ORDER BY key"}
	var result [][]any
	for _, query := range queries {
		rows, err := app.Deps.DB().Query(query)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			result = append(result, values)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	for _, path := range []string{configPath(), app.KnownHostsPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, []any{data})
	}
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestBackupCatalogPublishedRoundTripReadOnlyAndAudit(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	before := catalogProtectedState(t, app)
	database := app.Deps.DB()
	facts := catalogFacts(t, app, cookie)
	if len(facts.Archives) != 1 {
		t.Fatalf("catalog=%+v", facts)
	}
	item := facts.Archives[0]
	if item.ID == "" || item.PublishedAt == "" || item.LastVerifiedAt == "" || item.SizeBytes == nil || item.KnownHostsIncluded == nil || item.Verification != "verified" || item.Availability != "present" || item.JobID == "" {
		t.Fatalf("missing publication facts: %+v", item)
	}
	original, err := os.ReadFile(filepath.Join(a.deployment.Directory, archive.Name))
	if err != nil {
		t.Fatal(err)
	}
	downloaded := catalogRequest(app, cookie, item.ID, "download")
	if downloaded.Code != 200 || !bytes.Equal(downloaded.Body.Bytes(), original) {
		t.Fatalf("download=%d bytes=%d", downloaded.Code, downloaded.Body.Len())
	}
	if downloaded.Header().Get("Content-Disposition") != `attachment; filename="simplelinuxupdater-automated-backup.slubkp"` || !strings.Contains(downloaded.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("download headers=%v", downloaded.Header())
	}
	verified := catalogRequest(app, cookie, item.ID, "verify")
	if verified.Code != 200 || !strings.Contains(verified.Body.String(), `"restore_ready":true`) || !strings.Contains(verified.Body.String(), `"servers":1`) {
		t.Fatalf("verify=%d %s", verified.Code, verified.Body.String())
	}
	if app.Deps.DB() != database || catalogProtectedState(t, app) != before {
		t.Fatal("verification replaced or mutated active inventory, identity, settings, config or known_hosts")
	}
	for _, response := range []*httptest.ResponseRecorder{verified, performContractRequest(app.Handler, "GET", "/api/backup/archives", nil, cookie, false)} {
		for _, sensitive := range []string{a.deployment.Directory, a.deployment.SecretFile, archive.Name, archive.Owner, "a separate automated backup secret"} {
			if strings.Contains(response.Body.String(), sensitive) {
				t.Fatal("sensitive fact leaked")
			}
		}
	}
	var metadata string
	if err := database.QueryRow("SELECT meta_json FROM audit_events WHERE action='backup.download' ORDER BY id DESC LIMIT 1").Scan(&metadata); err != nil || !strings.Contains(metadata, item.ID) {
		t.Fatalf("download audit: %s %v", metadata, err)
	}
	var downloadMeta map[string]any
	if err := json.Unmarshal([]byte(metadata), &downloadMeta); err != nil {
		t.Fatal(err)
	}
	if downloadMeta["hash_valid"] != true {
		t.Fatalf("download integrity evidence missing: %s", metadata)
	}
	if _, claimedReadiness := downloadMeta["restore_ready"]; claimedReadiness {
		t.Fatal("download falsely claims restore readiness")
	}
	rows, err := database.Query("SELECT meta_json FROM audit_events WHERE action='backup.verify'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		if err := rows.Scan(&metadata); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(metadata, archive.Name) || strings.Contains(metadata, a.deployment.Directory) || strings.Contains(metadata, "separate automated") {
			t.Fatal("verification audit leaked private facts")
		}
	}
	// Rehydrated automation remains explicitly suspended even after successful
	// catalog use. No implicit Configure/create-identity action is allowed.
	if err := a.SuspendAfterRestore(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rec := catalogRequest(app, cookie, item.ID, "verify"); rec.Code != 200 {
		t.Fatalf("suspended verification=%d", rec.Code)
	}
	state := backupAutomationStateForTest(t, a)
	if state.Approval != "" || state.Outcome != "suspended" {
		t.Fatal("catalog rearmed restored automation")
	}
}

func TestBackupCatalogRejectsReplacedCorruptSymlinkMissingAndForeign(t *testing.T) {
	for _, kind := range []string{"same-size corruption", "replacement after listing", "identical replacement", "symlink", "missing", "wrong owner", "wrong hash", "missing secret", "wrong secret", "missing identity"} {
		t.Run(kind, func(t *testing.T) {
			app, a, cookie, archive := publishedCatalogFixture(t)
			id := catalogFacts(t, app, cookie).Archives[0].ID
			before := catalogProtectedState(t, app)
			path := filepath.Join(a.deployment.Directory, archive.Name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			externalOriginal := append([]byte(nil), original...)
			external := filepath.Join(t.TempDir(), "foreign.slubkp")
			if err := os.WriteFile(external, original, 0600); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "same-size corruption":
				original[len(original)/2] ^= 1
				err = os.WriteFile(path, original, 0600)
			case "replacement after listing":
				err = os.Rename(path, path+".old")
				if err == nil {
					err = os.WriteFile(path, []byte("foreign replacement"), 0600)
				}
			case "identical replacement":
				err = os.Rename(path, path+".old")
				if err == nil {
					err = os.WriteFile(path, original, 0600)
				}
			case "symlink":
				err = os.Remove(path)
				if err == nil {
					err = os.Symlink(external, path)
				}
			case "missing":
				err = os.Remove(path)
			case "wrong owner":
				state := backupAutomationStateForTest(t, a)
				state.Archives[0].Owner = "another-installation"
				id = state.Archives[0].ID
				err = a.save(context.Background(), state)
			case "wrong hash":
				state := backupAutomationStateForTest(t, a)
				state.Archives[0].SHA256 = strings.Repeat("0", 64)
				err = a.save(context.Background(), state)
			case "missing secret":
				err = os.Remove(a.deployment.SecretFile)
			case "wrong secret":
				err = os.WriteFile(a.deployment.SecretFile, []byte("a different deployment secret"), 0600)
			case "missing identity":
				err = os.Remove(app.DBPath + ".backup-instance")
			}
			if err != nil {
				t.Fatal(err)
			}
			operations := []string{"download", "verify"}
			if kind == "wrong secret" {
				operations = []string{"verify"}
			}
			for _, operation := range operations {
				rec := catalogRequest(app, cookie, id, operation)
				if rec.Code < 400 {
					t.Fatalf("%s accepted %s: %d", operation, kind, rec.Code)
				}
				for _, secret := range []string{path, archive.Owner, a.deployment.SecretFile, "different deployment secret"} {
					if strings.Contains(rec.Body.String(), secret) {
						t.Fatal("refusal leaks secret or path")
					}
				}
			}
			if catalogProtectedState(t, app) != before {
				t.Fatal("failed catalog command mutated active data")
			}
			if externalData, err := os.ReadFile(external); err != nil || !bytes.Equal(externalData, externalOriginal) {
				t.Fatal("foreign file removed")
			}
			if kind == "same-size corruption" || kind == "missing" || kind == "replacement after listing" {
				health, err := app.Deps.BackupService.RecoveryHealth(context.Background())
				if err != nil || health.Verification.State != internalbackup.RecoveryStateFailed {
					t.Fatalf("damage did not update recovery facts: %+v %v", health, err)
				}
				source := newNotificationConditionSource(app.Deps.DB, a.deps.Now)
				batch, err := source(context.Background(), "", nil)
				if err != nil {
					t.Fatal(err)
				}
				if !lastConditionFact(batch.Facts, "backup/verify").Active {
					t.Fatal("existing backup notification condition not updated")
				}
			}
		})
	}
}

func TestBackupCatalogLegacyMetadataAndForeignOwnershipRemainUnknown(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	state := backupAutomationStateForTest(t, a)
	// Serialize the exact legacy row, without any new metadata.
	state.Archives = []ownedBackupArchive{{Name: archive.Name, SHA256: archive.SHA256, Owner: archive.Owner}, {Name: "slu-auto-foreign-legacy.slubkp", SHA256: archive.SHA256, Owner: "foreign"}}
	if err := a.save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	catalog := catalogFacts(t, app, cookie)
	legacy := catalog.Archives[1]
	if legacy.PublishedAt != "" || legacy.LastVerifiedAt != "" || legacy.SizeBytes != nil || legacy.KnownHostsIncluded != nil || legacy.Verification != "unknown" || legacy.Availability != "present" {
		t.Fatalf("invented metadata: %+v", legacy)
	}
	if foreign := catalog.Archives[0]; foreign.Availability != "foreign" {
		t.Fatalf("foreign row available: %+v", foreign)
	}
	firstID := legacy.ID
	// Restart-style composition produces exactly the same ID, with no mtime
	// inference or approval and ownership changes.
	restarted := newBackupAutomation(app.Deps)
	restarted.deployment = a.deployment
	status, err := restarted.Status(context.Background())
	if err != nil || status.Catalog.Archives[1].ID != firstID {
		t.Fatal("unstable legacy ID")
	}
	if rec := catalogRequest(app, cookie, firstID, "verify"); rec.Code != 200 {
		t.Fatalf("legacy verify=%d %s", rec.Code, rec.Body.String())
	}
	after := catalogFacts(t, app, cookie).Archives[1]
	if after.PublishedAt != "" || after.LastVerifiedAt == "" || after.SizeBytes == nil || after.KnownHostsIncluded == nil {
		t.Fatalf("additive verification metadata=%+v", after)
	}
	persisted := backupAutomationStateForTest(t, a)
	if persisted.Archives[0].Owner != archive.Owner || persisted.Archives[0].Name != archive.Name || persisted.Archives[0].SHA256 != archive.SHA256 {
		t.Fatal("legacy ownership changed")
	}
	for i := 0; i < 120; i++ {
		state.Archives = append(state.Archives, ownedBackupArchive{Name: "foreign", Owner: "foreign"})
	}
	if err := a.save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	bounded := catalogFacts(t, app, cookie)
	if len(bounded.Archives) != 100 || !bounded.Truncated {
		t.Fatal("unbounded catalog")
	}
}

func TestBackupCatalogUnknownIDsAndSensitiveCookieAdmission(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	for _, id := range []string{"unknown", "..%2F..%2Fconfig.json", strings.Repeat("f", 64), archive.ID + "%00"} {
		if rec := catalogRequest(app, cookie, id, "download"); rec.Code < 400 {
			t.Fatalf("invalid ID accepted: %d", rec.Code)
		}
	}
	id := archive.ID
	for _, operation := range []string{"download", "verify"} {
		if rec := catalogRequest(app, nil, id, operation); rec.Code != 401 {
			t.Fatalf("anonymous %s=%d", operation, rec.Code)
		}
	}
	account, err := app.Deps.AuthService.AccountByName("admin")
	if err != nil {
		t.Fatal(err)
	}
	_, token, err := app.Deps.AuthService.CreatePersonalToken(account.ID, "catalog must reject bearer", []string{"operational:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"download", "verify"} {
		req := httptest.NewRequest("POST", "/api/backup/archives/"+id+"/"+operation, nil)
		markSameOriginAuthRequest(req)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		if rec.Code != 401 {
			t.Fatalf("bearer %s=%d", operation, rec.Code)
		}
	}
	if _, err := app.Deps.AuthService.CreateAccount("catalog-reader", testPasswordStrong, "reader"); err != nil {
		t.Fatal(err)
	}
	login := performContractRequest(app.Handler, "POST", "/api/auth/login", bytes.NewBufferString(`{"username":"catalog-reader","password":"`+testPasswordStrong+`"}`), nil, true)
	if login.Code != 200 {
		t.Fatalf("reader login=%d", login.Code)
	}
	readerCookie := testSessionCookieFromRecorder(t, login)
	for _, operation := range []string{"download", "verify"} {
		if rec := catalogRequest(app, readerCookie, id, operation); rec.Code != 403 {
			t.Fatalf("reader %s=%d", operation, rec.Code)
		}
	}
	if rec := performContractRequest(app.Handler, "GET", "/api/backup/archives", nil, readerCookie, false); rec.Code != 403 {
		t.Fatalf("reader catalog=%d", rec.Code)
	}
	sm := app.Deps.CurrentSessionManager()
	ctx, err := sm.Load(context.Background(), cookie.Value)
	if err != nil {
		t.Fatal(err)
	}
	sm.Put(ctx, "auth_reauthenticated_at", time.Now().Add(-6*time.Minute).Unix())
	if _, _, err := sm.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"download", "verify"} {
		rec := catalogRequest(app, cookie, id, operation)
		if rec.Code != 401 || !strings.Contains(rec.Body.String(), `"reauthentication_required":true`) || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
			t.Fatalf("expired %s=%d %s", operation, rec.Code, rec.Body.String())
		}
	}
	if _, err := a.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestBackupCatalogCancellationExcludesRestorePublicationAndCleansStaging(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	before := catalogProtectedState(t, app)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	entered := make(chan struct{})
	a.verify = func(ctx context.Context, file internalbackup.TemporaryFile, pass string) (internalbackup.VerifyResult, error) {
		// A scheduled Tick may be admitted only after the catalogue lease closes.
		// Gate the first catalogue call; later publications use the real pipeline.
		select {
		case <-entered:
			return a.deps.BackupService.VerifyArchiveFile(ctx, file, pass)
		default:
		}
		if _, err := os.Stat(file.Path); err != nil {
			t.Error(err)
		}
		close(entered)
		<-ctx.Done()
		return internalbackup.VerifyResult{}, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("POST", "/api/backup/archives/"+archive.ID+"/verify", nil).WithContext(ctx)
	req.AddCookie(cookie)
	markSameOriginAuthRequest(req)
	done := make(chan struct{})
	go func() { defer close(done); app.Handler.ServeHTTP(httptest.NewRecorder(), req) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("verify not admitted")
	}
	for _, operation := range []maintenancepkg.OperationClass{maintenancepkg.OperationBackupExport, maintenancepkg.OperationBackupRestore} {
		lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(operation)
		if decision.Allowed {
			lease.Close()
			t.Fatal("exclusive operation overlapped verification")
		}
	}
	for _, path := range []string{"/api/backup/restore", "/api/backup/export"} {
		rec := performContractRequest(app.Handler, "POST", path, nil, cookie, true)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("concurrent %s status=%d", path, rec.Code)
		}
	}
	tickDone := make(chan error, 1)
	go func() { tickDone <- a.Tick(context.Background(), true) }() // deferred, never retain while reader holds lease
	if rec := catalogRequest(app, cookie, archive.ID, "verify"); rec.Code != 409 {
		t.Fatalf("parallel verification=%d", rec.Code)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled verification leaked")
	}
	select {
	case err := <-tickDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("deferred publication leaked")
	}
	entries, err := os.ReadDir(temp)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging leaked: %v %v", entries, err)
	}
	lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("shared lease leaked")
	}
	lease.Close()
	if catalogProtectedState(t, app) != before {
		t.Fatal("cancelled verification mutated active data")
	}
	// Once released, publication and real retention work normally and preserve
	// the original newest archive while applying the configured bound.
	a.verify = a.deps.BackupService.VerifyArchiveFile
	if err := a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, a)
	if len(state.Archives) != 2 {
		t.Fatalf("publication did not recover: %d", len(state.Archives))
	}
	foreign := filepath.Join(a.deployment.Directory, "operator-managed.slubkp")
	if err := os.WriteFile(foreign, []byte("foreign retained"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.deployment.Directory, archive.Name)); !os.IsNotExist(err) {
		t.Fatal("retention did not remove the older owned archive")
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign retained" {
		t.Fatal("retention changed a foreign file")
	}
	if catalogProtectedState(t, app) != before {
		t.Fatal("retention after verification changed protected active data")
	}

}

type catalogMutatingWriter struct {
	*httptest.ResponseRecorder
	mutate  func()
	started bool
}

func (w *catalogMutatingWriter) Write(p []byte) (int, error) {
	if !w.started {
		w.started = true
		w.mutate()
	}
	return w.ResponseRecorder.Write(p)
}

func TestBackupCatalogServesPinnedBytesWhenSourceChangesDuringStream(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	path := filepath.Join(a.deployment.Directory, archive.Name)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var mutationErr error
	writer := &catalogMutatingWriter{ResponseRecorder: httptest.NewRecorder(), mutate: func() { mutationErr = os.WriteFile(path, bytes.Repeat([]byte("x"), len(original)), 0600) }}
	req := httptest.NewRequest("POST", "/api/backup/archives/"+archive.ID+"/download", nil)
	req.AddCookie(cookie)
	markSameOriginAuthRequest(req)
	app.Handler.ServeHTTP(writer, req)
	if mutationErr != nil {
		t.Fatal(mutationErr)
	}
	if writer.Code != 200 || !bytes.Equal(writer.Body.Bytes(), original) {
		t.Fatal("hash-to-stream mutation changed validated bytes")
	}
	if rec := catalogRequest(app, cookie, archive.ID, "download"); rec.Code != 409 {
		t.Fatal("later corruption was accepted")
	}
}

func TestBackupCatalogListingRecordsLossOnceWithoutChangingPublicationFacts(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	initial := backupAutomationStateForTest(t, a)
	if err := os.Remove(filepath.Join(a.deployment.Directory, archive.Name)); err != nil {
		t.Fatal(err)
	}
	first := catalogFacts(t, app, cookie).Archives[0]
	if first.Availability != "missing" || first.Verification != "failed" || first.LastVerifiedAt != archive.LastVerifiedAt {
		t.Fatalf("loss projection=%+v", first)
	}
	var count int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("loss audit count=%d %v", count, err)
	}
	catalogFacts(t, app, cookie)
	var after int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&after); err != nil || after != count {
		t.Fatal("listing duplicated loss evidence")
	}
	state := backupAutomationStateForTest(t, a)
	if state.LastSuccessAt != initial.LastSuccessAt || state.Outcome != initial.Outcome {
		t.Fatal("current availability overwrote historical publication")
	}
	rec := performContractRequest(app.Handler, "GET", "/api/backup/status", nil, cookie, false)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"state":"failed"`) {
		t.Fatalf("public recovery state=%d %s", rec.Code, rec.Body.String())
	}
}

func TestBackupCatalogPersistedIDSurvivesSecretFailureRotationAndRestart(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	ctx := context.Background()
	before := catalogProtectedState(t, app)
	secret, err := os.ReadFile(a.deployment.SecretFile)
	if err != nil {
		t.Fatal(err)
	}
	originalID := catalogFacts(t, app, cookie).Archives[0].ID
	if originalID != archive.ID || !validBackupArchiveID(originalID) {
		t.Fatal("publication did not persist a valid opaque ID")
	}
	var raw string
	if err := app.Deps.DB().QueryRow("SELECT value FROM settings WHERE key=?", backupAutomationSettingsKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var persisted backupAutomationState
	if err := json.Unmarshal([]byte(raw), &persisted); err != nil || persisted.Archives[0].ID != originalID {
		t.Fatal("public ID was not durable before exposure")
	}
	for _, tc := range []struct {
		name, secret string
		missing      bool
	}{
		{name: "missing", missing: true},
		{name: "invalid", secret: "short"},
		{name: "rotated", secret: "a different disposable deployment passphrase"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.missing {
				err = os.Remove(a.deployment.SecretFile)
			} else {
				err = os.WriteFile(a.deployment.SecretFile, []byte(tc.secret), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			restarted := newBackupAutomation(app.Deps)
			restarted.deployment = a.deployment
			status, err := restarted.Status(ctx)
			if err != nil || len(status.Catalog.Archives) != 1 || status.Catalog.Archives[0].ID != originalID {
				t.Fatalf("secret state changed durable ID: %v", err)
			}
			response := catalogRequest(app, cookie, originalID, "verify")
			if response.Code < 400 {
				t.Fatalf("verification accepted %s secret", tc.name)
			}
			facts := catalogFacts(t, app, cookie)
			if facts.Archives[0].ID != originalID {
				t.Fatal("failed verification rotated catalog ID")
			}
			for _, private := range []string{string(bytes.TrimSpace(secret)), tc.secret, a.deployment.SecretFile, archive.Owner, archive.Name} {
				if private != "" && (strings.Contains(response.Body.String(), private) || strings.Contains(string(mustCatalogJSON(t, facts)), private)) {
					t.Fatal("catalog or refusal leaked private identity/credential data")
				}
			}
			if catalogProtectedState(t, app) != before {
				t.Fatal("secret failure changed protected active data")
			}
		})
	}
	if err := os.WriteFile(a.deployment.SecretFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	if response := catalogRequest(app, cookie, originalID, "verify"); response.Code != 200 {
		t.Fatalf("restored secret verification=%d %s", response.Code, response.Body.String())
	}
	if facts := catalogFacts(t, app, cookie); facts.Archives[0].ID != originalID {
		t.Fatal("successful verification rotated catalog ID")
	}
	if catalogProtectedState(t, app) != before {
		t.Fatal("verification changed protected data")
	}
	rows, err := app.Deps.DB().Query("SELECT meta_json FROM audit_events WHERE action IN ('backup.verify','backup.download')")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatal(err)
		}
		var meta map[string]any
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		if meta["archive_id"] != originalID {
			t.Fatal("audit lost durable archive correlation")
		}
		for _, private := range []string{string(bytes.TrimSpace(secret)), "a different disposable deployment passphrase", archive.Owner, archive.Name} {
			if strings.Contains(raw, private) {
				t.Fatal("private fact leaked into audit")
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func mustCatalogJSON(t *testing.T, catalog backupArchiveCatalog) []byte {
	t.Helper()
	raw, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestBackupCatalogLegacyIDMigrationIsDurableWithoutSecretOrInventedFacts(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	state := backupAutomationStateForTest(t, a)
	state.Archives = []ownedBackupArchive{{Name: archive.Name, Owner: archive.Owner, SHA256: archive.SHA256}, {Name: "slu-auto-foreign-legacy.slubkp", Owner: "foreign", SHA256: archive.SHA256}}
	if err := a.save(context.Background(), state); err != nil {
		t.Fatal(err)
	}
	secret, err := os.ReadFile(a.deployment.SecretFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.deployment.SecretFile); err != nil {
		t.Fatal(err)
	}
	first := catalogFacts(t, app, cookie)
	if len(first.Archives) != 2 || first.Archives[0].ID == first.Archives[1].ID {
		t.Fatal("legacy rows lack distinct independent IDs")
	}
	migrated := backupAutomationStateForTest(t, a)
	for i, item := range migrated.Archives {
		if !validBackupArchiveID(item.ID) || item.PublishedAt != "" || item.LastVerifiedAt != "" || item.SizeBytes != nil || item.KnownHostsIncluded != nil || item.Verification != "" || item.Name != state.Archives[i].Name || item.Owner != state.Archives[i].Owner || item.SHA256 != state.Archives[i].SHA256 {
			t.Fatal("ID migration changed legacy facts or failed to persist")
		}
	}
	if migrated.Approval != state.Approval || migrated.LastSuccessAt != state.LastSuccessAt || migrated.Outcome != state.Outcome {
		t.Fatal("ID migration changed automation approval/history")
	}
	if err := os.WriteFile(a.deployment.SecretFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	restarted := newBackupAutomation(app.Deps)
	restarted.deployment = a.deployment
	status, err := restarted.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for i, item := range status.Catalog.Archives {
		if item.ID != first.Archives[i].ID {
			t.Fatal("restart or secret reappearance changed legacy ID")
		}
	}
	if response := catalogRequest(app, cookie, first.Archives[0].ID, "verify"); response.Code != 409 {
		t.Fatal("ID migration granted foreign ownership")
	}
	if response := catalogRequest(app, cookie, first.Archives[1].ID, "verify"); response.Code != 200 {
		t.Fatalf("migrated owned archive verification=%d", response.Code)
	}
}

func TestBackupCatalogMigrationCannotExposeUnsavedOrAmbiguousIDs(t *testing.T) {
	for _, kind := range []string{"persistence failure", "duplicate", "invalid"} {
		t.Run(kind, func(t *testing.T) {
			app, a, cookie, archive := publishedCatalogFixture(t)
			state := backupAutomationStateForTest(t, a)
			switch kind {
			case "persistence failure":
				state.Archives[0].ID = ""
				if err := a.save(context.Background(), state); err != nil {
					t.Fatal(err)
				}
				_, err := app.Deps.DB().Exec(`CREATE TRIGGER reject_catalog_id_migration BEFORE UPDATE ON settings WHEN NEW.key='backup_automation_v1' BEGIN SELECT RAISE(ABORT,'controlled catalog migration failure'); END`)
				if err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				state.Archives = append(state.Archives, archive)
				if err := a.save(context.Background(), state); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				state.Archives[0].ID = "../../private-passphrase"
				if err := a.save(context.Background(), state); err != nil {
					t.Fatal(err)
				}
			}
			response := performContractRequest(app.Handler, "GET", "/api/backup/archives", nil, cookie, false)
			if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), `"archives"`) || strings.Contains(response.Body.String(), "private-passphrase") {
				t.Fatalf("invalid migration exposed catalog: %d %s", response.Code, response.Body.String())
			}
			if kind == "persistence failure" {
				var raw string
				if err := app.Deps.DB().QueryRow("SELECT value FROM settings WHERE key=?", backupAutomationSettingsKey).Scan(&raw); err != nil {
					t.Fatal(err)
				}
				var stored backupAutomationState
				if err := json.Unmarshal([]byte(raw), &stored); err != nil || stored.Archives[0].ID != "" {
					t.Fatal("failed migration was partially persisted")
				}
				if _, err := app.Deps.DB().Exec("DROP TRIGGER reject_catalog_id_migration"); err != nil {
					t.Fatal(err)
				}
				allocated := catalogFacts(t, app, cookie).Archives[0].ID
				if !validBackupArchiveID(allocated) || catalogFacts(t, app, cookie).Archives[0].ID != allocated {
					t.Fatal("recovered migration did not expose a durable stable ID")
				}
			}
		})
	}
}
