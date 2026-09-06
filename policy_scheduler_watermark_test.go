package main

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"

	_ "modernc.org/sqlite"
)

func TestStartPolicySchedulerUsesProvidedRepositoryForWatermark(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "app-scoped-policy.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	if err := policypkg.EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	repository := policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{
		DB:        func() *sql.DB { return db },
		NowString: func() string { return "2026-09-06T15:00:00.000000000Z" },
	})
	now := time.Date(2026, 9, 6, 15, 0, 0, 0, time.UTC)
	service := NewPolicyService(PolicyServiceDeps{
		ListPolicies:        func() ([]UpdatePolicy, error) { return nil, nil },
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil },
		LoadGlobalBlackouts: func() ([]UpdatePolicyBlackoutWindow, error) { return nil, nil },
		SnapshotServers:     func() []Server { return nil },
		HandleScheduledRun:  func(PolicyScheduledRunRequest) PolicyScheduledRunResult { return PolicyScheduledRunResult{} },
		CurrentLocation:     func() *time.Location { return time.UTC },
		MarkInterruptedRuns: func() error { return nil },
		Now:                 func() time.Time { return now },
		Logf:                func(string, ...any) {},
	})

	ctx, cancel := context.WithCancel(context.Background())
	startPolicyScheduler(service, repository, ctx, PolicySchedulerOptions{TickInterval: time.Hour})
	cancel()
	service.WaitScheduler()

	got, found, err := repository.LoadSchedulerWatermark()
	if err != nil {
		t.Fatalf("LoadSchedulerWatermark() error = %v", err)
	}
	if !found || !got.Equal(now) {
		t.Fatalf("app-scoped watermark = %v found=%t, want %v true", got, found, now)
	}
}
