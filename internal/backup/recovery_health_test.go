package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestRecoveryHealthProjectsHealthyStaleNeverAndFailedEvidence(t *testing.T) {
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		seed             []recoveryHealthSeed
		wantState        string
		wantExportState  string
		wantVerifyState  string
		wantExportSize   int64
		wantVerifySize   int64
		wantExportAt     string
		wantVerification string
	}{
		{
			name: "healthy",
			seed: []recoveryHealthSeed{
				{action: "backup.export", status: "success", at: now.Add(-24 * time.Hour), message: "Backup exported", meta: map[string]any{"bytes": 4096}},
				{action: "backup.verify", status: "success", at: now.Add(-12 * time.Hour), message: "Backup restore readiness reviewed", meta: map[string]any{"restore_ready": true, "archive_size_bytes": 8192}},
			},
			wantState:        RecoveryStateHealthy,
			wantExportState:  RecoveryStateHealthy,
			wantVerifyState:  RecoveryStateHealthy,
			wantExportSize:   4096,
			wantVerifySize:   8192,
			wantExportAt:     now.Add(-24 * time.Hour).Format(time.RFC3339),
			wantVerification: now.Add(-12 * time.Hour).Format(time.RFC3339),
		},
		{
			name: "stale",
			seed: []recoveryHealthSeed{
				{action: "backup.export", status: "success", at: now.Add(-8 * 24 * time.Hour), message: "Backup exported", meta: map[string]any{"bytes": 2048}},
				{action: "backup.verify", status: "success", at: now.Add(-24 * time.Hour), message: "Backup restore readiness reviewed", meta: map[string]any{"restore_ready": true, "archive_size_bytes": 3072}},
			},
			wantState:        RecoveryStateStale,
			wantExportState:  RecoveryStateStale,
			wantVerifyState:  RecoveryStateHealthy,
			wantExportSize:   2048,
			wantVerifySize:   3072,
			wantExportAt:     now.Add(-8 * 24 * time.Hour).Format(time.RFC3339),
			wantVerification: now.Add(-24 * time.Hour).Format(time.RFC3339),
		},
		{
			name:            "never",
			wantState:       RecoveryStateNever,
			wantExportState: RecoveryStateNever,
			wantVerifyState: RecoveryStateNever,
		},
		{
			name: "failed retains the last successful export",
			seed: []recoveryHealthSeed{
				{action: "backup.export", status: "failure", at: now.Add(-time.Hour), message: "Failed to build backup payload"},
				{action: "backup.export", status: "success", at: now.Add(-24 * time.Hour), message: "Backup exported", meta: map[string]any{"bytes": 1024}},
				{action: "backup.verify", status: "success", at: now.Add(-2 * time.Hour), message: "Backup restore readiness reviewed", meta: map[string]any{"restore_ready": true, "archive_size_bytes": 1536}},
			},
			wantState:        RecoveryStateFailed,
			wantExportState:  RecoveryStateFailed,
			wantVerifyState:  RecoveryStateHealthy,
			wantExportSize:   1024,
			wantVerifySize:   1536,
			wantExportAt:     now.Add(-24 * time.Hour).Format(time.RFC3339),
			wantVerification: now.Add(-2 * time.Hour).Format(time.RFC3339),
		},
		{
			name: "not-ready verification is failed evidence",
			seed: []recoveryHealthSeed{
				{action: "backup.export", status: "success", at: now.Add(-time.Hour), message: "Backup exported", meta: map[string]any{"bytes": 512}},
				{action: "backup.verify", status: "success", at: now.Add(-30 * time.Minute), message: "Backup restore readiness reviewed", meta: map[string]any{"restore_ready": false, "archive_size_bytes": 768}},
			},
			wantState:       RecoveryStateFailed,
			wantExportState: RecoveryStateHealthy,
			wantVerifyState: RecoveryStateFailed,
			wantExportSize:  512,
			wantExportAt:    now.Add(-time.Hour).Format(time.RFC3339),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRecoveryHealthTestDB(t)
			seedRecoveryHealthFacts(t, db, tt.seed)
			service := NewService(ServiceDeps{
				DB:                            func() *sql.DB { return db },
				Now:                           func() time.Time { return now },
				RecoveryStaleAfter:            7 * 24 * time.Hour,
				RecoveryEvidenceRetentionDays: 90,
			})

			got, err := service.RecoveryHealth(context.Background())
			if err != nil {
				t.Fatalf("RecoveryHealth() error = %v", err)
			}
			if got.State != tt.wantState || got.Export.State != tt.wantExportState || got.Verification.State != tt.wantVerifyState {
				t.Fatalf("RecoveryHealth() states = %q/%q/%q, want %q/%q/%q", got.State, got.Export.State, got.Verification.State, tt.wantState, tt.wantExportState, tt.wantVerifyState)
			}
			if got.Export.LastSuccessAt != tt.wantExportAt || got.Verification.LastSuccessAt != tt.wantVerification {
				t.Fatalf("RecoveryHealth() success times = %q/%q, want %q/%q", got.Export.LastSuccessAt, got.Verification.LastSuccessAt, tt.wantExportAt, tt.wantVerification)
			}
			assertRecoverySize(t, "export", got.Export.SizeBytes, tt.wantExportSize)
			assertRecoverySize(t, "verification", got.Verification.SizeBytes, tt.wantVerifySize)
			if got.StaleAfterHours != 168 || got.Retention.EvidenceDays != 90 {
				t.Fatalf("RecoveryHealth() threshold/retention = %d/%d, want 168/90", got.StaleAfterHours, got.Retention.EvidenceDays)
			}
			if got.Schedule.Scheduled || got.Schedule.NextBackupAt != "" || got.Schedule.Message != "No backup is scheduled." {
				t.Fatalf("RecoveryHealth() schedule = %+v, want explicit unscheduled state", got.Schedule)
			}
			if got.Retention.ArchiveRetained || got.Retention.AutomaticDeletion {
				t.Fatalf("RecoveryHealth() retention = %+v, app must not retain or delete archives", got.Retention)
			}
		})
	}
}

func TestRecoveryHealthReturnsUnavailableWhenEvidenceCannotLoad(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "missing-audit-schema.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	service := NewService(ServiceDeps{DB: func() *sql.DB { return db }, Now: func() time.Time { return now }})

	got, err := service.RecoveryHealth(context.Background())
	if err == nil {
		t.Fatal("RecoveryHealth() error = nil, want unavailable evidence error")
	}
	if got.State != RecoveryStateUnavailable || got.Export.State != RecoveryStateUnavailable || got.Verification.State != RecoveryStateUnavailable {
		t.Fatalf("RecoveryHealth() unavailable states = %+v", got)
	}
}

type recoveryHealthSeed struct {
	action  string
	status  string
	at      time.Time
	message string
	meta    map[string]any
}

func newRecoveryHealthTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "recovery-health.db"))
	if err != nil {
		t.Fatalf("open recovery health database: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT NOT NULL,
			meta_json TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("create audit_events: %v", err)
	}
	return db
}

func seedRecoveryHealthFacts(t *testing.T, db *sql.DB, seeds []recoveryHealthSeed) {
	t.Helper()
	for i := len(seeds) - 1; i >= 0; i-- {
		meta, err := json.Marshal(seeds[i].meta)
		if err != nil {
			t.Fatalf("marshal recovery metadata: %v", err)
		}
		if _, err := db.Exec(`
			INSERT INTO audit_events(created_at, action, target_type, status, message, meta_json)
			VALUES (?, ?, 'backup', ?, ?, ?)
		`, seeds[i].at.UTC().Format(time.RFC3339), seeds[i].action, seeds[i].status, seeds[i].message, string(meta)); err != nil {
			t.Fatalf("seed recovery evidence: %v", err)
		}
	}
}

func assertRecoverySize(t *testing.T, label string, got *int64, want int64) {
	t.Helper()
	if want == 0 {
		if got != nil {
			t.Fatalf("%s size = %d, want unavailable", label, *got)
		}
		return
	}
	if got == nil || *got != want {
		t.Fatalf("%s size = %v, want %d", label, got, want)
	}
}
