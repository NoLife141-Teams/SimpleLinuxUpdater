package main

import (
	"bytes"
	"context"
	maintenancepkg "debian-updater/internal/maintenance"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	auditpkg "debian-updater/internal/audit"
	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

func TestInventoryExportRoundtripSecondInstallationNoCredentialsTrustOrActivation(t *testing.T) {
	source := newIsolatedTestApp(t)
	cookie := source.authenticate(t)
	for _, body := range []string{`{"name":"export-b","host":"192.0.2.82","port":2222,"user":"updater","pass":"synthetic-secret-only","tags":[" production ","production","雪"]}`, `{"name":"export-a","host":"2001:db8::20","user":"root"}`} {
		rec := importAPIRequest(source, cookie, "POST", "/api/servers", []byte(body))
		if rec.Code != 201 {
			t.Fatalf("seed = %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := importAPIRequest(source, cookie, "POST", "/api/servers/export", []byte(`{"scope":"all","count":2}`))
	if rec.Code != 200 {
		t.Fatalf("export = %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-SLU-Export-Version") != "1" || rec.Header().Get("X-SLU-Export-Count") != "2" {
		t.Fatal("missing export headers")
	}
	var entries []map[string]any
	if json.Unmarshal(rec.Body.Bytes(), &entries) != nil {
		t.Fatal("invalid JSON")
	}
	for _, entry := range entries {
		if len(entry) != 5 {
			t.Fatalf("unexpected export fields: %v", entry)
		}
		for _, field := range []string{"name", "host", "port", "user", "tags"} {
			if _, ok := entry[field]; !ok {
				t.Fatal("missing public field")
			}
		}
	}
	if strings.Contains(rec.Body.String(), "synthetic-secret") {
		t.Fatal("credential exported")
	}
	raw := append([]byte{}, rec.Body.Bytes()...)
	destination := newIsolatedTestApp(t)
	destinationCookie := destination.authenticate(t)
	rec = importAPIRequest(destination, destinationCookie, "POST", "/api/servers/import/preview?format=json", raw)
	var preview serverpkg.ImportPreview
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &preview) != nil || len(preview.Rows) != 2 {
		t.Fatalf("preview = %d %s", rec.Code, rec.Body.String())
	}
	selection := serverpkg.ImportSelection{BatchID: preview.BatchID}
	for _, row := range preview.Rows {
		if len(row.Errors) > 0 {
			t.Fatal(row.Errors)
		}
		selection.Rows = append(selection.Rows, row.ImportEntry)
	}
	body, _ := json.Marshal(selection)
	rec = importAPIRequest(destination, destinationCookie, "POST", "/api/servers/import", body)
	if rec.Code != 201 {
		t.Fatalf("import = %d %s", rec.Code, rec.Body.String())
	}
	exportedAgain := importAPIRequest(destination, destinationCookie, "POST", "/api/servers/export", []byte(`{"scope":"all","count":2}`))
	if !bytes.Equal(raw, exportedAgain.Body.Bytes()) {
		t.Fatal("public normalized fields changed during roundtrip")
	}
	for _, server := range destination.Deps.ServerState.CloneServers() {
		if !server.Disabled || server.Pass != "" || server.Key != "" {
			t.Fatal("import activated hosts or added credentials")
		}
	}
	knownHosts, _ := os.ReadFile(destination.KnownHostsPath)
	if len(knownHosts) != 0 {
		t.Fatal("trust imported")
	}
}
func TestOperationalExportSelectionsPermissionsBearerAndEmpty(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	for _, role := range []string{"reader", "operator"} {
		if _, err := app.Deps.AuthService.CreateAccount(role, testPasswordStrong, role); err != nil {
			t.Fatal(err)
		}
		cookie := identityLogin(t, app, role)
		for _, path := range []string{"/api/servers/export", "/api/audit-events/export"} {
			rec := importAPIRequest(app, cookie, "POST", path, []byte(`{}`))
			if rec.Code != 403 {
				t.Fatalf("%s %s = %d", role, path, rec.Code)
			}
		}
	}
	_, secret, err := app.Deps.AuthService.CreatePersonalToken(1, "export audit", []string{"audit:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		path string
		body any
		want int
	}{
		{"/api/servers/export", map[string]any{"scope": "all", "count": 0}, 401},
		{"/api/audit-events/export", map[string]any{"format": "json", "filters": map[string]string{"actor": "no-match"}}, 200},
	} {
		rec := identityRequest(app, "POST", tc.path, nil, secret, tc.body)
		if rec.Code != tc.want {
			t.Fatalf("bearer %s = %d %s", tc.path, rec.Code, rec.Body.String())
		}
	}
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"scope":"all","count":0}`, 200},
		{`{"scope":"selection","names":[],"count":0}`, 200},
		{`{"scope":"filtered","names":[],"count":0}`, 200},
		{`{"scope":"all","count":1}`, 409},
		{`{"scope":"filtered","names":["missing"],"count":1}`, 409},
		{`{"count":0}`, 409},
		{`{"scope":"all","count":0,"pass":"never"}`, 400},
	} {
		rec := importAPIRequest(app, admin, "POST", "/api/servers/export", []byte(tc.body))
		if rec.Code != tc.want {
			t.Fatalf("%s = %d %s", tc.body, rec.Code, rec.Body.String())
		}
	}
	for _, format := range []string{"json", "csv"} {
		rec := importAPIRequest(app, admin, "POST", "/api/audit-events/export", []byte(`{"format":"`+format+`","filters":{"actor":"no-match"}}`))
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("X-SLU-Export-Count") != "0" {
			t.Fatalf("empty audit = %d %s", rec.Code, rec.Body.String())
		}
	}
}
func TestAuditExportAPIFiltersRedactionAndClientCancellation(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, actor := range []string{"specific", "other"} {
		if err := app.Deps.AuditService.Write(AuditEvent{CreatedAt: "2026-10-28T00:00:00Z", Actor: actor, Action: "server.import", TargetType: "server", TargetName: "x", Status: "failure", MetaJSON: `{"password":"not-for-export","last_error_class":"network"}`}); err != nil {
			t.Fatal(err)
		}
	}
	request := []byte(`{"format":"json","filters":{"category":"admin_activity","actor":"specific","target_name":"x","target_type":"server","action":"server.import","status":"failure","failure_cause":"error_class:network","from":"2026-10-27T00:00:00Z","to":"2026-10-29T00:00:00Z"}}`)
	rec := importAPIRequest(app, cookie, "POST", "/api/audit-events/export", request)
	var got struct {
		Items    []AuditEvent            `json:"items"`
		Manifest auditpkg.ExportManifest `json:"manifest"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &got) != nil || len(got.Items) != 1 || got.Items[0].Actor != "specific" || strings.Contains(rec.Body.String(), "not-for-export") {
		t.Fatalf("filtered export = %d %s", rec.Code, rec.Body.String())
	}
	listed := importAPIRequest(app, cookie, "GET", "/api/audit-events?actor=specific&action=server.import", nil)
	var list AuditListResult
	if json.Unmarshal(listed.Body.Bytes(), &list) != nil || list.Total != got.Manifest.Count {
		t.Fatal("canonical search differs")
	}
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest("POST", "/api/audit-events/export", bytes.NewReader(request)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	req.AddCookie(cookie)
	app.Handler.ServeHTTP(httptest.NewRecorder(), req)
	files, _ := os.ReadDir(dir)
	if len(files) != 0 || app.Deps.DB().Stats().InUse != 0 {
		t.Fatal("cancel retained spool or cursor")
	}
}

type failedExportResponse struct{ *httptest.ResponseRecorder }

func (w failedExportResponse) Write([]byte) (int, error) {
	return 0, errors.New("synthetic disconnected downloader")
}
func TestAuditExportDisconnectedDownloadReleasesSpoolAndMaintenanceLease(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)
	req := httptest.NewRequest("POST", "/api/audit-events/export", strings.NewReader(`{"format":"json"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(cookie)
	markSameOriginAuthRequest(req)
	app.Handler.ServeHTTP(failedExportResponse{httptest.NewRecorder()}, req)
	files, _ := os.ReadDir(dir)
	if len(files) != 0 || app.Deps.DB().Stats().InUse != 0 {
		t.Fatal("disconnected download retained cursor or spool")
	}
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupExport)
	if !decision.Allowed {
		t.Fatal("export retained maintenance lease")
	}
	lease.Close()
}

func TestAuditExportHTTPCountLimitAndPreparationBudgetAreExplicit(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	_, err := app.Deps.DB().Exec(`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<10001) INSERT INTO audit_events(created_at,actor,action,target_type,target_name,status,message,meta_json,request_id,client_ip) SELECT '2026-10-27T00:00:00Z','limit-fixture','a','server','a','success','','{}','','' FROM seq`)
	if err != nil {
		t.Fatal(err)
	}
	rec := importAPIRequest(app, cookie, "POST", "/api/audit-events/export", []byte(`{"format":"json","filters":{"actor":"limit-fixture"}}`))
	if rec.Code != 413 || !strings.Contains(rec.Body.String(), "10000") || rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("limit must be explicit before download: %d %s", rec.Code, rec.Body.String())
	}
	// Exercise the transport mapping with an already exhausted preparation budget,
	// independently of authentication's own database work.
	router := gin.New()
	registerOperationalExportRoutes(router, app.Deps)
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	req := httptest.NewRequest("POST", "/api/audit-events/export", strings.NewReader(`{"format":"csv"}`)).WithContext(ctx)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 408 || !strings.Contains(rec.Body.String(), "15 second") || rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("budget must be explicit before download: %d %s", rec.Code, rec.Body.String())
	}
}
