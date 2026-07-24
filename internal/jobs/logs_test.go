package jobs

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestSQLiteJobLogsAppendOrderedStreamsAndPagination(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	db, manager := openConfiguredJobLogTest(t, LogConfig{
		RetentionDays: 30,
		MaxBytes:      DefaultLogMaxBytes,
		Now:           func() time.Time { return now },
	})
	job, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin", Status: StatusRunning})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	updated, err := manager.AppendActiveLogFragments(job.ID, []LogFragment{
		{Stream: LogStreamStdout, Data: "download 10%\rdownload 20%\r"},
		{Stream: LogStreamStderr, Data: "warning\n"},
		{Stream: LogStreamStdout, Data: "done\n"},
	})
	if err != nil || !updated {
		t.Fatalf("AppendActiveLogFragments() = %t, %v", updated, err)
	}
	page, err := manager.ReadLogPage(job.ID, 0, 2)
	if err != nil {
		t.Fatalf("ReadLogPage(first) error = %v", err)
	}
	if len(page.Fragments) != 2 || !page.HasMore || page.NextSequence != page.Fragments[1].Sequence {
		t.Fatalf("first page = %+v", page)
	}
	if page.Fragments[0].Stream != LogStreamStdout || !strings.Contains(page.Fragments[0].Data, "\r") {
		t.Fatalf("stdout fragment did not preserve stream/raw carriage returns: %+v", page.Fragments[0])
	}
	next, err := manager.ReadLogPage(job.ID, page.NextSequence, 2)
	if err != nil {
		t.Fatalf("ReadLogPage(next) error = %v", err)
	}
	if len(next.Fragments) != 1 || next.Fragments[0].Stream != LogStreamStdout || next.Fragments[0].Sequence <= page.NextSequence {
		t.Fatalf("next page = %+v", next)
	}
	var persisted int
	if err := db.QueryRow("SELECT COUNT(*) FROM job_log_chunks WHERE job_id = ?", job.ID).Scan(&persisted); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	if persisted != 3 {
		t.Fatalf("persisted chunks = %d, want 3", persisted)
	}

	fullSnapshot := "download 10%\rdownload 20%\rwarning\ndone\n"
	if err := manager.Transition(job.ID, Intent{Kind: IntentAdvance, LogsText: &fullSnapshot}); err != nil {
		t.Fatalf("Transition(full log snapshot) error = %v", err)
	}
	afterTransition, err := manager.ReadLogPage(job.ID, 0, 10)
	if err != nil {
		t.Fatalf("ReadLogPage(after transition) error = %v", err)
	}
	if len(afterTransition.Fragments) != 3 || afterTransition.Fragments[1].Stream != LogStreamStderr {
		t.Fatalf("full snapshot replacement lost structured streams: %+v", afterTransition.Fragments)
	}
}

func TestSQLiteJobLogsNotifyWithPersistedStructuredFragments(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSchemaConfigured(db, LogConfig{Now: func() time.Time { return now }}); err != nil {
		t.Fatalf("EnsureSchemaConfigured() error = %v", err)
	}
	var events []LogEvent
	manager := NewManager(NewSQLiteRepositoryWithLogConfig(db, LogConfig{Now: func() time.Time { return now }}), ManagerOptions{
		Now:       func() time.Time { return now },
		NotifyLog: func(event LogEvent) { events = append(events, event) },
	})
	job, err := manager.CreateJob(CreateParams{
		Kind:       KindUpdate,
		ServerName: "alpha",
		Actor:      "admin",
		Status:     StatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	if _, err := manager.AppendActiveLogFragments(job.ID, []LogFragment{
		{Stream: LogStreamStdout, Data: "Reading 20%\r"},
		{Stream: LogStreamStderr, Data: "warning\n"},
	}); err != nil {
		t.Fatalf("AppendActiveLogFragments() error = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("structured events = %+v, want two", events)
	}
	if events[0].ServerName != "alpha" || events[0].JobID != job.ID || events[0].Sequence <= 0 ||
		events[0].Stream != LogStreamStdout || events[0].Data != "Reading 20%\r" {
		t.Fatalf("first structured event = %+v", events[0])
	}
	if events[1].Sequence != events[0].Sequence+1 || events[1].Stream != LogStreamStderr || events[1].Data != "warning\n" {
		t.Fatalf("second structured event = %+v", events[1])
	}
}

func TestSQLiteJobLogsTruncateMiddleAndBoundPreview(t *testing.T) {
	now := time.Date(2026, 7, 24, 10, 0, 0, 0, time.UTC)
	maxBytes := MinLogMaxBytes
	db, manager := openConfiguredJobLogTest(t, LogConfig{
		RetentionDays: 30,
		MaxBytes:      maxBytes,
		Now:           func() time.Time { return now },
	})
	job, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin", Status: StatusRunning})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	head := strings.Repeat("H", LogHeadBytes)
	middle := strings.Repeat("M", 96*1024)
	tail := strings.Repeat("T", 64*1024)
	if _, err := manager.AppendActiveLogStream(job.ID, LogStreamStdout, head+middle+tail); err != nil {
		t.Fatalf("AppendActiveLogStream() error = %v", err)
	}
	full, err := manager.GetJobWithLogs(job.ID)
	if err != nil {
		t.Fatalf("GetJobWithLogs() error = %v", err)
	}
	if !full.LogsTruncated || !strings.HasPrefix(full.LogsText, head) || !strings.HasSuffix(full.LogsText, "T") || !strings.Contains(full.LogsText, LogTruncationMarker) {
		t.Fatalf("truncated log did not preserve head/marker/tail: truncated=%t len=%d", full.LogsTruncated, len(full.LogsText))
	}
	var storedBytes int
	if err := db.QueryRow("SELECT COALESCE(SUM(length(data)), 0) FROM job_log_chunks WHERE job_id = ?", job.ID).Scan(&storedBytes); err != nil {
		t.Fatalf("sum persisted bytes: %v", err)
	}
	if storedBytes > maxBytes {
		t.Fatalf("persisted bytes = %d, max = %d", storedBytes, maxBytes)
	}
	preview, err := manager.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if len(preview.LogsText) > LogPreviewMaxBytes {
		t.Fatalf("logs_text preview bytes = %d, max = %d", len(preview.LogsText), LogPreviewMaxBytes)
	}
	if _, err := manager.AppendActiveLogStream(job.ID, LogStreamStderr, "LATEST\r"); err != nil {
		t.Fatalf("append after truncation: %v", err)
	}
	full, err = manager.GetJobWithLogs(job.ID)
	if err != nil {
		t.Fatalf("GetJobWithLogs(after append) error = %v", err)
	}
	if !strings.HasSuffix(full.LogsText, "LATEST\r") {
		t.Fatalf("truncated log tail = %q, want latest stderr", full.LogsText[len(full.LogsText)-32:])
	}
}

func TestSQLiteJobLogRetentionUsesInjectedClockAndSkipsActiveJobs(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	_, manager := openConfiguredJobLogTest(t, LogConfig{
		RetentionDays: 30,
		MaxBytes:      DefaultLogMaxBytes,
		Now:           func() time.Time { return now },
	})
	old := now.Add(-30 * 24 * time.Hour)
	active := Record{
		ID: "active-old", Kind: KindUpdate, Actor: "admin", Status: StatusRunning,
		LogsText: "active log", CreatedAt: FormatTimestamp(old), UpdatedAt: FormatTimestamp(old),
		RetryPolicyJSON: "{}", MetaJSON: "{}",
	}
	expired := Record{
		ID: "finished-old", Kind: KindUpdate, Actor: "admin", Status: StatusSucceeded,
		LogsText: "finished log", CreatedAt: FormatTimestamp(old), UpdatedAt: FormatTimestamp(old),
		FinishedAt: FormatTimestamp(old), RetryPolicyJSON: "{}", MetaJSON: "{}",
	}
	for _, record := range []Record{active, expired} {
		if err := manager.ImportJobRecord(record); err != nil {
			t.Fatalf("ImportJobRecord(%s) error = %v", record.ID, err)
		}
	}
	count, err := manager.PurgeExpiredLogs()
	if err != nil {
		t.Fatalf("PurgeExpiredLogs() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("purged = %d, want 1", count)
	}
	activePage, err := manager.ReadLogPage(active.ID, 0, 10)
	if err != nil || activePage.Expired || len(activePage.Fragments) == 0 {
		t.Fatalf("active old page = %+v, err=%v", activePage, err)
	}
	expiredPage, err := manager.ReadLogPage(expired.ID, 0, 10)
	if err != nil || !expiredPage.Expired || len(expiredPage.Fragments) != 0 {
		t.Fatalf("expired page = %+v, err=%v", expiredPage, err)
	}
}

func TestJobLogLegacyMigrationIsIdempotent(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "legacy.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE jobs (
			id TEXT PRIMARY KEY, kind TEXT NOT NULL, parent_job_id TEXT NOT NULL DEFAULT '',
			server_name TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL, client_ip TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL, phase TEXT NOT NULL DEFAULT '', summary TEXT NOT NULL DEFAULT '',
			logs_text TEXT NOT NULL DEFAULT '', error_class TEXT NOT NULL DEFAULT '',
			retry_policy_json TEXT NOT NULL DEFAULT '{}', meta_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, started_at TEXT NOT NULL DEFAULT '',
			finished_at TEXT NOT NULL DEFAULT ''
		);
		INSERT INTO jobs(id, kind, actor, status, logs_text, created_at, updated_at, finished_at)
		VALUES
			('recent', 'update', 'admin', 'succeeded', 'recent logs', ?, ?, ?),
			('expired', 'update', 'admin', 'succeeded', 'expired logs', ?, ?, ?),
			('active', 'update', 'admin', 'running', 'active logs', ?, ?, '')
	`, FormatTimestamp(now.Add(-29*24*time.Hour)), FormatTimestamp(now.Add(-29*24*time.Hour)), FormatTimestamp(now.Add(-29*24*time.Hour)),
		FormatTimestamp(now.Add(-31*24*time.Hour)), FormatTimestamp(now.Add(-31*24*time.Hour)), FormatTimestamp(now.Add(-31*24*time.Hour)),
		FormatTimestamp(now.Add(-31*24*time.Hour)), FormatTimestamp(now.Add(-31*24*time.Hour))); err != nil {
		t.Fatalf("seed legacy jobs: %v", err)
	}
	config := LogConfig{RetentionDays: 30, MaxBytes: DefaultLogMaxBytes, Now: func() time.Time { return now }}
	for run := 0; run < 2; run++ {
		if err := EnsureSchemaConfigured(db, config); err != nil {
			t.Fatalf("EnsureSchemaConfigured(run %d) error = %v", run+1, err)
		}
	}
	var recentChunks, activeChunks, expiredChunks, expiredFlag int
	if err := db.QueryRow("SELECT COUNT(*) FROM job_log_chunks WHERE job_id = 'recent'").Scan(&recentChunks); err != nil {
		t.Fatalf("count recent chunks: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM job_log_chunks WHERE job_id = 'active'").Scan(&activeChunks); err != nil {
		t.Fatalf("count active chunks: %v", err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM job_log_chunks WHERE job_id = 'expired'").Scan(&expiredChunks); err != nil {
		t.Fatalf("count expired chunks: %v", err)
	}
	if err := db.QueryRow("SELECT logs_expired FROM jobs WHERE id = 'expired'").Scan(&expiredFlag); err != nil {
		t.Fatalf("read expired flag: %v", err)
	}
	if recentChunks != 1 || activeChunks != 1 || expiredChunks != 0 || expiredFlag != 1 {
		t.Fatalf("migration counts recent=%d active=%d expired=%d flag=%d", recentChunks, activeChunks, expiredChunks, expiredFlag)
	}
}

func TestSQLiteJobLogConcurrentAppendsHaveUniqueMonotonicSequences(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	_, manager := openConfiguredJobLogTest(t, LogConfig{
		RetentionDays: 30,
		MaxBytes:      DefaultLogMaxBytes,
		Now:           func() time.Time { return now },
	})
	job, err := manager.CreateJob(CreateParams{Kind: KindUpdate, Actor: "admin", Status: StatusRunning})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	const writers = 32
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			_, err := manager.AppendActiveLogStream(job.ID, LogStreamStdout, fmt.Sprintf("fragment-%02d\n", index))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent append error = %v", err)
		}
	}
	page, err := manager.ReadLogPage(job.ID, 0, 100)
	if err != nil {
		t.Fatalf("ReadLogPage() error = %v", err)
	}
	if len(page.Fragments) != writers {
		t.Fatalf("fragments = %d, want %d", len(page.Fragments), writers)
	}
	for i := 1; i < len(page.Fragments); i++ {
		if page.Fragments[i].Sequence <= page.Fragments[i-1].Sequence {
			t.Fatalf("sequences not monotonic at %d: %+v", i, page.Fragments)
		}
	}
}

func openConfiguredJobLogTest(t *testing.T, config LogConfig) (*sql.DB, *Manager) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "job-logs.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := EnsureSchemaConfigured(db, config); err != nil {
		t.Fatalf("EnsureSchemaConfigured() error = %v", err)
	}
	manager := NewManager(NewSQLiteRepositoryWithLogConfig(db, config), ManagerOptions{Now: config.Now})
	return db, manager
}
