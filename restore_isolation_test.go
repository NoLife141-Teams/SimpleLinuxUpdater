package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	policypkg "debian-updater/internal/policies"

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
	for _, path := range []string{"/api/update/fixture", "/api/servers/fixture/facts/refresh", "/api/status/batches", "/api/notifications/test", "/api/hostkeys/scan"} {
		rec := identityRequest(app, "POST", path, admin, "", gin.H{})
		if rec.Code != 409 || !strings.Contains(rec.Body.String(), "restore_isolated") {
			t.Fatalf("%s while isolated: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	diagnostics := identityRequest(app, "GET", "/api/installation/health", admin, "", nil)
	if diagnostics.Code != 200 || !strings.Contains(diagnostics.Body.String(), `"restore_isolation":"active"`) {
		t.Fatalf("diagnostics while isolated: %d %s", diagnostics.Code, diagnostics.Body.String())
	}
	review := identityRequest(app, "GET", "/api/update-policies", admin, "", nil)
	if review.Code != 200 {
		t.Fatalf("local policy review while isolated: %d %s", review.Code, review.Body.String())
	}
	confirmation := gin.H{"confirmation": "RESUME"}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, operator, "", confirmation); rec.Code != 403 {
		t.Fatalf("operator resumption status = %d", rec.Code)
	}
	if rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", gin.H{}); rec.Code != 400 {
		t.Fatalf("unconfirmed resumption status = %d", rec.Code)
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
	if isolation.BlocksExternalWork() || !isolation.BlocksScheduledWork(minute) || isolation.BlocksScheduledWork(minute.Add(time.Minute)) {
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

func TestPolicySchedulerIgnoresIsolatedAndPreResumeTicks(t *testing.T) {
	at := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	database := filepath.Join(t.TempDir(), "servers.db")
	isolation := internalbackup.NewRestoreIsolation(func() string { return database })
	accepted := 0
	service := policypkg.NewService(policypkg.ServiceDeps{
		BlocksScheduledRun: isolation.BlocksScheduledWork,
		ListPolicies: func() ([]policypkg.Policy, error) {
			accepted++
			return nil, nil
		},
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return nil, nil },
		LoadGlobalBlackouts: func() ([]policypkg.BlackoutWindow, error) { return nil, nil },
		SnapshotServers:     func() []Server { return nil },
		HandleScheduledRun: func(policypkg.ScheduledRunRequest) policypkg.ScheduledRunResult {
			return policypkg.ScheduledRunResult{}
		},
	})
	if err := isolation.Activate(); err != nil {
		t.Fatal(err)
	}
	store := policypkg.SchedulerWatermarkStore{}
	if err := service.ProcessDueWithRecovery(at, store); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessDue(at); err != nil {
		t.Fatal(err)
	}
	if err := isolation.Reactivate(at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := service.ProcessDueWithRecovery(at, store); err != nil {
		t.Fatal(err)
	}
	if accepted != 0 {
		t.Fatalf("scheduler evaluated %d isolated or old ticks", accepted)
	}
	if err := service.ProcessDueWithRecovery(at.Add(time.Minute), store); err != nil {
		t.Fatal(err)
	}
	if accepted != 1 {
		t.Fatalf("expected one evaluation after release, got %d", accepted)
	}
}
