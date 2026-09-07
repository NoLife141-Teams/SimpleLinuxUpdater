package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestAppScopedBackupRestoreReloadsApplicationTime(t *testing.T) {
	app := newUpdatePolicyTestApp(t, filepath.Join(t.TempDir(), "restore-timezone.db"))
	module := app.Deps.ApplicationTime
	if _, err := module.Configure(t.Context(), "UTC"); err != nil {
		t.Fatal(err)
	}
	// Include the actual encrypted inventory and config in the restored files.
	seedUpdatePolicyTestInventory(t, app, []Server{{Name: "restored", Host: "example.org", Port: 22, User: "root", Pass: "test-password"}}, nil)
	dbSnapshot, err := createDBBackupSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := module.Configure(t.Context(), "America/Toronto"); err != nil {
		t.Fatal(err)
	}
	if err := app.Deps.BackupService.ApplyFiles(context.Background(), map[string][]byte{
		"servers.db": dbSnapshot, "config.json": config,
	}); err != nil {
		t.Fatal(err)
	}
	stored, err := getSettingValue(appTimezoneSetting)
	if err != nil || stored != "UTC" {
		t.Fatalf("persisted timezone = %q, %v; want UTC", stored, err)
	}
	if got := module.Current().ResolvedName; got != stored {
		t.Errorf("runtime timezone = %q, want restored %q without restart", got, stored)
	}
	if got := app.Deps.PolicyService.EnsureDeps().CurrentLocation(); got.String() != "UTC" {
		t.Errorf("scheduler timezone = %s, want UTC", got)
	}
	policy := UpdatePolicy{CadenceKind: updatePolicyCadenceDaily, TimeLocal: "09:00"}
	if !app.Deps.PolicyService.PolicyDueAt(policy, time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC).In(app.Deps.PolicyService.EnsureDeps().CurrentLocation())) {
		t.Error("restored UTC policy must be due at 09:00 UTC")
	}
	// Use the existing router and its captured module after session replacement.
	app.Handler = app.Deps.CurrentSessionManager().LoadAndSave(app.Router)
	cookie := app.authenticate(t)
	req := httptest.NewRequest(http.MethodGet, "/api/app-settings/timezone", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	var response AppTimezoneResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if rec.Code != http.StatusOK || response.ResolvedTimezone != "UTC" {
		t.Fatalf("restored timezone API = %d %s", rec.Code, rec.Body.String())
	}
}
