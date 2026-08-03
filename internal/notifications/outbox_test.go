package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func TestNotificationOutboxMigratesDestinationFingerprintColumn(t *testing.T) {
	db := openOutboxTestDB(t, "destination-fingerprint-migration.db")
	if _, err := db.Exec(`CREATE TABLE notification_outbox (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := ensureOutboxDestinationFingerprintColumn(db); err != nil {
		t.Fatalf("ensureOutboxDestinationFingerprintColumn() error = %v", err)
	}
	if err := ensureOutboxDestinationFingerprintColumn(db); err != nil {
		t.Fatalf("ensureOutboxDestinationFingerprintColumn() second call error = %v", err)
	}
	var found int
	rows, err := db.Query(`PRAGMA table_info(notification_outbox)`)
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
		if name == "destination_fingerprint" {
			found++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if found != 1 {
		t.Fatalf("destination_fingerprint columns=%d, want 1", found)
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

func TestNotificationOutboxAdmissionBindsDestinationConfiguration(t *testing.T) {
	db := openOutboxTestDB(t, "destination-binding.db")
	server, _ := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	svc := NewService(outboxTestDeps(db, time.Hour))
	t.Cleanup(func() { closeOutboxService(t, svc) })

	if got := svc.Accept(DeliveryIntent{ID: "bound-destination", Action: EventUpdateComplete, TargetName: "srv-bound", MetaJSON: `{}`}); got.State != AdmissionAdmitted {
		t.Fatalf("Accept()=%+v, want admitted", got)
	}
	var fingerprint string
	if err := db.QueryRow("SELECT destination_fingerprint FROM notification_outbox").Scan(&fingerprint); err != nil {
		t.Fatal(err)
	}
	want := destinationConfigFingerprint(Settings{Enabled: true, WebhookURL: server.URL}, DestinationWebhook)
	if fingerprint == "" || fingerprint != want {
		t.Fatalf("destination fingerprint=%q, want bound fingerprint %q", fingerprint, want)
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

func TestNotificationOutboxPersistenceReplacementPausesBetweenRows(t *testing.T) {
	db := openOutboxTestDB(t, "replacement-pause.db")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondStarted := make(chan struct{})
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&requests, 1) {
		case 1:
			close(firstStarted)
			<-releaseFirst
		case 2:
			close(secondStarted)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "replacement-first", Action: EventUpdateComplete, TargetName: "srv-first", MetaJSON: `{}`}, now)
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "replacement-second", Action: EventUpdateComplete, TargetName: "srv-second", MetaJSON: `{}`}, now)
	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))
	t.Cleanup(func() { closeOutboxService(t, svc) })

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification delivery did not start")
	}
	prepareDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		prepareDone <- svc.PreparePersistenceReplacement(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for {
		svc.persistenceMu.Lock()
		paused := svc.persistencePaused
		svc.persistenceMu.Unlock()
		if paused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("persistence replacement did not request a pause")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseFirst)
	if err := <-prepareDone; err != nil {
		t.Fatalf("PreparePersistenceReplacement() error=%v", err)
	}
	select {
	case <-secondStarted:
		t.Fatal("worker claimed a second row while persistence replacement was waiting")
	default:
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests=%d, want only the in-flight delivery before replacement", got)
	}
	if err := svc.ReloadPersistence(context.Background()); err != nil {
		t.Fatalf("ReloadPersistence() error=%v", err)
	}
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
}

func TestNotificationOutboxCloseLeavesQueuedRowsDurable(t *testing.T) {
	db := openOutboxTestDB(t, "close-backlog.db")
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	var requests int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&requests, 1) == 1 {
			close(firstStarted)
			<-releaseFirst
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "close-first", Action: EventUpdateComplete, TargetName: "srv-first", MetaJSON: `{}`}, now)
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "close-second", Action: EventUpdateComplete, TargetName: "srv-second", MetaJSON: `{}`}, now)
	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))

	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first notification delivery did not start")
	}
	closeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		closeDone <- svc.Close(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for !svc.isClosing() {
		if time.Now().After(deadline) {
			t.Fatal("notification lifecycle did not begin closing")
		}
		time.Sleep(time.Millisecond)
	}
	close(releaseFirst)
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error=%v", err)
	}
	if got := atomic.LoadInt32(&requests); got != 1 {
		t.Fatalf("requests=%d, want shutdown to finish only the in-flight delivery", got)
	}
	var pending, succeeded int
	if err := db.QueryRow(`
		SELECT
			SUM(CASE WHEN state = ? THEN 1 ELSE 0 END),
			SUM(CASE WHEN state = ? THEN 1 ELSE 0 END)
		  FROM notification_outbox
	`, outboxStatePending, outboxStateSucceeded).Scan(&pending, &succeeded); err != nil {
		t.Fatal(err)
	}
	if pending != 1 || succeeded != 1 {
		t.Fatalf("outbox states=(pending=%d,succeeded=%d), want queued row preserved", pending, succeeded)
	}
}

func TestNotificationOutboxKeepsPollingAfterRepeatedClaimFailures(t *testing.T) {
	db := openOutboxTestDB(t, "claim-retry.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "claim-retry", Action: EventUpdateComplete, TargetName: "srv-retry", MetaJSON: `{}`}, time.Now().UTC())

	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			_, _ = lockConn.ExecContext(context.Background(), "ROLLBACK")
		}
	}()

	var claimFailures int32
	deps := outboxTestDeps(db, 5*time.Millisecond)
	deps.Logf = func(format string, _ ...any) {
		if strings.Contains(format, "notification outbox claim failed") {
			atomic.AddInt32(&claimFailures, 1)
		}
	}
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	svc.signalWorker()

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt32(&claimFailures) < defaultAttempts {
		if time.Now().After(deadline) {
			t.Fatalf("claim failures=%d, want at least %d", atomic.LoadInt32(&claimFailures), defaultAttempts)
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := lockConn.ExecContext(context.Background(), "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	locked = false

	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want delivery after repeated claim failures", got)
	}
}

func TestNotificationOutboxKeepsPollingAfterWakeSchedulingFailure(t *testing.T) {
	db := openOutboxTestDB(t, "wake-scheduling-retry.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	rowID := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "wake-scheduling-retry", Action: EventUpdateComplete, TargetName: "srv-retry", MetaJSON: `{}`}, now)
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, next_attempt_at=? WHERE id=?", outboxStateRetrying, now.Add(25*time.Millisecond).Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}

	deps := outboxTestDeps(db, 5*time.Millisecond)
	var wakeCalls int32
	deps.NextOutboxWake = func(ctx context.Context, db *sql.DB, ttl time.Duration) (time.Time, bool, error) {
		if atomic.AddInt32(&wakeCalls, 1) == 1 {
			return time.Time{}, false, errors.New("temporary wake query failure")
		}
		return nextOutboxWake(ctx, db, ttl)
	}
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want delivery after wake scheduling retry", got)
	}
	if got := atomic.LoadInt32(&wakeCalls); got < 2 {
		t.Fatalf("wake scheduling calls=%d, want retry after failure", got)
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

func TestNotificationOutboxSkipsDestinationChangedAfterAdmission(t *testing.T) {
	db := openOutboxTestDB(t, "changed-destination.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	rows, err := buildOutboxRows(DeliveryIntent{
		ID: "changed-destination", Action: EventUpdateComplete, TargetName: "srv-changed", MetaJSON: `{}`,
	}, EventUpdateComplete, []string{DestinationWebhook}, now)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].DestinationFingerprint = destinationConfigFingerprint(Settings{
		Enabled: true, WebhookURL: "https://old.example.test/hook",
	}, DestinationWebhook)
	if err := insertOutboxRows(context.Background(), db, rows, now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))
	t.Cleanup(func() { closeOutboxService(t, svc) })
	waitForLatestOutboxState(t, db, outboxStateSkipped)
	if got := atomic.LoadInt32(requests); got != 0 {
		t.Fatalf("requests=%d, want changed destination skipped", got)
	}
	var persistedError string
	if err := db.QueryRow("SELECT error FROM notification_outbox").Scan(&persistedError); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(persistedError, "destination configuration changed") {
		t.Fatalf("persisted error=%q, want destination-change explanation", persistedError)
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

func TestNotificationOutboxPrunesOldTerminalRowsWhileRunning(t *testing.T) {
	db := openOutboxTestDB(t, "periodic-retention.db")
	server, _ := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, db, server.URL, true)
	now := time.Now().UTC()
	deps := outboxTestDeps(db, time.Hour)
	deps.Now = func() time.Time { return now }
	deps.Retention = 30 * 24 * time.Hour
	deps.PruneInterval = 5 * time.Millisecond
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })

	rowID := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "periodic-old-terminal", Action: EventUpdateComplete, TargetName: "srv-old", MetaJSON: `{}`}, now.Add(-60*24*time.Hour))
	if _, err := db.Exec("UPDATE notification_outbox SET state=?, completed_at=? WHERE id=?", outboxStateSucceeded, now.Add(-60*24*time.Hour).Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		var count int
		if err := db.QueryRow("SELECT COUNT(*) FROM notification_outbox WHERE id=?", rowID).Scan(&count); err == nil && count == 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("old terminal notification row was not pruned while the service remained running")
}

func TestNotificationOutboxReloadPersistenceDrainsRestoredRows(t *testing.T) {
	initialDB := openOutboxTestDB(t, "reload-initial.db")
	if err := EnsureSchema(initialDB); err != nil {
		t.Fatal(err)
	}
	var currentDB atomic.Pointer[sql.DB]
	currentDB.Store(initialDB)
	deps := outboxTestDeps(initialDB, time.Hour)
	deps.DB = currentDB.Load
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })

	restoredDB := openOutboxTestDB(t, "reload-restored.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, restoredDB, server.URL, true)
	rowID := insertPendingOutboxIntent(t, restoredDB, DeliveryIntent{ID: "restored-pending", Action: EventUpdateComplete, TargetName: "srv-restored", MetaJSON: `{}`}, time.Now().UTC())
	currentDB.Store(restoredDB)

	if err := svc.ReloadPersistence(context.Background()); err != nil {
		t.Fatalf("ReloadPersistence() error=%v", err)
	}
	waitForLatestOutboxState(t, restoredDB, outboxStateSucceeded)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("restored notification requests=%d, want 1", got)
	}
	var state string
	if err := restoredDB.QueryRow("SELECT state FROM notification_outbox WHERE id=?", rowID).Scan(&state); err != nil || state != outboxStateSucceeded {
		t.Fatalf("restored row state=%q error=%v, want succeeded", state, err)
	}
}

func TestNotificationOutboxReloadPreservesCompletedDeliveryOutcome(t *testing.T) {
	initialDB := openOutboxTestDB(t, "reload-completed-initial.db")
	server, requests := newOutboxHTTPServer(t)
	prepareOutboxSettings(t, initialDB, server.URL, true)
	var currentDB atomic.Pointer[sql.DB]
	currentDB.Store(initialDB)
	deps := outboxTestDeps(initialDB, 5*time.Millisecond)
	deps.DB = currentDB.Load
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	intent := DeliveryIntent{ID: "restored-completed", Action: EventUpdateComplete, TargetName: "srv-completed", MetaJSON: `{}`}
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		t.Fatalf("Accept()=%+v, want admitted", got)
	}
	waitForLatestOutboxState(t, initialDB, outboxStateSucceeded)
	if err := svc.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatalf("PreparePersistenceReplacement() error=%v", err)
	}

	restoredDB := openOutboxTestDB(t, "reload-completed-restored.db")
	prepareOutboxSettings(t, restoredDB, server.URL, true)
	rowID := insertPendingOutboxIntent(t, restoredDB, intent, time.Now().UTC())
	currentDB.Store(restoredDB)
	if err := svc.ReloadPersistence(context.Background()); err != nil {
		t.Fatalf("ReloadPersistence() error=%v", err)
	}
	waitForLatestOutboxState(t, restoredDB, outboxStateSucceeded)
	time.Sleep(25 * time.Millisecond)
	if got := atomic.LoadInt32(requests); got != 1 {
		t.Fatalf("requests=%d, want restored pending copy suppressed after completed delivery", got)
	}
	var state string
	if err := restoredDB.QueryRow("SELECT state FROM notification_outbox WHERE id=?", rowID).Scan(&state); err != nil || state != outboxStateSucceeded {
		t.Fatalf("restored row state=%q error=%v, want succeeded", state, err)
	}
}

func TestNotificationOutboxReloadPreservesRetryBudget(t *testing.T) {
	now := time.Now().UTC()
	nextAttempt := now.Add(time.Hour).Format(time.RFC3339Nano)
	intent := DeliveryIntent{ID: "restored-retrying", Action: EventUpdateComplete, TargetName: "srv-retrying", MetaJSON: `{}`}
	initialDB := openOutboxTestDB(t, "reload-retrying-initial.db")
	if err := EnsureSchema(initialDB); err != nil {
		t.Fatal(err)
	}
	rowID := insertPendingOutboxIntent(t, initialDB, intent, now)
	if _, err := initialDB.Exec(`
		UPDATE notification_outbox
		   SET state=?, attempts=2, next_attempt_at=?, attempted_at=?, status_code=503, error='temporary failure'
		 WHERE id=?
	`, outboxStateRetrying, nextAttempt, now.Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}

	var currentDB atomic.Pointer[sql.DB]
	currentDB.Store(initialDB)
	deps := outboxTestDeps(initialDB, 5*time.Millisecond)
	deps.DB = currentDB.Load
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	if err := svc.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatalf("PreparePersistenceReplacement() error=%v", err)
	}

	restoredDB := openOutboxTestDB(t, "reload-retrying-restored.db")
	if err := EnsureSchema(restoredDB); err != nil {
		t.Fatal(err)
	}
	insertPendingOutboxIntent(t, restoredDB, intent, now.Add(-time.Hour))
	currentDB.Store(restoredDB)
	if err := svc.ReloadPersistence(context.Background()); err != nil {
		t.Fatalf("ReloadPersistence() error=%v", err)
	}

	var state, restoredNextAttempt, restoredError string
	var attempts, statusCode int
	if err := restoredDB.QueryRow(`
		SELECT state, attempts, next_attempt_at, status_code, error
		  FROM notification_outbox WHERE id=?
	`, rowID).Scan(&state, &attempts, &restoredNextAttempt, &statusCode, &restoredError); err != nil {
		t.Fatal(err)
	}
	if state != outboxStateRetrying || attempts != 2 || restoredNextAttempt != nextAttempt || statusCode != 503 || restoredError != "temporary failure" {
		t.Fatalf("restored retry state=(%q,%d,%q,%d,%q), want retrying budget preserved", state, attempts, restoredNextAttempt, statusCode, restoredError)
	}
}

func TestNotificationOutboxReloadReinsertsCurrentRowMissingFromRestore(t *testing.T) {
	now := time.Now().UTC()
	nextAttempt := now.Add(time.Hour).Format(time.RFC3339Nano)
	intent := DeliveryIntent{
		ID: "current-only-retrying", Action: EventUpdateComplete, TargetType: "server",
		TargetName: "srv-current", Status: "failure", Message: "Current delivery is still retrying",
		MetaJSON: `{"safe":"retained"}`,
	}
	initialDB := openOutboxTestDB(t, "reload-current-only-initial.db")
	if err := EnsureSchema(initialDB); err != nil {
		t.Fatal(err)
	}
	rowID := insertPendingOutboxIntent(t, initialDB, intent, now)
	if _, err := initialDB.Exec(`
		UPDATE notification_outbox
		   SET state=?, attempts=2, next_attempt_at=?, attempted_at=?, status_code=503, error='temporary failure'
		 WHERE id=?
	`, outboxStateRetrying, nextAttempt, now.Format(time.RFC3339Nano), rowID); err != nil {
		t.Fatal(err)
	}

	var currentDB atomic.Pointer[sql.DB]
	currentDB.Store(initialDB)
	deps := outboxTestDeps(initialDB, 5*time.Millisecond)
	deps.DB = currentDB.Load
	svc := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, svc) })
	if err := svc.PreparePersistenceReplacement(context.Background()); err != nil {
		t.Fatalf("PreparePersistenceReplacement() error=%v", err)
	}

	restoredDB := openOutboxTestDB(t, "reload-current-only-restored.db")
	if err := EnsureSchema(restoredDB); err != nil {
		t.Fatal(err)
	}
	currentDB.Store(restoredDB)
	if err := svc.ReloadPersistence(context.Background()); err != nil {
		t.Fatalf("ReloadPersistence() error=%v", err)
	}

	var state, targetName, message, metaJSON, restoredNextAttempt, restoredError string
	var attempts, statusCode int
	if err := restoredDB.QueryRow(`
		SELECT state, target_name, message, meta_json, attempts, next_attempt_at, status_code, error
		  FROM notification_outbox WHERE id=?
	`, rowID).Scan(&state, &targetName, &message, &metaJSON, &attempts, &restoredNextAttempt, &statusCode, &restoredError); err != nil {
		t.Fatal(err)
	}
	if state != outboxStateRetrying || targetName != intent.TargetName || message != intent.Message ||
		metaJSON != intent.MetaJSON || attempts != 2 || restoredNextAttempt != nextAttempt ||
		statusCode != 503 || restoredError != "temporary failure" {
		t.Fatalf("restored current-only row=(%q,%q,%q,%q,%d,%q,%d,%q), want complete retrying row preserved",
			state, targetName, message, metaJSON, attempts, restoredNextAttempt, statusCode, restoredError)
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
