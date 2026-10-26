package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	auditpkg "debian-updater/internal/audit"
	serverpkg "debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

func importAPIRequest(app *testApp, cookie *http.Cookie, method, path string, body []byte) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	return rec
}

func TestInventoryImportAPIAuthValidationCommitAuditAndExistingSetupFlow(t *testing.T) {
	app := newIsolatedTestApp(t)
	raw := []byte(`[{"name":"imported","host":"192.0.2.71","user":"root","tags":[" production ","production"]},{"name":"bad","host":"https://invalid","user":"root"}]`)
	for _, path := range []string{"/api/servers/import/preview?format=json", "/api/servers/import"} {
		if rec := importAPIRequest(app, nil, http.MethodPost, path, raw); rec.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d", path, rec.Code)
		}
	}
	cookie := app.authenticate(t)
	originalScanner := scanHostKeyFunc
	scanHostKeyFunc = func(string, int) (ssh.PublicKey, error) {
		t.Fatal("preview/import contacted SSH")
		return nil, nil
	}
	t.Cleanup(func() { scanHostKeyFunc = originalScanner })
	for _, format := range []string{"csv", "json"} {
		example := importAPIRequest(app, cookie, http.MethodGet, "/api/servers/import/example/"+format, nil)
		if example.Code != http.StatusOK || !strings.Contains(example.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("example %s = %d", format, example.Code)
		}
		if rows, err := serverpkg.ParseInventoryImport(example.Body, format); err != nil || len(rows) != 2 {
			t.Fatalf("example %s cannot be imported: %v", format, err)
		}
	}
	rec := importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import/preview?format=json", raw)
	var preview serverpkg.ImportPreview
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &preview) != nil || len(preview.Rows) != 2 || len(preview.Rows[1].Errors) == 0 {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body.String())
	}
	if len(app.Deps.ServerState.CloneServers()) != 0 {
		t.Fatal("preview published inventory")
	}
	selection := serverpkg.ImportSelection{BatchID: preview.BatchID, Rows: []serverpkg.ImportEntry{preview.Rows[0].ImportEntry}}
	body, _ := json.Marshal(selection)
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	if rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"created":["imported"]`) {
		t.Fatalf("commit = %d %s", rec.Code, rec.Body.String())
	}
	var disabled bool
	var encryptedPass, encryptedKey string
	if err := app.Deps.DB().QueryRow("SELECT disabled, pass_enc, key_enc FROM servers WHERE name = ?", "imported").Scan(&disabled, &encryptedPass, &encryptedKey); err != nil || !disabled {
		t.Fatalf("imported disabled=%v err=%v", disabled, err)
	}
	pass, _ := decryptSecret(encryptedPass)
	key, _ := decryptSecret(encryptedKey)
	knownHosts, _ := os.ReadFile(app.KnownHostsPath)
	if pass != "" || key != "" || len(knownHosts) != 0 {
		t.Fatal("import added credentials or host trust")
	}
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	if rec.Code != http.StatusConflict || len(app.Deps.ServerState.CloneServers()) != 1 {
		t.Fatalf("replay was not atomically rejected: %d", rec.Code)
	}
	var actor, targetName, meta string
	if err := app.Deps.DB().QueryRow("SELECT actor, target_name, meta_json FROM audit_events WHERE action = 'server.import' AND status = 'success'").Scan(&actor, &targetName, &meta); err != nil || actor != "admin" || targetName != preview.BatchID || !strings.Contains(meta, `"created_names":["imported"]`) || strings.Contains(meta, "192.0.2.71") {
		t.Fatalf("audit actor=%s target=%s meta=%s err=%v", actor, targetName, meta, err)
	}
	rec = importAPIRequest(app, cookie, http.MethodGet, "/api/audit-events?category=admin_activity&action=server.import", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Inventory imported as disabled servers") {
		t.Fatalf("import missing from admin activity: %d %s", rec.Code, rec.Body.String())
	}
	// Reuse Manage's separate credential, fingerprint confirmation, and enable
	// commands. No special import bypass of existing server mutation rules.
	rec = importAPIRequest(app, cookie, http.MethodPut, "/api/servers/imported", []byte(`{"name":"imported","host":"192.0.2.71","port":22,"user":"root","pass":"test-fixture-password","tags":["production"]}`))
	if rec.Code != http.StatusOK || !app.Deps.ServerState.CloneServers()[0].Disabled {
		t.Fatalf("configure imported server = %d %s", rec.Code, rec.Body.String())
	}
	signer, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}
	scanHostKeyFunc = func(string, int) (ssh.PublicKey, error) { return signer.PublicKey(), nil }
	scan := importAPIRequest(app, cookie, http.MethodPost, "/api/hostkeys/scan", []byte(`{"host":"192.0.2.71","port":22}`))
	if scan.Code != http.StatusOK {
		t.Fatalf("scan imported: %d %s", scan.Code, scan.Body.String())
	}
	trustBody, _ := json.Marshal(map[string]any{"host": "192.0.2.71", "port": 22, "fingerprint_sha256": ssh.FingerprintSHA256(signer.PublicKey())})
	trust := importAPIRequest(app, cookie, http.MethodPost, "/api/hostkeys/trust", trustBody)
	if trust.Code != http.StatusOK {
		t.Fatalf("trust imported: %d %s", trust.Code, trust.Body.String())
	}
	enable := importAPIRequest(app, cookie, http.MethodPut, "/api/servers/imported/availability", []byte(`{"disabled":false}`))
	if enable.Code != http.StatusOK {
		t.Fatalf("enable imported: %d %s", enable.Code, enable.Body.String())
	}
	statuses := app.Deps.ServerInventoryService.ListStatuses()
	if len(statuses) != 1 || statuses[0].Disabled || !statuses[0].HasPassword || statuses[0].HostKeyStatus != serverpkg.HostKeyStatusTrusted {
		t.Fatalf("configured server = %+v", statuses)
	}
}

func TestInventoryImportSQLRollbackAndConcurrentInventoryChange(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	commands := serverpkg.NewCommandService(app.Deps.ServerInventoryService)
	if !commands.CreateServer(Server{Name: "existing", Host: "existing.example", User: "root", Pass: "fixture", Tags: []string{"retained"}}).Succeeded() {
		t.Fatal("seed inventory failed")
	}
	raw := []byte(`[{"name":"first","host":"first.example","user":"root"},{"name":"second","host":"second.example","user":"root"}]`)
	rec := importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import/preview?format=json", raw)
	var preview serverpkg.ImportPreview
	if json.Unmarshal(rec.Body.Bytes(), &preview) != nil {
		t.Fatal(rec.Body.String())
	}
	selection := serverpkg.ImportSelection{BatchID: preview.BatchID, Rows: []serverpkg.ImportEntry{preview.Rows[0].ImportEntry, preview.Rows[1].ImportEntry}}
	body, _ := json.Marshal(selection)
	if _, err := app.Deps.DB().Exec("CREATE TRIGGER fail_import_second BEFORE INSERT ON servers WHEN NEW.name = 'second' BEGIN SELECT RAISE(ABORT, 'fixture SQL failure'); END"); err != nil {
		t.Fatal(err)
	}
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	var count int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM servers").Scan(&count); err != nil || count != 1 || rec.Code != http.StatusInternalServerError || len(app.Deps.ServerState.CloneServers()) != 1 || strings.Contains(rec.Body.String(), "fixture SQL failure") {
		t.Fatalf("rollback count=%d state=%v response=%d %s err=%v", count, app.Deps.ServerState.CloneServers(), rec.Code, rec.Body.String(), err)
	}
	var savedPass string
	if err := app.Deps.DB().QueryRow("SELECT pass_enc FROM servers WHERE name = 'existing'").Scan(&savedPass); err != nil {
		t.Fatal(err)
	}
	if pass, err := decryptSecret(savedPass); err != nil || pass != "fixture" {
		t.Fatal("rollback changed existing credentials")
	}
	if _, err := app.Deps.DB().Exec("DROP TRIGGER fail_import_second"); err != nil {
		t.Fatal(err)
	}
	if !commands.CreateServer(Server{Name: "concurrent", Host: "FIRST.EXAMPLE", User: "root"}).Succeeded() {
		t.Fatal("concurrent inventory change failed")
	}
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	var result serverpkg.ImportResult
	if json.Unmarshal(rec.Body.Bytes(), &result) != nil || rec.Code != http.StatusConflict || len(app.Deps.ServerState.CloneServers()) != 2 || len(result.Preview.Rows[0].Errors) == 0 || len(result.Preview.Rows[1].Errors) != 0 {
		t.Fatalf("concurrent conflict = %d %s", rec.Code, rec.Body.String())
	}
	selection.Rows = []serverpkg.ImportEntry{result.Preview.Rows[1].ImportEntry}
	body, _ = json.Marshal(selection)
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	if rec.Code != http.StatusCreated || len(app.Deps.ServerState.CloneServers()) != 3 {
		t.Fatalf("refreshed selection retry = %d %s", rec.Code, rec.Body.String())
	}
}

func TestInventoryImportMultipleServersPersistAsDisabledBatch(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	input := []byte(`[{"name":"batch-a","host":"192.0.2.101","user":"updater","tags":["prod","web"]},{"name":"batch-b","host":"2001:db8::102","port":2222,"user":"root","tags":["staging"]}]`)
	rec := importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import/preview?format=json", input)
	var preview serverpkg.ImportPreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatal(err)
	}
	selection := serverpkg.ImportSelection{BatchID: preview.BatchID}
	for _, row := range preview.Rows {
		selection.Rows = append(selection.Rows, row.ImportEntry)
	}
	body, _ := json.Marshal(selection)
	rec = importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("batch = %d %s", rec.Code, rec.Body.String())
	}
	stored, err := (serverpkg.SQLiteRepository{DB: app.Deps.DB, Decrypt: decryptSecret}).Load()
	if err != nil || len(stored) != 2 || len(app.Deps.ServerState.CloneServers()) != 2 {
		t.Fatalf("stored batch = %+v, %v", stored, err)
	}
	for _, server := range stored {
		if !server.Disabled || server.Pass != "" || server.Key != "" || len(server.Tags) == 0 {
			t.Fatalf("batch member = %+v", server)
		}
	}
	var jobs int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("import launched maintenance: jobs=%d err=%v", jobs, err)
	}
}

func TestInventoryImportLargeBatchRetainsAllAuditFacts(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	selection := serverpkg.ImportSelection{BatchID: strings.Repeat("a", 32)}
	for i := range 100 {
		// Quotes and HTML-sensitive characters exercise the actual JSON byte cap.
		name := fmt.Sprintf("%03d-%s", i, strings.Repeat("<\"", 50))
		selection.Rows = append(selection.Rows, serverpkg.ImportEntry{Row: i + 1, Name: name, Host: fmt.Sprintf("host%d.example", i), Port: 22, User: "root", Tags: []string{}})
	}
	body, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	response := importAPIRequest(app, cookie, http.MethodPost, "/api/servers/import", body)
	if response.Code != http.StatusCreated {
		t.Fatalf("import = %d %s", response.Code, response.Body.String())
	}
	rows, err := app.Deps.DB().Query("SELECT actor, target_name, meta_json FROM audit_events WHERE action = 'server.import' AND status = 'success' ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	created := map[string]bool{}
	records := 0
	parts := 0
	for rows.Next() {
		var actor, batchID, raw string
		if err := rows.Scan(&actor, &batchID, &raw); err != nil {
			t.Fatal(err)
		}
		var meta struct {
			BatchID       string   `json:"batch_id"`
			SelectedCount int      `json:"selected_count"`
			CreatedCount  int      `json:"created_count"`
			CreatedNames  []string `json:"created_names"`
			Part          int      `json:"part"`
			Parts         int      `json:"parts"`
			Truncated     bool     `json:"_truncated"`
		}
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			t.Fatal(err)
		}
		records++
		if actor != "admin" || batchID != selection.BatchID || meta.BatchID != selection.BatchID || meta.SelectedCount != len(selection.Rows) || meta.CreatedCount != len(selection.Rows) || meta.Truncated || len(raw) > auditpkg.MetaMaxLen || meta.Part != records {
			t.Fatalf("audit facts lost: actor=%s target=%s meta=%s", actor, batchID, raw)
		}
		if parts != 0 && meta.Parts != parts {
			t.Fatal("inconsistent audit part counts")
		}
		parts = meta.Parts
		for _, name := range meta.CreatedNames {
			if created[name] {
				t.Fatalf("name recorded more than once: %q", name)
			}
			created[name] = true
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if records < 2 || records != parts || len(created) != len(selection.Rows) {
		t.Fatalf("audit records=%d parts=%d names=%d, want %d names", records, parts, len(created), len(selection.Rows))
	}
	for _, row := range selection.Rows {
		if !created[row.Name] {
			t.Fatalf("missing created name: %q", row.Name)
		}
	}
}
