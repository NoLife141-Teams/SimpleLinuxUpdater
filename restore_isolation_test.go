package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	notificationpkg "debian-updater/internal/notifications"
	policypkg "debian-updater/internal/policies"

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

func TestRestoreResumptionRequiresCompleteInputsAndInvalidatesOldReview(t *testing.T) {
	for _, section := range []string{"hosts", "schedules", "notifications"} {
		t.Run(section, func(t *testing.T) {
			now := time.Now().UTC().Truncate(time.Second)
			app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
				Now: func() time.Time { return now },
			})
			admin := app.authenticate(t)
			if err := app.Deps.RestoreIsolation.Activate(); err != nil {
				t.Fatal(err)
			}
			empty := reviewRestoreResumption(t, app, admin)
			if !empty.Complete || empty.Impact.Hosts == nil || empty.Impact.Schedules == nil || empty.Impact.Notifications == nil ||
				len(empty.Impact.Hosts)+len(empty.Impact.Schedules)+len(empty.Impact.Notifications) != 0 {
				t.Fatalf("empty inputs were not reviewable: %+v", empty)
			}
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "fixture-host", Host: "fixture.example.test", User: "root"})
			if err != nil {
				t.Fatal(err)
			}
			db := app.Deps.DB()
			exec := func(query string, args ...any) {
				t.Helper()
				if _, err := db.Exec(query, args...); err != nil {
					t.Fatal(err)
				}
			}
			exec(`INSERT INTO restore_apt_validation(identity,target,host_key,result,checked_at)
				VALUES (?,?,?,'healthy',?)`, server.Identity, server.APTTarget(), "fixture-key", now.Unix())
			watermark := app.Deps.PolicyRepository.(policySchedulerWatermarkRepository)
			before := now.Truncate(time.Minute).Add(-time.Hour)
			if err := watermark.SaveSchedulerWatermark(before); err != nil {
				t.Fatal(err)
			}
			complete := reviewRestoreResumption(t, app, admin)
			switch section {
			case "hosts":
				exec("ALTER TABLE restore_apt_validation RENAME TO unavailable_apt")
			case "schedules":
				exec("ALTER TABLE update_policies RENAME TO unavailable_policies")
			case "notifications":
				exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", notificationpkg.SettingsKey, "corrupted-settings")
			}
			assertIncomplete := func(preview restoreResumePreview) {
				t.Helper()
				impact := preview.Impact
				if preview.Complete || preview.Token != "" || !preview.ExpiresAt.IsZero() ||
					(impact.Hosts == nil) != (section == "hosts") ||
					(impact.Schedules == nil) != (section == "schedules") ||
					(impact.Notifications == nil) != (section == "notifications") {
					t.Fatalf("invalid partial preview for %s: %+v", section, preview)
				}
			}
			for range 2 {
				rec := identityRequest(app, "GET", restoreResumePreviewRoute, admin, "", nil)
				var preview restoreResumePreview
				if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil || rec.Code != http.StatusOK {
					t.Fatalf("partial review = %d %s", rec.Code, rec.Body.String())
				}
				assertIncomplete(preview)
			}
			send := func(token string) *httptest.ResponseRecorder {
				t.Helper()
				return identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "",
					gin.H{"confirmation": "RESUME", "preview_token": token})
			}
			blocked := send(complete.Token)
			var response struct {
				Code    string               `json:"code"`
				Preview restoreResumePreview `json:"preview"`
			}
			if err := json.Unmarshal(blocked.Body.Bytes(), &response); err != nil ||
				blocked.Code != http.StatusConflict || response.Code != "restore_resume_preview_incomplete" ||
				!app.Deps.RestoreIsolation.BlocksExternalWork() {
				t.Fatalf("confirmation during outage = %d %s", blocked.Code, blocked.Body.String())
			}
			assertIncomplete(response.Preview)
			switch section {
			case "hosts":
				exec("ALTER TABLE unavailable_apt RENAME TO restore_apt_validation")
			case "schedules":
				exec("ALTER TABLE unavailable_policies RENAME TO update_policies")
			case "notifications":
				exec("DELETE FROM settings WHERE key=?", notificationpkg.SettingsKey)
			}
			if got, found, err := watermark.LoadSchedulerWatermark(); err != nil || !found || !got.Equal(before) {
				t.Fatalf("watermark changed during outage: %v, %v, %v", got, found, err)
			}
			var approved, audits int
			if err := db.QueryRow("SELECT approved FROM restore_apt_validation WHERE identity=?", server.Identity).Scan(&approved); err != nil || approved != 0 {
				t.Fatalf("APT approval changed during outage: %d, %v", approved, err)
			}
			if err := db.QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.restore_isolation.resume'").Scan(&audits); err != nil || audits != 0 {
				t.Fatalf("rejected preview recorded a resumption decision: %d, %v", audits, err)
			}
			if stale := send(complete.Token); stale.Code != http.StatusConflict ||
				!strings.Contains(stale.Body.String(), "restore_resume_preview_changed") {
				t.Fatalf("old preview accepted after recovery: %d %s", stale.Code, stale.Body.String())
			}
			if fresh := send(reviewRestoreResumption(t, app, admin).Token); fresh.Code != http.StatusOK ||
				app.Deps.RestoreIsolation.BlocksExternalWork() {
				t.Fatalf("fresh complete preview rejected: %d %s", fresh.Code, fresh.Body.String())
			}
		})
	}
}

func TestRestoreResumptionProjectsFromCurrentInventoryWithoutWritingAndRechecksLateSlots(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 58, 0, time.UTC)
	nextSlot := now.Truncate(time.Minute).Add(time.Minute)
	var clock atomic.Int64
	clock.Store(now.Unix())
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
		Now: func() time.Time { return time.Unix(clock.Load(), 0) },
		NotifyDashboardEvent: func(action string) {
			if action == "backup.restore_isolation.resume" {
				clock.Store(nextSlot.Unix())
			}
		},
	})
	admin := app.authenticate(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{
		Name: "current-host", Host: "current.example.test", User: "root", Tags: []string{"maintenance"},
	})
	if err != nil {
		t.Fatal(err)
	}
	policy, err := app.Deps.PolicyRepository.CreatePolicy(UpdatePolicy{
		Name: "Nightly", Enabled: true, TargetTag: "maintenance",
		ExecutionMode: policypkg.ExecutionScanOnly, PackageScope: policypkg.PackageScopeSecurity,
		CadenceKind: policypkg.CadenceDaily, TimeLocal: nextSlot.In(app.Deps.ApplicationTime.Current().Location).Format("15:04"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, inserted, err := app.Deps.PolicyRepository.CreateRun(policypkg.Run{
		PolicyID: policy.ID, PolicyName: policy.Name, ServerName: "deleted-host",
		ScheduledForUTC: nextSlot.Format(jobTimestampLayout), Status: policypkg.RunSkipped,
	}); err != nil || !inserted {
		t.Fatalf("set up historical scheduled run: %v, %v", inserted, err)
	}
	if _, err := app.Deps.DB().Exec("DROP TABLE IF EXISTS " + policypkg.SchedulerStateRevisionTable); err != nil {
		t.Fatal(err)
	}
	watermark := app.Deps.PolicyRepository.(policySchedulerWatermarkRepository)
	originalMark := now.Truncate(time.Minute).Add(-time.Hour)
	if err := watermark.SaveSchedulerWatermark(originalMark); err != nil {
		t.Fatal(err)
	}
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	preview := reviewRestoreResumption(t, app, admin)
	var revisionTables int
	err = app.Deps.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", policypkg.SchedulerStateRevisionTable).Scan(&revisionTables)
	if err != nil || revisionTables != 0 {
		t.Fatalf("preview created scheduler revision storage: %d, %v", revisionTables, err)
	}
	if len(preview.Impact.Schedules) != 1 || preview.Impact.Schedules[0].Server != server.Name ||
		preview.Impact.Schedules[0].At != nextSlot.Format(jobTimestampLayout) {
		t.Fatalf("preview schedule = %+v", preview.Impact.Schedules)
	}
	rec := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "",
		gin.H{"confirmation": "RESUME", "preview_token": preview.Token})
	if rec.Code != http.StatusConflict || !app.Deps.RestoreIsolation.BlocksExternalWork() ||
		!strings.Contains(rec.Body.String(), nextSlot.AddDate(0, 0, 1).Format(jobTimestampLayout)) {
		t.Fatalf("resumption across the reviewed slot returned %d %s", rec.Code, rec.Body.String())
	}
	if mark, found, err := watermark.LoadSchedulerWatermark(); err != nil || !found || !mark.Equal(originalMark) {
		t.Fatalf("an expired approval rebased the scheduler: %v, %v, %v", mark, found, err)
	}
	rows, err := app.Deps.DB().Query("SELECT status FROM audit_events WHERE action='backup.restore_isolation.resume'")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var status string
	if !rows.Next() || rows.Scan(&status) != nil || status != "approved" || rows.Next() || rows.Err() != nil {
		t.Fatalf("an expired approval was recorded as a successful resumption: %q, %v", status, rows.Err())
	}
}
