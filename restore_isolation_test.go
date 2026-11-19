package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestRestoreIsolationResumeRequiresAdminConfirmationAndReauthentication(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 43, 0, time.UTC)
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{Now: func() time.Time { return now }})
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
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", confirmation); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"restore_isolation":"inactive"`) {
		t.Fatalf("resumption = %d %s", rec.Code, rec.Body.String())
	}
	isolation := app.Deps.RestoreIsolation
	minute := now.Truncate(time.Minute)
	resumeAt, stillBlocked := isolation.ScheduledWork(minute)
	if isolation.BlocksExternalWork() || !stillBlocked || !resumeAt.Equal(minute.Add(time.Minute)) {
		t.Fatal("resumption failed to fence the current schedule slot")
	}
	if mark, found, err := watermark.LoadSchedulerWatermark(); err != nil || !found || !mark.Equal(minute) {
		t.Fatalf("rebased mark = %v, %v, %v", mark, found, err)
	}
	var audited int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action=? AND status='success'", "backup.restore_isolation.resume").Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("resumption audit count = %d, %v", audited, err)
	}
}
