package main

import (
	"encoding/json"
	"net/http"
	"testing"

	"debian-updater/internal/vulnerabilities"
)

func TestStandaloneCVERoutesAreRetiredWithoutStartingWork(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, path := range []string{"/vulnerabilities", "/vulnerabilities/all?server=example", "/remediation"} {
		r := identityRequest(app, "GET", path, cookie, "", nil)
		if r.Code != http.StatusSeeOther || r.Header().Get("Location") != "/" {
			t.Fatal(path, r.Code, r.Body.String())
		}
	}
	for method, paths := range map[string][]string{
		"GET":  {"/api/security-actions", "/api/vulnerabilities", "/api/vulnerabilities/CVE-2026-1000", "/api/vulnerabilities/plan", "/api/servers/one/vulnerabilities", "/api/servers/one/remediation", "/api/servers/one/remediation/old", "/api/servers/one/remediation/old/evidence"},
		"POST": {"/api/servers/one/vulnerabilities/scan", "/api/servers/one/vulnerabilities/scans/old/cancel", "/api/servers/one/vulnerabilities/reviews", "/api/servers/one/remediation/prepare", "/api/servers/one/remediation/old/retry"},
	} {
		for _, path := range paths {
			r := identityRequest(app, method, path, cookie, "", nil)
			if r.Code != http.StatusGone || r.Header().Get("Cache-Control") == "" {
				t.Fatal(path, r.Code, r.Body.String())
			}
		}
	}
	var count int
	if err := app.Deps.DB().QueryRow(`SELECT COUNT(*) FROM jobs`).Scan(&count); err != nil || count != 0 {
		t.Fatal("retired endpoint created work", count, err)
	}
	if app.Deps.UpdateService.EnsureDeps().RecordApprovalFindings != nil || app.Deps.SecurityIntel != nil {
		t.Fatal("standalone intelligence still composed")
	}
}

func TestLegacyCVEObservationsDoNotContributeToStatusOrGetRewritten(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	if _, err := app.Deps.ServerInventoryService.Create(Server{Name: "legacy-recorded", Host: "192.0.2.97", User: "root"}); err != nil {
		t.Fatal(err)
	}
	observation := vulnerabilities.Observation{Accepted: &vulnerabilities.Scan{State: "partial", Distro: "debian 13", Inventoried: 500}, Findings: []vulnerabilities.Finding{{ID: "CVE-2026-1000", Package: "old-package"}}}
	raw, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = app.Deps.DB().Exec(`INSERT INTO server_vulnerability_observations (server_name, observation) VALUES (?, ?)`, "legacy-recorded", string(raw)); err != nil {
		t.Fatal(err)
	}
	r := identityRequest(app, "GET", "/api/dashboard/summary", cookie, "", nil)
	if r.Code != 200 {
		t.Fatal(r.Code, r.Body.String())
	}
	var summary dashboardSummaryResponse
	if err = json.Unmarshal(r.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if len(summary.Servers) != 1 || len(summary.Servers[0].Risk.CVEs) != 0 {
		t.Fatal(summary)
	}
	var payload map[string]any
	if err = json.Unmarshal(r.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	risk := payload["servers"].([]any)[0].(map[string]any)["risk"].(map[string]any)
	if _, found := risk["vulnerability"]; found {
		t.Fatal("legacy scan exposed on Status", risk)
	}
	var after string
	if err = app.Deps.DB().QueryRow(`SELECT observation FROM server_vulnerability_observations WHERE server_name=?`, "legacy-recorded").Scan(&after); err != nil || after != string(raw) {
		t.Fatal("legacy data changed", err)
	}
}
