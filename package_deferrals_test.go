package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/deferrals"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
)

func TestPackageDeferralAPIActorInheritanceExpirationAndInventoryContinuity(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "deferrals.db")})
	cookie := app.authenticate(t)
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	server := Server{Name: "deferral-web", Host: "example.invalid", Port: 22, User: "operator", Tags: []string{"web"}}
	if _, err := app.Deps.ServerInventoryService.Create(server); err != nil {
		t.Fatal(err)
	}
	policy, err := app.Deps.PolicyRepository.CreatePolicy(policies.Policy{Name: "Web maintenance", Enabled: true, IncludeTags: []string{"web"}, PackageScope: "security", ExecutionMode: "scan_only", CadenceKind: "daily", TimeLocal: "02:00"})
	if err != nil {
		t.Fatal(err)
	}
	rule := deferrals.Rule{ServerName: server.Name, Selector: "openssl:amd64", Reason: "Investigating compatibility", Author: "attacker", CreatedAt: "2000-01-01T00:00:00Z", ExpiresAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}
	response := request("POST", "/api/package-deferrals", rule)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	var saved deferrals.Rule
	if err = json.Unmarshal(response.Body.Bytes(), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Author == "attacker" || saved.Author == "unknown" || saved.CreatedAt == rule.CreatedAt {
		t.Fatal(saved)
	}
	rule.ServerName = ""
	rule.PolicyID = policy.ID
	response = request("POST", "/api/package-deferrals", rule)
	if response.Code != 201 {
		t.Fatal(response.Code, response.Body.String())
	}
	response = request("GET", "/api/servers/"+server.Name+"/package-deferrals", nil)
	if response.Code != 200 {
		t.Fatal(response.Code, response.Body.String())
	}
	var effective deferrals.Snapshot
	if err = json.Unmarshal(response.Body.Bytes(), &effective); err != nil {
		t.Fatal(err)
	}
	if len(effective.Rules) != 2 || effective.Revision == "" || effective.Rules[0].ExpiresAtDisplay == "" {
		t.Fatal(effective)
	}
	response = request("GET", "/api/package-deferral-snapshots/"+effective.Revision, nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Investigating compatibility") {
		t.Fatal(response.Code, response.Body.String())
	}
	if _, err = app.Deps.PolicyRepository.SetOverride(policy.ID, server.Name, true); err != nil {
		t.Fatal(err)
	}
	snapshot, err := packageDeferralLoader(app.Deps)(server, "")
	if err != nil || len(snapshot.Rules) != 1 {
		t.Fatal(snapshot, err)
	}
	if _, err = app.Deps.PolicyRepository.SetOverride(policy.ID, server.Name, false); err != nil {
		t.Fatal(err)
	}
	// Inventory Save deletes/reinserts all Server rows. Deferrals have explicit
	// identity hooks and survive ordinary writes without FK cascade loss.
	if _, err = app.Deps.ServerInventoryService.Create(Server{Name: "other", Host: "other.invalid", User: "operator"}); err != nil {
		t.Fatal(err)
	}
	server.Name = "renamed-deferral-web"
	if _, err = app.Deps.ServerInventoryService.Update("deferral-web", server); err != nil {
		t.Fatal(err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, "")
	if err != nil || len(snapshot.Rules) != 2 {
		t.Fatal(snapshot, err)
	}
	if err = app.Deps.ServerInventoryService.Delete(server.Name); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Deps.ServerInventoryService.Create(server); err != nil {
		t.Fatal(err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, "")
	if err != nil || len(snapshot.Rules) != 1 || snapshot.Rules[0].PolicyID != policy.ID {
		t.Fatal(snapshot, err)
	}
	originJob, originErr := app.Deps.CurrentJobManager().CreateJob(jobs.CreateParams{Kind: jobs.KindUpdate, ServerName: server.Name, Status: jobs.StatusSucceeded, MetaJSON: jobs.MarshalJSON(scheduledJobMeta{Trigger: "scheduled", PolicyID: policy.ID, PolicyName: policy.Name, PolicyRevision: policy.Revision})})
	if originErr != nil {
		t.Fatal(originErr)
	}
	policy.IncludeTags = []string{"unmatched"}
	policy.TargetTag = ""
	policy, err = app.Deps.PolicyRepository.UpdatePolicy(policy.ID, policy)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, "")
	if err != nil || len(snapshot.Rules) != 0 {
		t.Fatal(snapshot, err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, originJob.ID)
	if err != nil || len(snapshot.Rules) != 1 {
		t.Fatal(snapshot, err)
	}
	// Policy deletion leaves live rule evidence available to already admitted
	// immutable manifests, while new manual discovery stops inheriting it.
	if err = app.Deps.PolicyRepository.DeletePolicy(policy.ID); err != nil {
		t.Fatal(err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, "")
	if err != nil || len(snapshot.Rules) != 0 {
		t.Fatal(snapshot, err)
	}
	snapshot, err = packageDeferralLoader(app.Deps)(server, originJob.ID)
	if err != nil || len(snapshot.Rules) != 1 || !strings.Contains(snapshot.Rules[0].Origin, "manifest revision") {
		t.Fatal(snapshot, err)
	}
	app.Deps.ServerState.Lock()
	active := app.Deps.ServerState.StatusMap()[server.Name]
	active.JobID = originJob.ID
	active.Status = "pending_approval"
	active.ActionRunning = true
	app.Deps.ServerState.Unlock()
	response = request("GET", "/api/servers/"+server.Name+"/package-deferrals", nil)
	if response.Code != 200 || !strings.Contains(response.Body.String(), "manifest revision") {
		t.Fatal(response.Code, response.Body.String())
	}
	app.Deps.ServerState.Lock()
	active.Status = "idle"
	active.ActionRunning = false
	active.JobID = ""
	app.Deps.ServerState.Unlock()
	if response = request("GET", "/api/package-deferrals", nil); response.Code != 200 {
		t.Fatal(response.Code)
	}
	for _, selector := range []string{"*", "openssl/stable", "--help", "../path", "openssl:amd64:extra"} {
		rule.Selector = selector
		rule.PolicyID = 0
		rule.ServerName = server.Name
		if response = request("POST", "/api/package-deferrals", rule); response.Code != 400 {
			t.Fatal(selector, response.Code)
		}
	}
	if response = request("GET", "/api/servers/missing/package-deferrals", nil); response.Code != 404 {
		t.Fatal(response.Code)
	}
	if response = request("DELETE", "/api/package-deferrals/missing", nil); response.Code != 404 {
		t.Fatal(response.Code)
	}
	// Complete history is retained after early deletion.
	if response = request("GET", "/api/package-deferral-snapshots/"+effective.Revision, nil); response.Code != 200 {
		t.Fatal(response.Code)
	}
}

func TestPackageDeferralSnapshotSurvivesLargeAuditPreview(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "deferral-volume.db")})
	now := time.Now().UTC()
	rules := []deferrals.Rule{}
	for i := range 24 {
		r, err := deferrals.Create(context.Background(), app.Deps.DB(), deferrals.Rule{ServerName: "web", Selector: fmt.Sprintf("package%d", i), Reason: strings.Repeat("Investigation ", 60), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}, "admin", now)
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, r)
	}
	snapshot, err := deferrals.Resolve(rules, "web", nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if err = deferrals.RecordSnapshot(context.Background(), app.Deps.DB(), snapshot); err != nil {
		t.Fatal(err)
	}
	// Use the app-scoped service, independent of the package-main audit fixture.
	_ = app.Deps.AuditService.Record("admin", "", "deferral.volume", "server", "web", "success", "Protected transaction", map[string]any{"package_deferrals": snapshot, "deferral_revision": snapshot.Revision, "deferred_package_count": 24, "deferral_rule_count": 24, "job_id": "fixture-job-with-full-plan"})
	var raw string
	if err = app.Deps.DB().QueryRow(`SELECT meta_json FROM audit_events WHERE action='deferral.volume' ORDER BY id DESC LIMIT 1`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) > 4096 || !strings.Contains(raw, snapshot.Revision) || !strings.Contains(raw, "fixture-job-with-full-plan") {
		t.Fatal(len(raw), raw)
	}
	restored, err := deferrals.LoadSnapshot(context.Background(), app.Deps.DB(), snapshot.Revision)
	if err != nil || len(restored.Rules) != 24 || restored.Rules[0].Reason != snapshot.Rules[0].Reason {
		t.Fatal(restored, err)
	}
}

func TestApprovalRejectsChangedDeferralIdentityBeforePersistence(t *testing.T) {
	state := newServerState()
	state.SetServers([]Server{{Name: "srv"}})
	state.SetStatusMap(map[string]*ServerStatus{"srv": {Name: "srv", Status: "pending_approval", JobID: "job", ApprovalGeneration: 1, UpgradePlan: serverpkg.UpgradePlan{PackageDeferrals: deferrals.Snapshot{Revision: "new"}}}})
	lifecycle := &serverActionLifecycle{serverState: state}
	result := lifecycle.withPendingApproval("srv", serverActionApprovalIdentity{JobID: "job", Generation: 1, DeferralRevision: "old"}, func(*ServerStatus) serverActionLifecycleResult {
		t.Fatal("stale rules accepted")
		return serverActionLifecycleResult{}
	})
	if result.statusCode != 409 {
		t.Fatal(result)
	}
}

func TestPackageDeferralBackupRestoreRehydratesRulesAndHistoricalSnapshot(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "deferral-restore.db"), Deps: AppDeps{DB: getDB}})
	server := Server{Name: "restored-deferral", Host: "example.invalid", User: "operator", Pass: "fixture-password"}
	seedUpdatePolicyTestInventory(t, app, []Server{server}, nil)
	now := time.Now().UTC()
	rule, err := deferrals.Create(context.Background(), app.Deps.DB(), deferrals.Rule{ServerName: server.Name, Selector: "openssl:amd64", Reason: "Restore continuity", ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}, "admin", now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := packageDeferralLoader(app.Deps)(server, "")
	if err != nil {
		t.Fatal(err)
	}
	database, err := app.Deps.BackupService.CreateDBSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if err = deferrals.Delete(context.Background(), app.Deps.DB(), rule.ID); err != nil {
		t.Fatal(err)
	}
	if err = app.Deps.BackupService.ApplyFiles(context.Background(), map[string][]byte{"servers.db": database, "config.json": config}); err != nil {
		t.Fatal(err)
	}
	restored, err := packageDeferralLoader(app.Deps)(server, "")
	if err != nil || restored.Revision != snapshot.Revision || len(restored.Rules) != 1 || restored.Rules[0].Author != "admin" {
		t.Fatal(restored, err)
	}
	history, err := deferrals.LoadSnapshot(context.Background(), app.Deps.DB(), snapshot.Revision)
	if err != nil || history.Rules[0].Reason != rule.Reason {
		t.Fatal(history, err)
	}
}
