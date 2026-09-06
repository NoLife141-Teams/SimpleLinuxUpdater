package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"

	_ "modernc.org/sqlite"
)

func TestRuntimeCompositionReloadRestoredStateRebasesSchedulerWatermark(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "restore-scheduler-watermark.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	fixedNow := time.Date(2026, 9, 6, 16, 20, 47, 0, time.UTC)
	composition := newRuntimeComposition(AppDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return dbPath },
		Now:    func() time.Time { return fixedNow },
	})
	composition.resetCaches = func() {}
	deps := composeRuntimeForTest(t, composition)
	repository, ok := deps.PolicyRepository.(*policypkg.SQLiteRepository)
	if !ok {
		t.Fatalf("PolicyRepository = %T, want *policies.SQLiteRepository", deps.PolicyRepository)
	}
	oldWatermark := fixedNow.Add(-14 * 24 * time.Hour)
	if err := repository.SaveSchedulerWatermark(oldWatermark); err != nil {
		t.Fatalf("seed scheduler watermark: %v", err)
	}

	if err := composition.ReloadRestoredState(t.Context()); err != nil {
		t.Fatalf("ReloadRestoredState() error = %v", err)
	}
	got, found, err := repository.LoadSchedulerWatermark()
	if err != nil {
		t.Fatalf("LoadSchedulerWatermark() error = %v", err)
	}
	want := fixedNow.UTC().Truncate(time.Minute)
	if !found || !got.Equal(want) {
		t.Fatalf("restored scheduler watermark = %v, found=%t; want %v, true", got, found, want)
	}
}
