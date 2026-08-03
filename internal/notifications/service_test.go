package notifications

import (
	"context"
	"database/sql"
	"encoding/base64"
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

func testWebhookCodec() (func(string) (string, error), func(string) (string, error)) {
	return func(value string) (string, error) {
			return base64.StdEncoding.EncodeToString([]byte(value)), nil
		}, func(value string) (string, error) {
			plain, err := base64.StdEncoding.DecodeString(value)
			return string(plain), err
		}
}

func TestDeliveryDiagnosticsWaitsForTransientSQLiteContention(t *testing.T) {
	db := openOutboxTestDB(t, "diagnostics-contention.db")
	prepareOutboxSettings(t, db, "https://hooks.example.test/notify", false)
	svc := NewService(outboxTestDeps(db, 5*time.Millisecond))
	t.Cleanup(func() { closeOutboxService(t, svc) })

	lockConn, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lockConn.Close()
	if _, err := lockConn.ExecContext(context.Background(), "BEGIN EXCLUSIVE"); err != nil {
		t.Fatal(err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		_, err := lockConn.ExecContext(context.Background(), "ROLLBACK")
		released <- err
	}()

	if _, err := svc.DeliveryDiagnostics(); err != nil {
		t.Fatalf("DeliveryDiagnostics() during transient SQLite contention error = %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release SQLite lock: %v", err)
	}
}

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, <-chan WebhookPayload) {
	return newTestServiceWithQueue(t, handler, defaultQueueSize)
}

func newTestServiceWithQueue(t *testing.T, handler http.HandlerFunc, queueSize int) (*Service, <-chan WebhookPayload) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notifications.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	payloads := make(chan WebhookPayload, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
			payloads <- payload
		}
		handler(w, r)
	}))
	t.Cleanup(server.Close)
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB: func() *sql.DB { return db },
		Now: func() time.Time {
			return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
		},
		Backoff:       func(int) time.Duration { return 0 },
		Logf:          func(string, ...any) {},
		QueueSize:     queueSize,
		EncryptSecret: encrypt,
		DecryptSecret: decrypt,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})
	if _, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          true,
		WebhookURL:       server.URL,
		WebhookURLIntent: WebhookURLReplace,
		EventTypes:       []string{EventUpdateComplete},
	}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	return svc, payloads
}

func TestNotificationDeliveryLifecycleAcceptsAndDeliversAuditIntent(t *testing.T) {
	svc, payloads := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	admission := svc.Accept(DeliveryIntent{
		CreatedAt: "2026-05-17T12:00:00Z", Action: EventUpdateComplete,
		TargetType: "server", TargetName: "srv-a", Status: "success", MetaJSON: `{}`,
	})
	if admission.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", admission)
	}
	select {
	case payload := <-payloads:
		if payload.TargetName != "srv-a" || payload.EventType != EventUpdateComplete {
			t.Fatalf("payload = %+v, want accepted audit intent", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("accepted notification was not delivered")
	}
}

func TestNotificationDeliveryLifecycleSkipsNoOpUpdate(t *testing.T) {
	svc, payloads := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	admission := svc.Accept(DeliveryIntent{
		CreatedAt:  "2026-05-17T12:00:00Z",
		Action:     EventUpdateComplete,
		TargetType: "server",
		TargetName: "srv-current",
		Status:     "success",
		Message:    "Final status: done",
		MetaJSON:   `{"upgrade_completed":false,"approved_package_count":0}`,
	})
	if admission.State != AdmissionSkipped {
		t.Fatalf("Accept() = %+v, want no-op update skipped", admission)
	}
	select {
	case payload := <-payloads:
		t.Fatalf("unexpected no-op update notification: %+v", payload)
	case <-time.After(100 * time.Millisecond):
	}

	failedAdmission := svc.Accept(DeliveryIntent{
		Action:     EventUpdateComplete,
		TargetName: "srv-current",
		Status:     "failure",
		MetaJSON:   `{"upgrade_completed":false,"approved_package_count":0}`,
	})
	if failedAdmission.State != AdmissionAdmitted {
		t.Fatalf("failed no-op Accept() = %+v, want failure admitted", failedAdmission)
	}
	select {
	case payload := <-payloads:
		if payload.Status != "failure" {
			t.Fatalf("failure payload = %+v, want failure notification", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("failed update notification was not delivered")
	}

	completedAdmission := svc.Accept(DeliveryIntent{
		Action:     EventUpdateComplete,
		TargetName: "srv-updated",
		Status:     "success",
		MetaJSON:   `{"upgrade_completed":true,"approved_package_count":3}`,
	})
	if completedAdmission.State != AdmissionAdmitted {
		t.Fatalf("completed update Accept() = %+v, want admitted", completedAdmission)
	}
	select {
	case payload := <-payloads:
		if payload.TargetName != "srv-updated" {
			t.Fatalf("completed update payload = %+v", payload)
		}
	case <-time.After(time.Second):
		t.Fatal("completed update notification was not delivered")
	}
}

func TestNotificationDeliveryLifecycleReportsSkipCapacityAndClosing(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var delivered int32
	svc, _ := newTestServiceWithQueue(t, func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		atomic.AddInt32(&delivered, 1)
		w.WriteHeader(http.StatusAccepted)
	}, 1)

	skipped := svc.Accept(DeliveryIntent{Action: EventBackupRestore, MetaJSON: `{}`})
	if skipped.State != AdmissionSkipped {
		t.Fatalf("disabled Accept() = %+v, want skipped", skipped)
	}
	intent := DeliveryIntent{Action: EventUpdateComplete, TargetName: "srv", MetaJSON: `{}`}
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		t.Fatalf("first Accept() = %+v, want admitted", got)
	}
	<-started
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		t.Fatalf("queued Accept() = %+v, want admitted", got)
	}
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		close(release)
		t.Fatalf("saturated Accept() = %+v, want durable admission", got)
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
	close(release)
	if err := <-closeDone; err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if got := atomic.LoadInt32(&delivered); got != 1 {
		t.Fatalf("delivered=%d, want shutdown to finish only the in-flight notification", got)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() error = %v", err)
	}
	if diagnostics.PendingCount != 2 {
		t.Fatalf("pending=%d, want two admitted notifications retained for restart", diagnostics.PendingCount)
	}
	if got := svc.Accept(intent); got.State != AdmissionClosing {
		t.Fatalf("post-close Accept() = %+v, want closing", got)
	}
}

func TestAcceptedDeliveryPostsRedactedPayloadAndStoresStatus(t *testing.T) {
	svc, payloads := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	admission := svc.Accept(DeliveryIntent{
		CreatedAt:  "2026-05-17T12:00:00Z",
		Actor:      "admin",
		Action:     EventUpdateComplete,
		TargetType: "server",
		TargetName: "srv-a",
		Status:     "success",
		Message:    "Update completed",
		MetaJSON:   `{"package_count":3,"password":"secret-value","nested":[{"apiKey":"nested-secret","safe":"visible"}]}`,
		ClientIP:   "127.0.0.1",
	})
	if admission.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", admission)
	}
	payload := <-payloads
	if payload.EventType != EventUpdateComplete || payload.TargetName != "srv-a" || payload.Meta["password"] != "[redacted]" {
		t.Fatalf("payload = %+v, want redacted update payload", payload)
	}
	nested, ok := payload.Meta["nested"].([]any)
	if !ok || len(nested) != 1 {
		t.Fatalf("nested payload meta = %#v, want one nested object", payload.Meta["nested"])
	}
	nestedMeta, ok := nested[0].(map[string]any)
	if !ok || nestedMeta["apiKey"] != "[redacted]" || nestedMeta["safe"] != "visible" {
		t.Fatalf("nested payload meta = %#v, want redacted apiKey and preserved safe value", nested[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeSucceeded ||
		diagnostics.LastAttempt.TargetName != "srv-a" {
		t.Fatalf("last attempt = %+v, want saved success for srv-a", diagnostics.LastAttempt)
	}
}

func TestAcceptedDeliveryRetriesAndStoresFailure(t *testing.T) {
	var attempts int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "remote-secret-body", http.StatusBadGateway)
	})

	admission := svc.Accept(DeliveryIntent{
		CreatedAt:  "2026-05-17T12:00:00Z",
		Action:     EventUpdateComplete,
		TargetType: "server",
		TargetName: "srv-b",
		Status:     "failure",
		Message:    "Update failed",
		MetaJSON:   `{}`,
	})
	if admission.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", admission)
	}
	waitForLatestOutboxState(t, svc.deps.DB(), outboxStateFailed)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("attempts=%d, want three failed attempts", attempts)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeFailed ||
		diagnostics.LastAttempt.ConsecutiveFailures != 3 || diagnostics.LastAttempt.NextRetryAt != "" ||
		!strings.Contains(diagnostics.LastAttempt.Error, "502") {
		t.Fatalf("last attempt = %+v, want saved terminal HTTP failure", diagnostics.LastAttempt)
	}
	var persistedRaw string
	if err := svc.deps.DB().QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatalf("load persisted failure: %v", err)
	}
	if strings.Contains(persistedRaw, "remote-secret-body") {
		t.Fatalf("remote response body leaked into persistence: %s", persistedRaw)
	}
}

func TestDeliveryDiagnosticsExposeRetryOnlyWhileScheduled(t *testing.T) {
	attempted := make(chan struct{}, 1)
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		attempted <- struct{}{}
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	svc.deps.Backoff = func(int) time.Duration { return 10 * time.Second }

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan struct {
		status DeliveryStatus
		err    error
	}, 1)
	go func() {
		status, err := svc.TestDelivery(ctx)
		result <- struct {
			status DeliveryStatus
			err    error
		}{status: status, err: err}
	}()
	<-attempted

	deadline := time.Now().Add(time.Second)
	var retrying DeliveryDiagnostics
	for time.Now().Before(deadline) {
		diagnostics, err := svc.DeliveryDiagnostics()
		if err != nil {
			t.Fatalf("DeliveryDiagnostics(retrying) error = %v", err)
		}
		if diagnostics.LastAttempt != nil && diagnostics.LastAttempt.Outcome == DeliveryOutcomeRetrying {
			retrying = diagnostics
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if retrying.LastAttempt == nil || retrying.LastAttempt.NextRetryAt == "" ||
		retrying.LastAttempt.StatusCode != http.StatusServiceUnavailable ||
		retrying.LastAttempt.ConsecutiveFailures != 1 {
		t.Fatalf("retrying diagnostics = %+v, want one failed attempt with a scheduled retry", retrying.LastAttempt)
	}

	cancel()
	final := <-result
	if final.err == nil || final.status.Outcome != DeliveryOutcomeFailed || final.status.NextRetryAt != "" {
		t.Fatalf("TestDelivery() status=%+v error=%v, want canceled terminal failure without retry", final.status, final.err)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics(final) error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeFailed ||
		diagnostics.LastAttempt.NextRetryAt != "" {
		t.Fatalf("final diagnostics = %+v, want no scheduled retry", diagnostics.LastAttempt)
	}
}

func TestDeliveryDiagnosticsUseSharedOutcomeTimestampsAndResetFailures(t *testing.T) {
	var requests int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		if atomic.AddInt32(&requests, 1) <= defaultAttempts {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	svc.deps.Backoff = func(int) time.Duration { return 0 }

	if status, err := svc.TestDelivery(context.Background()); err == nil ||
		status.Outcome != DeliveryOutcomeFailed || status.ConsecutiveFailures != defaultAttempts {
		t.Fatalf("first TestDelivery() status=%+v error=%v, want terminal failure", status, err)
	}

	base := time.Date(2026, 7, 28, 12, 0, 0, 0, time.UTC)
	var ticks int64
	svc.deps.Now = func() time.Time {
		tick := atomic.AddInt64(&ticks, 1) - 1
		return base.Add(time.Duration(tick) * 125 * time.Millisecond)
	}
	status, err := svc.TestDelivery(context.Background())
	if err != nil {
		t.Fatalf("second TestDelivery() error = %v", err)
	}
	if status.Outcome != DeliveryOutcomeSucceeded || !status.Success ||
		status.ConsecutiveFailures != 0 || status.DurationMS != 125 ||
		status.AttemptedAt == "" || status.CompletedAt == "" || status.DeliveredAt != status.CompletedAt {
		t.Fatalf("successful test status = %+v, want shared successful outcome semantics", status)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeSucceeded ||
		diagnostics.LastAttempt.ConsecutiveFailures != 0 {
		t.Fatalf("diagnostics = %+v, want failure counter reset", diagnostics.LastAttempt)
	}
}

func TestAcceptedDeliverySkipsDisabledEventTypes(t *testing.T) {
	var attempts int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	admission := svc.Accept(DeliveryIntent{
		Action:     EventBackupRestore,
		TargetType: "backup",
		TargetName: "state",
		Status:     "success",
		Message:    "Backup restored",
		MetaJSON:   `{}`,
	})
	if admission.State != AdmissionSkipped || atomic.LoadInt32(&attempts) != 0 {
		t.Fatalf("admission=%+v attempts=%d, want skipped event", admission, attempts)
	}
}

func TestSaveSettingsValidatesURLAndEvents(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notifications-validation.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB:            func() *sql.DB { return db },
		Logf:          func(string, ...any) {},
		EncryptSecret: encrypt,
		DecryptSecret: decrypt,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})
	if _, err := svc.SaveSettings(SettingsUpdate{Enabled: true, WebhookURL: "ftp://example.test/hook", WebhookURLIntent: WebhookURLReplace, EventTypes: []string{EventUpdateComplete}}); err == nil {
		t.Fatalf("SaveSettings() accepted invalid URL")
	}
	if _, err := svc.SaveSettings(SettingsUpdate{Enabled: false, EventTypes: []string{"unknown.event"}}); err == nil {
		t.Fatalf("SaveSettings() accepted unsupported event type")
	}
	resp, err := svc.SaveSettings(SettingsUpdate{Enabled: false, EventTypes: []string{}})
	if err != nil {
		t.Fatalf("SaveSettings(empty events) error = %v", err)
	}
	if len(resp.EventTypes) != 0 {
		t.Fatalf("EventTypes = %+v, want explicit empty selection preserved", resp.EventTypes)
	}
}

func TestWebhookSettingsPreserveReplaceClearAndMigrateLegacySecret(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notifications-intents.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	const legacySecretURL = "https://legacy.example.test/hook?token=legacy-secret"
	legacy, err := json.Marshal(persistedSettings{
		Enabled:          true,
		LegacyWebhookURL: legacySecretURL,
		EventTypes:       []string{EventUpdateComplete},
		LastDelivery: &DeliveryStatus{
			EventType:   EventUpdateComplete,
			Success:     false,
			Attempts:    3,
			StatusCode:  http.StatusBadGateway,
			Error:       "legacy-secret remote response body",
			DeliveredAt: "2026-07-27T12:00:00Z",
		},
	})
	if err != nil {
		t.Fatalf("marshal legacy settings: %v", err)
	}
	if _, err := db.Exec("INSERT INTO settings(key, value) VALUES(?, ?)", SettingsKey, string(legacy)); err != nil {
		t.Fatalf("insert legacy settings: %v", err)
	}
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB:            func() *sql.DB { return db },
		EncryptSecret: encrypt,
		DecryptSecret: decrypt,
		Logf:          func(string, ...any) {},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})

	status, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings(legacy) error = %v", err)
	}
	responseJSON, _ := json.Marshal(status)
	if !status.WebhookConfigured || status.WebhookURLMasked != "https://legacy.example.test/••••" ||
		strings.Contains(string(responseJSON), "legacy-secret") || strings.Contains(string(responseJSON), legacySecretURL) {
		t.Fatalf("legacy status = %+v json=%s, want configured masked response", status, responseJSON)
	}
	var persistedRaw string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatalf("load automatically migrated settings: %v", err)
	}
	if strings.Contains(persistedRaw, legacySecretURL) || strings.Contains(persistedRaw, "legacy-secret") ||
		strings.Contains(persistedRaw, `"webhook_url":`) || !strings.Contains(persistedRaw, `"webhook_url_enc":`) {
		t.Fatalf("legacy read did not protect persisted URL: %s", persistedRaw)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics(legacy) error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeFailed ||
		diagnostics.LastAttempt.AttemptedAt != "2026-07-27T12:00:00Z" ||
		diagnostics.LastAttempt.CompletedAt != "2026-07-27T12:00:00Z" ||
		diagnostics.LastAttempt.ConsecutiveFailures != 1 ||
		diagnostics.LastAttempt.Error != "Webhook delivery failed." {
		t.Fatalf("legacy diagnostics = %+v, want compatible safe failure", diagnostics.LastAttempt)
	}

	preserved, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          false,
		WebhookURLIntent: WebhookURLPreserve,
		EventTypes:       []string{EventBackupRestore},
	})
	if err != nil {
		t.Fatalf("SaveSettings(preserve) error = %v", err)
	}
	if preserved.WebhookURLIntent != WebhookURLPreserve || !preserved.WebhookConfigured || preserved.Enabled {
		t.Fatalf("preserved response = %+v", preserved)
	}
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatalf("load migrated settings: %v", err)
	}
	if strings.Contains(persistedRaw, legacySecretURL) || strings.Contains(persistedRaw, "legacy-secret") ||
		strings.Contains(persistedRaw, `"webhook_url":`) || !strings.Contains(persistedRaw, `"webhook_url_enc":`) {
		t.Fatalf("preserved persistence leaked legacy URL: %s", persistedRaw)
	}

	const replacementSecretURL = "https://replacement.example.test/new?token=replacement-secret"
	replaced, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          true,
		WebhookURL:       replacementSecretURL,
		WebhookURLIntent: WebhookURLReplace,
		EventTypes:       []string{EventUpdateComplete},
	})
	if err != nil {
		t.Fatalf("SaveSettings(replace) error = %v", err)
	}
	if replaced.WebhookURLIntent != WebhookURLReplace || replaced.WebhookURLMasked != "https://replacement.example.test/••••" {
		t.Fatalf("replaced response = %+v", replaced)
	}
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatalf("load replaced settings: %v", err)
	}
	if strings.Contains(persistedRaw, replacementSecretURL) || strings.Contains(persistedRaw, "replacement-secret") {
		t.Fatalf("replacement URL leaked into persistence: %s", persistedRaw)
	}

	cleared, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          false,
		WebhookURLIntent: WebhookURLClear,
		EventTypes:       []string{EventUpdateComplete},
	})
	if err != nil {
		t.Fatalf("SaveSettings(clear) error = %v", err)
	}
	if cleared.WebhookURLIntent != WebhookURLClear || cleared.WebhookConfigured || cleared.WebhookURLMasked != "" {
		t.Fatalf("cleared response = %+v", cleared)
	}
}

func TestWebhookReplacementPolicyRejectsAmbiguousAndCredentialBearingURLs(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notifications-policy.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{DB: func() *sql.DB { return db }, EncryptSecret: encrypt, DecryptSecret: decrypt})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})

	tests := []struct {
		name   string
		update SettingsUpdate
		want   string
	}{
		{"public HTTP", SettingsUpdate{WebhookURLIntent: WebhookURLReplace, WebhookURL: "http://example.com/hook"}, "Use HTTPS"},
		{"embedded credentials", SettingsUpdate{WebhookURLIntent: WebhookURLReplace, WebhookURL: "https://admin:secret@example.com/hook"}, "credentials"},
		{"missing replacement", SettingsUpdate{WebhookURLIntent: WebhookURLReplace}, "required"},
		{"enabled clear", SettingsUpdate{Enabled: true, WebhookURLIntent: WebhookURLClear}, "Disable"},
		{"unknown intent", SettingsUpdate{WebhookURLIntent: "rotate"}, "preserve, replace, or clear"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := svc.SaveSettings(tt.update)
			var validationErr *ValidationError
			if !errors.As(err, &validationErr) || !strings.Contains(validationErr.Error(), tt.want) {
				t.Fatalf("SaveSettings() error = %v, want validation containing %q", err, tt.want)
			}
			if strings.Contains(validationErr.Error(), "admin:secret") {
				t.Fatalf("validation error leaked URL credentials: %v", validationErr)
			}
		})
	}
	for _, accepted := range []string{
		"https://public.example.com/hook",
		"http://localhost:8080/hook",
		"http://192.168.1.5/hook",
		"http://hooks.example.internal/hook",
	} {
		if _, err := svc.SaveSettings(SettingsUpdate{
			WebhookURLIntent: WebhookURLReplace,
			WebhookURL:       accepted,
			EventTypes:       []string{},
		}); err != nil {
			t.Fatalf("SaveSettings(%q) error = %v", accepted, err)
		}
	}
}

func TestReencryptStoredWebhookURLHandlesEncryptedAndLegacyBackupSettings(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "encrypted", true: "legacy"}[legacy], func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notification-rewrap.db"))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
				t.Fatalf("create settings table: %v", err)
			}
			const webhookURL = "https://backup.example.test/hook?token=backup-secret"
			stored := persistedSettings{Enabled: true, EventTypes: []string{EventUpdateComplete}}
			if legacy {
				stored.LegacyWebhookURL = webhookURL
			} else {
				stored.EncryptedWebhookURL = "old:" + base64.StdEncoding.EncodeToString([]byte(webhookURL))
			}
			body, _ := json.Marshal(stored)
			if _, err := db.Exec("INSERT INTO settings(key, value) VALUES(?, ?)", SettingsKey, string(body)); err != nil {
				t.Fatalf("insert settings: %v", err)
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			err = ReencryptStoredWebhookURL(
				context.Background(),
				tx,
				func(value string) (string, error) {
					plain, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(value, "old:"))
					return string(plain), err
				},
				func(value string) (string, error) {
					return "new:" + base64.StdEncoding.EncodeToString([]byte(value)), nil
				},
			)
			if err != nil {
				_ = tx.Rollback()
				t.Fatalf("ReencryptStoredWebhookURL() error = %v", err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatalf("commit transaction: %v", err)
			}
			var raw string
			if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&raw); err != nil {
				t.Fatalf("load rewrapped settings: %v", err)
			}
			if strings.Contains(raw, webhookURL) || strings.Contains(raw, "backup-secret") ||
				strings.Contains(raw, `"webhook_url":`) || !strings.Contains(raw, `"webhook_url_enc":"new:`) {
				t.Fatalf("rewrapped settings = %s", raw)
			}
		})
	}
}

func TestDeliveryOutcomeDoesNotOverwriteConcurrentSettings(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusAccepted)
	})
	intent := DeliveryIntent{Action: EventUpdateComplete, TargetName: "srv", MetaJSON: `{}`}
	if got := svc.Accept(intent); got.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", got)
	}
	<-started
	const replacementURL = "https://replacement.example.test/hook"
	if _, err := svc.SaveSettings(SettingsUpdate{
		Enabled: true, WebhookURL: replacementURL, WebhookURLIntent: WebhookURLReplace, EventTypes: []string{EventScheduleRunFailed},
	}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if !settings.WebhookConfigured || settings.WebhookURLMasked != "https://replacement.example.test/••••" || len(settings.EventTypes) != 1 || settings.EventTypes[0] != EventScheduleRunFailed {
		t.Fatalf("settings = %+v, want concurrent replacement preserved", settings)
	}
	diagnostics, err := svc.DeliveryDiagnostics()
	if err != nil {
		t.Fatalf("DeliveryDiagnostics() error = %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeSucceeded {
		t.Fatalf("last attempt = %+v, want recorded outcome", diagnostics.LastAttempt)
	}
}

func TestTestDeliveryReturnsOutcomePersistenceFailure(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	db := svc.deps.DB()
	if _, err := db.Exec(`CREATE TRIGGER reject_notification_outcome
		BEFORE UPDATE ON settings
		WHEN NEW.value LIKE '%last_delivery%'
		BEGIN SELECT RAISE(FAIL, 'outcome rejected'); END`); err != nil {
		t.Fatalf("create rejection trigger: %v", err)
	}
	status, err := svc.TestDelivery(context.Background())
	if err == nil || !strings.Contains(err.Error(), "outcome rejected") {
		t.Fatalf("TestDelivery() status=%+v error=%v, want outcome persistence failure", status, err)
	}
}

func TestNotificationDeliveryLifecycleCloseCancelsInFlightDelivery(t *testing.T) {
	started := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	svc, _ := newTestService(t, func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
		cancelled <- struct{}{}
	})
	if got := svc.Accept(DeliveryIntent{Action: EventUpdateComplete, TargetName: "srv", MetaJSON: `{}`}); got.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", got)
	}
	<-started
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error = %v, want context canceled", err)
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("in-flight delivery was not cancelled")
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), time.Second)
	defer drainCancel()
	if err := svc.Close(drainCtx); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
}
