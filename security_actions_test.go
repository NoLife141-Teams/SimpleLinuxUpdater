package main

import (
	"encoding/json"
	"testing"

	"debian-updater/internal/observability"
	"debian-updater/internal/vulnerabilities"
)

func TestSecurityActionsReaderRouteIsReadOnlyAndBounded(t *testing.T) {
	app := newIsolatedTestApp(t)
	app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("actions-reader", testPasswordStrong, "reader"); err != nil {
		t.Fatal(err)
	}
	reader := identityLogin(t, app, "actions-reader")
	response := identityRequest(app, "GET", "/api/security-actions?page_size=1", reader, "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var data struct {
		Fixes, Alerts, Lifecycle []json.RawMessage
		Pages                    int
	}
	if err := json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Fixes) > 1 || len(data.Alerts) > 1 || len(data.Lifecycle) > 1 || data.Pages < 1 {
		t.Fatal("unbounded response")
	}
	if response.Header().Get("Cache-Control") == "" {
		t.Fatal("security inventory response may be cached")
	}
	if r := identityRequest(app, "GET", "/api/security-actions?page_size=1000", reader, "", nil); r.Code != 400 {
		t.Fatal("invalid pagination accepted")
	}
}

func TestSecurityActionsReadsLegacyFindingInventoryWithoutRewritingObservations(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	if _, err := app.Deps.ServerInventoryService.Create(Server{Name: "legacy-recorded", Host: "192.0.2.97", User: "root"}); err != nil {
		t.Fatal(err)
	}
	at := "2020-01-01T00:00:00Z"
	observation := vulnerabilities.Observation{Accepted: &vulnerabilities.Scan{State: "partial", Distro: "debian 11", Inventoried: 500, FinishedAt: at, Packages: []vulnerabilities.Package{}}, Findings: []vulnerabilities.Finding{
		{ID: "CVE-2026-1000", Package: "python3.9-minimal:amd64", InstalledVersion: "3.9.2-1", LastSeen: at},
		{ID: "CVE-2026-1001", Package: "nodejs", CandidateVersion: "22.1"},
	}}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Deps.DB().Exec(`INSERT INTO server_vulnerability_observations (server_name, observation) VALUES (?, ?)`, "legacy-recorded", string(raw)); err != nil {
		t.Fatal(err)
	}
	response := identityRequest(app, "GET", "/api/security-actions?server=legacy-recorded&q=python&page_size=1", cookie, "", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var data observability.SecurityActions
	if err = json.Unmarshal(response.Body.Bytes(), &data); err != nil {
		t.Fatal(err)
	}
	if data.LifecycleTotal != 1 || len(data.Lifecycle) != 1 {
		t.Fatalf("legacy installed application missing from API: %+v", data)
	}
	row := data.Lifecycle[0]
	if row.Component != "python3.9-minimal:amd64" || row.Version != "3.9.2-1" || row.InventorySource != "recorded_findings" || !row.InventoryPartial || row.InventoryObservedAt != at || row.InventoryFreshness != "stale" {
		t.Fatalf("legacy inventory provenance or freshness lost: %+v", row)
	}
	var after string
	if err = app.Deps.DB().QueryRow(`SELECT observation FROM server_vulnerability_observations WHERE server_name = ?`, "legacy-recorded").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != string(raw) {
		t.Fatal("read-only lifecycle query rewrote recorded evidence")
	}
}
