package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func reviewRestoreResumption(t *testing.T, app *testApp, admin *http.Cookie) restoreResumePreview {
	t.Helper()
	rec := identityRequest(app, "GET", restoreResumePreviewRoute, admin, "", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("restore resumption preview = %d %s", rec.Code, rec.Body.String())
	}
	var preview restoreResumePreview
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || preview.Token == "" {
		t.Fatalf("invalid resumption preview: %v", err)
	}
	return preview
}

func confirmReviewedRestoration(t *testing.T, app *testApp, admin *http.Cookie) gin.H {
	t.Helper()
	return gin.H{"confirmation": "RESUME", "preview_token": reviewRestoreResumption(t, app, admin).Token}
}

func TestRestoreIsolationResumeRequiresAdminConfirmationAndReauthentication(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 59, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(now.Unix())
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
		Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
		NotifyDashboardEvent: func(action string) {
			if action == "backup.restore_isolation.resume" {
				clock.Store(now.Add(2 * time.Second).Unix())
			}
		},
	})
	admin := app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("operator", testPasswordStrong, "operator"); err != nil {
		t.Fatal(err)
	}
	operator := identityLogin(t, app, "operator")
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	confirmation := gin.H{"confirmation": "RESUME"}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, operator, "", confirmation); rec.Code != 403 {
		t.Fatalf("operator resumption status = %d", rec.Code)
	}
	session := app.Deps.CurrentSessionManager()
	ctx, err := session.Load(t.Context(), admin.Value)
	if err != nil {
		t.Fatal(err)
	}
	session.Put(ctx, "auth_reauthenticated_at", time.Now().Add(-6*time.Minute).Unix())
	if _, _, err := session.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", confirmation); rec.Code != 401 || !strings.Contains(rec.Body.String(), "reauthentication_required") {
		t.Fatalf("stale reauthentication status = %d %s", rec.Code, rec.Body.String())
	}
	if !app.Deps.RestoreIsolation.BlocksExternalWork() {
		t.Fatal("denied confirmation released isolation")
	}
	watermark := app.Deps.PolicyRepository.(policySchedulerWatermarkRepository)
	if err := watermark.SaveSchedulerWatermark(now.Add(-2 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	if rec := identityRequest(app, "POST", "/api/auth/reauthenticate", admin, "", gin.H{"password": testPasswordStrong}); rec.Code != 200 {
		t.Fatalf("reauthentication = %d %s", rec.Code, rec.Body.String())
	}
	confirmation = confirmReviewedRestoration(t, app, admin)
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", confirmation); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"restore_isolation":"inactive"`) {
		t.Fatalf("resumption = %d %s", rec.Code, rec.Body.String())
	}
	isolation := app.Deps.RestoreIsolation
	minute := now.Truncate(time.Minute)
	resumeAt, stillBlocked := isolation.ScheduledWork(minute.Add(time.Minute))
	if isolation.BlocksExternalWork() || !stillBlocked || !resumeAt.Equal(minute.Add(2*time.Minute)) {
		t.Fatal("resumption failed to fence a schedule slot that began during the audit")
	}
	if mark, found, err := watermark.LoadSchedulerWatermark(); err != nil || !found || !mark.Equal(minute) {
		t.Fatalf("rebased mark = %v, %v, %v", mark, found, err)
	}
	var audited int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action=? AND status='success'", "backup.restore_isolation.resume").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("resumption audit count = %d, %v", audited, err)
	}
}

func TestRestoreResumptionRejectsChangedAndExpiredPreview(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
		Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
	})
	admin := app.authenticate(t)
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	assertRejected := func(token string) restoreResumePreview {
		t.Helper()
		rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "",
			gin.H{"confirmation": "RESUME", "preview_token": token})
		var result struct {
			Code    string               `json:"code"`
			Preview restoreResumePreview `json:"preview"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil ||
			rec.Code != http.StatusConflict || result.Code != "restore_resume_preview_changed" ||
			!app.Deps.RestoreIsolation.BlocksExternalWork() {
			t.Fatalf("resume with outdated preview = %d %s", rec.Code, rec.Body.String())
		}
		return result.Preview
	}
	initial := reviewRestoreResumption(t, app, admin)
	if _, err := app.Deps.ServerInventoryService.Create(Server{
		Name: "added-after-review", Host: "private-address.example.test", User: "root",
	}); err != nil {
		t.Fatal(err)
	}
	changed := assertRejected(initial.Token)
	if len(changed.Impact.Hosts) != 1 || changed.Impact.Hosts[0].Name != "added-after-review" {
		t.Fatalf("updated impact omitted the new server: %+v", changed.Impact.Hosts)
	}
	clock.Store(changed.ExpiresAt.Unix())
	current := assertRejected(changed.Token)
	rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "",
		gin.H{"confirmation": "RESUME", "preview_token": current.Token})
	if rec.Code != http.StatusOK {
		t.Fatalf("resume with current preview = %d %s", rec.Code, rec.Body.String())
	}
}
