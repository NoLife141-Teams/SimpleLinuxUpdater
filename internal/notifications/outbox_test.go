package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestNotificationOutboxMigratesExistingSettingsDatabase(t *testing.T) {
	db := openOutboxTestDB(t, "migration.db")
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() second call error = %v", err)
	}
	var table string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='notification_outbox'").Scan(&table); err != nil {
		t.Fatalf("notification outbox table missing: %v", err)
	}
	rows, err := db.Query("PRAGMA table_info(notification_outbox)")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		lowerName := strings.ToLower(name)
		for _, forbidden := range []string{"url", "token", "secret", "password", "credential"} {
			if strings.Contains(lowerName, forbidden) {
				t.Fatalf("outbox schema persists a credential-like column %q", name)
			}
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestNotificationOutboxPersistsOnlyRedactedIntentFacts(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	rows, err := buildOutboxRows(DeliveryIntent{
		ID: "audit-41", CreatedAt: now.Format(time.RFC3339Nano), Action: EventUpdateComplete,
		TargetType: "server", TargetName: "srv-a", Status: "success", Message: "Update completed",
		MetaJSON: `{"password":"plain-secret","nested":{"api_token":"token-value","safe":"visible"}}`,
	}, EventUpdateComplete, []string{DestinationWebhook}, now)
	if err != nil {
		t.Fatal(err)
	}
	db := openOutboxTestDB(t, "redaction.db")
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := insertOutboxRows(context.Background(), db, rows, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var persisted string
	if err := db.QueryRow("SELECT meta_json FROM notification_outbox").Scan(&persisted); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted, "plain-secret") || strings.Contains(persisted, "token-value") || !strings.Contains(persisted, "visible") {
		t.Fatalf("persisted meta is not safely redacted: %s", persisted)
	}
}

func TestNotificationOutboxIdentityDeduplicatesOneDestination(t *testing.T) {
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	intent := DeliveryIntent{ID: "audit-42", Action: EventUpdateComplete, TargetName: "srv-a", MetaJSON: `{}`}
	rows, err := buildOutboxRows(intent, EventUpdateComplete, []string{DestinationWebhook}, now)
	if err != nil {
		t.Fatal(err)
	}
	db := openOutboxTestDB(t, "identity.db")
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	if err := insertOutboxRows(context.Background(), db, rows, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	changedIntent := intent
	changedIntent.Message = "duplicate callback with different fields"
	duplicateRows, err := buildOutboxRows(changedIntent, EventUpdateComplete, []string{DestinationWebhook}, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOutboxRows(context.Background(), db, duplicateRows, now.Add(time.Second).Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM notification_outbox").Scan(&count); err != nil || count != 1 {
		t.Fatalf("count=%d error=%v, want one stable row", count, err)
	}
}

func TestNotificationOutboxReplaysPendingRowAfterStartup(t *testing.T) {
	db := openOutboxTestDB(t, "replay.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "replay-1", Action: EventUpdateComplete, TargetName: "srv-replay", MetaJSON: `{}`}, now)
	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want one startup replay", got)
	}
}

func TestNotificationOutboxRecoversExpiredClaimAfterStartup(t *testing.T) {
	db := openOutboxTestDB(t, "claim-recovery.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	rowID := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "replay-claim", Action: EventUpdateComplete, TargetName: "srv-claim", MetaJSON: `{}`}, now)
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, claimed_at=? WHERE id=?", outboxStateClaimed, now.Add(-time.Hour).Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}
	deps := outboxTestDeps(db, 5*time.Millisecond)
	deps.ClaimTTL = time.Minute
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want one recovered delivery", got)
	}
}

func TestNotificationOutboxSchedulesRecoveryForLiveClaim(t *testing.T) {
	db := openOutboxTestDB(t, "live-claim-recovery.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	rowID := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "live-claim", Action: EventUpdateComplete, TargetName: "srv-live-claim", MetaJSON: `{}`}, now)
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, claimed_at=? WHERE id=?", outboxStateClaimed, now.Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}
	deps := outboxTestDeps(db, 5*time.Millisecond)
	deps.ClaimTTL = 30 * time.Millisecond
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want one delivery after the bounded claim expires", got)
	}
}

func TestNotificationOutboxDuplicateWakeupsDoNotDuplicateDelivery(t *testing.T) {
	db := openOutboxTestDB(t, "duplicate-wake.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	svc := NewService(outboxTestDeps(db, time.Hour))
	t.Cleanup(func() { closeOutboxService(t, svc) })
	intent := DeliveryIntent{ID: "wake-1", Action: EventUpdateComplete, TargetName: "srv-wake", MetaJSON: `{}`}
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		t.Fatalf("Accept()=%+v", got)
	}
	for range 100 {
		svc.signalWorker()
	}
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want one claimed delivery", got)
	}
}

func TestNotificationOutboxSkipsDestinationDisabledBeforeReplay(t *testing.T) {
	db := openOutboxTestDB(t, "disabled.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, false)
	now := time.Now().UTC()
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "disabled-1", Action: EventUpdateComplete, TargetName: "srv-disabled", MetaJSON: `{}`}, now)
	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSkipped)
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Fatalf("requests=%d, want disabled destination skipped", got)
	}
}

func TestNotificationOutboxAdmissionReportsPersistenceFailure(t *testing.T) {
	db := openOutboxTestDB(t, "admission-failure.db")
	server, _ := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	if _, err := db.Exec(`CREATE TRIGGER reject_notification_outbox BEFORE INSERT ON notification_outbox BEGIN SELECT RAISE(FAIL, 'outbox unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	svc := NewService(outboxTestDeps(db, time.Hour))
	t.Cleanup(func() { closeOutboxService(t, svc) })
	got := svc.Accept(DeliveryIntent{ID: "failure-1", Action: EventUpdateComplete, TargetName: "srv", MetaJSON: `{}`})
	if got.State != AdmissionRejected || strings.Contains(got.Error, "outbox unavailable") {
		t.Fatalf("Accept()=%+v, want safe persistence rejection", got)
	}
}

func TestNotificationOutboxPrunesOldTerminalRowsButKeepsDiagnostics(t *testing.T) {
	db := openOutboxTestDB(t, "retention.db")
	server, _ := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	var raw string
	if err := db.QueryRow("SELECT value FROM settings WHERE key=?", SettingsKey).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var stored persistedSettings
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		t.Fatal(err)
	}
	stored.LastDelivery = &DeliveryStatus{EventType: EventUpdateComplete, TargetName: "srv-last", Outcome: DeliveryOutcomeSucceeded, Success: true, AttemptedAt: now.Format(time.RFC3339)}
	stored.LastDeliveries = map[string]*DeliveryStatus{DestinationWebhook: stored.LastDelivery}
	body, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE settings SET value=? WHERE key=?", string(body), SettingsKey); err != nil {
		t.Fatal(err)
	}
	rowID := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "old-terminal", Action: EventUpdateComplete, TargetName: "srv-old", MetaJSON: `{}`}, now.Add(-60*24*time.Hour))
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, completed_at=? WHERE id=?", outboxStateSucceeded, now.Add(-60*24*time.Hour).Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}
	deps := outboxTestDeps(db, time.Hour)
	deps.Now = func() time.Time { return now }
	deps.Retention = 30 * 24 * time.Hour
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM notification_outbox").Scan(&count); err != nil || count != 0 {
		t.Fatalf("retained terminal rows=%d error=%v", count, err)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() after pruning error=%v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.TargetName != "srv-last" {
		t.Fatalf("diagnostics=%+v, want aggregate diagnostic preserved", diagnostics)
	}
}

func TestNotificationOutboxCountsPendingAndRetryingRows(t *testing.T) {
	db := openOutboxTestDB(t, "counts.db")
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "pending-count", Action: EventUpdateComplete, TargetName: "srv-pending", MetaJSON: `{}`}, now)
	second := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "retry-count", Action: EventUpdateComplete, TargetName: "srv-retry", MetaJSON: `{}`}, now)
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, next_attempt_at=? WHERE id=?", outboxStateClaimed, now.Format(time.RFC3339Nano), first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, next_attempt_at=? WHERE id=?", outboxStateRetrying, now.Add(time.Minute).Format(time.RFC3339Nano), second); err != nil {
		t.Fatal(err)
	}
	counts, err := loadOutboxCounts(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	if counts.Pending != 1 || counts.Retrying != 1 {
		t.Fatalf("counts=%+v, want one claimed/pending and one retrying", counts)
	}
}

func openOutboxTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func prepareOutboxSettings(t *testing.T, db *sql.DB, endpoint string, enabled bool) {
	t.Helper()
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	stored := persistedSettings{Enabled: enabled, EncryptedWebhookURL: endpoint, EventTypes: []string{EventUpdateComplete}}
	body, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT OR REPLACE INTO settings(key,value) VALUES(?,?)", SettingsKey, string(body)); err != nil {
		t.Fatal(err)
	}
}

func outboxTestDeps(db *sql.DB, poll time.Duration) ServiceDeps {
	return ServiceDeps{
		DB: func() *sql.DB { return db }, PollInterval: poll, Backoff: func(int) time.Duration { return 0 },
		EncryptSecret: func(value string) (string, error) { return value, nil },
		DecryptSecret: func(value string) (string, error) { return value, nil }, Logf: func(string, ...any) {},
	}
}

func newOutboxHTTPServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&requests, 1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func insertPendingOutboxIntent(t *testing.T, db *sql.DB, intent DeliveryIntent, now time.Time) string {
	t.Helper()
	rows, err := buildOutboxRows(intent, EventUpdateComplete, []string{DestinationWebhook}, now)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertOutboxRows(context.Background(), db, rows, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	return rows[0].ID
}

func waitForLatestOutboxState(t *testing.T, db *sql.DB, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var state string
		err := db.QueryRow("SELECT state FROM notification_outbox ORDER BY rowid DESC LIMIT 1").Scan(&state)
		if err == nil && state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	var state string
	_ = db.QueryRow("SELECT state FROM notification_outbox ORDER BY rowid DESC LIMIT 1").Scan(&state)
	t.Fatalf("latest outbox state=%q, want %q", state, want)
}

func closeOutboxService(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error=%v", err)
	}
}
