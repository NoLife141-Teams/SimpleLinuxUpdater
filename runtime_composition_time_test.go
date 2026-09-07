package main

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	policypkg "debian-updater/internal/policies"
)

func TestReloadRestoredStateRejectsInvalidTimezoneBeforeSchedulerCheckpoint(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "restore-invalid-time.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	repo := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{DB: func() *sql.DB { return db }})
	oldWatermark := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	if err := repo.SaveSchedulerWatermark(oldWatermark); err != nil {
		t.Fatal(err)
	}
	store := apptimepkg.NewMemoryStore("UTC")
	module := apptimepkg.New(apptimepkg.Deps{Store: store})
	if err := module.Initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(t.Context(), "invalid/timezone"); err != nil {
		t.Fatal(err)
	}
	notifications := &reloadTrackingNotificationLifecycle{}
	composition := newRuntimeComposition(AppDeps{
		DB:                  func() *sql.DB { return db },
		ApplicationTime:     module,
		PolicyRepository:    repo,
		NotificationService: notifications,
		Now:                 func() time.Time { return oldWatermark.Add(time.Hour) },
	})
	composition.resetCaches = func() {}
	err = composition.ReloadRestoredState(t.Context())
	if err == nil || !strings.Contains(err.Error(), "reload restored Application Time Interpretation") {
		t.Fatalf("restore error = %v, want labeled timezone failure", err)
	}
	if notifications.reloads != 0 {
		t.Fatal("restore continued rehydrating services after timezone failure")
	}
	watermark, found, err := repo.LoadSchedulerWatermark()
	if err != nil || !found || !watermark.Equal(oldWatermark) {
		t.Fatalf("watermark = %v, %t, %v; must not advance on failed restore", watermark, found, err)
	}
	if got := module.Current().ResolvedName; got != "UTC" {
		t.Fatalf("invalid restored timezone replaced valid interpretation: %q", got)
	}
}
