package updates

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestServerFactsRepositorySchemaAndRoundTrip(t *testing.T) {
	db, repo := openServerFactsTestRepository(t, "server-facts.db")
	for i := 0; i < 2; i++ {
		if err := EnsureServerFactsSchema(db); err != nil {
			t.Fatalf("EnsureServerFactsSchema run %d error = %v", i+1, err)
		}
	}
	rebootRequired := true
	record := ServerFactsRecord{
		ServerName:     "srv-a",
		CollectedAt:    "2026-05-18T12:00:00Z",
		OSPrettyName:   "Ubuntu 24.04",
		UptimeSeconds:  42,
		DiskStatus:     "ok",
		DiskFreeKB:     1234,
		DiskTotalKB:    5678,
		DiskDetails:    "disk ok",
		AptStatus:      "ok",
		AptDetails:     "apt ok",
		RebootRequired: &rebootRequired,
		RawJSON:        `{"source":"test"}`,
	}
	if err := repo.Save(record); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := repo.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	got := loaded["srv-a"]
	if got.ServerName != record.ServerName || got.OSPrettyName != record.OSPrettyName || got.RawJSON != record.RawJSON || got.DiskTotalKB != record.DiskTotalKB {
		t.Fatalf("loaded record = %+v, want %+v", got, record)
	}
	if got.RebootRequired == nil || !*got.RebootRequired {
		t.Fatalf("reboot_required = %v, want true", got.RebootRequired)
	}
}

func TestServerFactsRepositoryRenameAndDeleteTx(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "server-facts-tx.db")
	if err := repo.Save(ServerFactsRecord{ServerName: "old", CollectedAt: "2026-05-18T12:00:00Z"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tx, err := repo.dbConn().Begin()
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	if err := repo.RenameServerTx(tx, "old", "new"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("RenameServerTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("rename commit error = %v", err)
	}
	loaded, err := repo.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() after rename error = %v", err)
	}
	if _, ok := loaded["new"]; !ok {
		t.Fatalf("renamed record missing: %+v", loaded)
	}
	snapshots, err := repo.ListHealthSnapshots("2026-05-01T00:00:00Z", "2026-05-30T00:00:00Z", "new")
	if err != nil {
		t.Fatalf("ListHealthSnapshots() after rename error = %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].ServerName != "new" {
		t.Fatalf("snapshots after rename = %+v, want one renamed snapshot", snapshots)
	}
	tx, err = repo.dbConn().Begin()
	if err != nil {
		t.Fatalf("Begin delete tx error = %v", err)
	}
	if err := repo.DeleteServerTx(tx, "new"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("DeleteServerTx() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("delete commit error = %v", err)
	}
	loaded, err = repo.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() after delete error = %v", err)
	}
	if len(loaded) != 0 {
		t.Fatalf("records after delete = %+v, want empty", loaded)
	}
	snapshots, err = repo.ListHealthSnapshots("2026-05-01T00:00:00Z", "2026-05-30T00:00:00Z", "")
	if err != nil {
		t.Fatalf("ListHealthSnapshots() after delete error = %v", err)
	}
	if len(snapshots) != 0 {
		t.Fatalf("snapshots after delete = %+v, want empty", snapshots)
	}
}

func TestServerFactsRepositoryDefaultsCollectedAt(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "server-facts-default-time.db")
	now := time.Date(2026, 5, 18, 12, 34, 56, 0, time.UTC)
	repo.Now = func() time.Time { return now }
	if err := repo.Save(ServerFactsRecord{ServerName: "srv-time"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	loaded, err := repo.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	if got := loaded["srv-time"].CollectedAt; got != "2026-05-18T12:34:56Z" {
		t.Fatalf("CollectedAt = %q, want default timestamp", got)
	}
}

func TestServerFactsRepositoryHealthSnapshotsAndRetention(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "server-health-snapshots.db")
	rebootRequired := true
	if err := repo.saveHealthSnapshot(HealthSnapshotRecord{
		ServerName:       "srv-a",
		CapturedAt:       "2026-05-18T12:00:00Z",
		Source:           "audit",
		PackageCount:     4,
		SecurityCount:    2,
		LastUpdateStatus: "failure",
		DiskStatus:       "critical",
		DiskFreeKB:       128,
		DiskTotalKB:      1024,
		AptStatus:        "ok",
		RebootRequired:   &rebootRequired,
		OSPrettyName:     "Debian",
		RawJSON:          `{"source":"test"}`,
	}); err != nil {
		t.Fatalf("saveHealthSnapshot() error = %v", err)
	}
	snapshots, err := repo.ListHealthSnapshots("2026-05-18T00:00:00Z", "2026-05-19T00:00:00Z", "srv-a")
	if err != nil {
		t.Fatalf("ListHealthSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshots))
	}
	got := snapshots[0]
	if got.PackageCount != 4 || got.SecurityCount != 2 || got.LastUpdateStatus != "failure" || got.DiskFreeKB != 128 {
		t.Fatalf("snapshot = %+v, want captured health and package data", got)
	}
	if got.RebootRequired == nil || !*got.RebootRequired {
		t.Fatalf("snapshot reboot_required = %v, want true", got.RebootRequired)
	}

	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	repo.Now = func() time.Time { return now }
	if _, err := repo.dbConn().Exec(
		"UPDATE settings SET value = ? WHERE key = ?",
		"1",
		HealthSnapshotRetentionSettingKey,
	); err != nil {
		t.Fatalf("update retention setting: %v", err)
	}
	if err := repo.saveHealthSnapshot(HealthSnapshotRecord{ServerName: "srv-a", CapturedAt: now.Format(time.RFC3339)}); err != nil {
		t.Fatalf("saveHealthSnapshot(new) error = %v", err)
	}
	snapshots, err = repo.ListHealthSnapshots("2026-05-01T00:00:00Z", "2026-05-21T00:00:00Z", "srv-a")
	if err != nil {
		t.Fatalf("ListHealthSnapshots(after prune) error = %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].CapturedAt != "2026-05-20T12:00:00Z" {
		t.Fatalf("snapshots after retention prune = %+v, want only newest snapshot", snapshots)
	}
}

func TestServerFactsSaveWritesHealthSnapshot(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "server-facts-snapshot.db")
	if err := repo.Save(ServerFactsRecord{
		ServerName:   "srv-save",
		CollectedAt:  "2026-05-18T12:00:00Z",
		DiskStatus:   "ok",
		DiskFreeKB:   2048,
		DiskTotalKB:  4096,
		AptStatus:    "ok",
		OSPrettyName: "Ubuntu",
		RawJSON:      `{"facts":true}`,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	snapshots, err := repo.ListHealthSnapshots("2026-05-18T00:00:00Z", "2026-05-19T00:00:00Z", "srv-save")
	if err != nil {
		t.Fatalf("ListHealthSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 || snapshots[0].Source != "facts" || snapshots[0].DiskFreeKB != 2048 || snapshots[0].OSPrettyName != "Ubuntu" {
		t.Fatalf("snapshots = %+v, want one facts-derived snapshot", snapshots)
	}
}

func TestHealthSnapshotCaptureFactsWritesNormalizedObservation(t *testing.T) {
	_, repo := openServerFactsTestRepository(t, "capture-facts.db")
	if err := repo.CaptureFacts(ServerFactsRecord{
		ServerName:   " capture-facts ",
		CollectedAt:  "2026-07-10T12:00:00Z",
		DiskFreeKB:   512,
		DiskTotalKB:  1024,
		OSPrettyName: "Debian",
	}); err != nil {
		t.Fatalf("CaptureFacts() error = %v", err)
	}

	snapshots, err := repo.ListHealthSnapshots("2026-07-10T00:00:00Z", "2026-07-11T00:00:00Z", "capture-facts")
	if err != nil {
		t.Fatalf("ListHealthSnapshots() error = %v", err)
	}
	if len(snapshots) != 1 {
		t.Fatalf("snapshot count = %d, want 1", len(snapshots))
	}
	got := snapshots[0]
	if got.Source != "facts" || got.DiskStatus != "unknown" || got.AptStatus != "unknown" || got.RawJSON != "{}" {
		t.Fatalf("snapshot = %+v, want normalized facts observation", got)
	}
}

func TestServerFactsSaveKeepsCurrentFactsWhenCaptureFails(t *testing.T) {
	db, repo := openServerFactsTestRepository(t, "capture-failure.db")
	if _, err := db.Exec(`CREATE TRIGGER reject_health_snapshot
		BEFORE INSERT ON server_health_snapshots
		BEGIN SELECT RAISE(FAIL, 'capture rejected'); END`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}

	err := repo.Save(ServerFactsRecord{ServerName: "srv-capture-failure", CollectedAt: "2026-07-10T12:00:00Z"})
	if err == nil || !strings.Contains(err.Error(), "capture rejected") {
		t.Fatalf("Save() error = %v, want capture failure", err)
	}
	loaded, err := repo.LoadAll()
	if err != nil {
		t.Fatalf("LoadAll() error = %v", err)
	}
	if got := loaded["srv-capture-failure"].CollectedAt; got != "2026-07-10T12:00:00Z" {
		t.Fatalf("saved current facts collected_at = %q, want retained facts", got)
	}
}

func openServerFactsTestRepository(t *testing.T, name string) (*sql.DB, SQLiteServerFactsRepository) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureServerFactsSchema(db); err != nil {
		t.Fatalf("EnsureServerFactsSchema() error = %v", err)
	}
	return db, SQLiteServerFactsRepository{DB: func() *sql.DB { return db }}
}
