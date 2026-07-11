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
	svc := NewService(ServiceDeps{
		DB: func() *sql.DB { return db },
		Now: func() time.Time {
			return time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC)
		},
		Backoff:   func(int) time.Duration { return 0 },
		Logf:      func(string, ...any) {},
		QueueSize: queueSize,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})
	if _, err := svc.SaveSettings(Settings{
		Enabled:    true,
		WebhookURL: server.URL,
		EventTypes: []string{EventUpdateComplete},
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

func TestNotificationDeliveryLifecycleReportsSkipCapacityAndClosing(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	svc, _ := newTestServiceWithQueue(t, func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
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
	if got := svc.Accept(intent); got.State != AdmissionRejected {
		t.Fatalf("saturated Accept() = %+v, want rejected", got)
	}
	close(release)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
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
	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if settings.LastDelivery == nil || !settings.LastDelivery.Success || settings.LastDelivery.TargetName != "srv-a" {
		t.Fatalf("last delivery = %+v, want saved success for srv-a", settings.LastDelivery)
	}
}

func TestAcceptedDeliveryRetriesAndStoresFailure(t *testing.T) {
	var attempts int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "down", http.StatusBadGateway)
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
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("attempts=%d, want three failed attempts", attempts)
	}
	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if settings.LastDelivery == nil || settings.LastDelivery.Success || !strings.Contains(settings.LastDelivery.Error, "502") {
		t.Fatalf("last delivery = %+v, want saved HTTP failure", settings.LastDelivery)
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
	svc := NewService(ServiceDeps{DB: func() *sql.DB { return db }, Logf: func(string, ...any) {}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})
	if _, err := svc.SaveSettings(Settings{Enabled: true, WebhookURL: "ftp://example.test/hook", EventTypes: []string{EventUpdateComplete}}); err == nil {
		t.Fatalf("SaveSettings() accepted invalid URL")
	}
	if _, err := svc.SaveSettings(Settings{Enabled: false, EventTypes: []string{"unknown.event"}}); err == nil {
		t.Fatalf("SaveSettings() accepted unsupported event type")
	}
	resp, err := svc.SaveSettings(Settings{Enabled: false, EventTypes: []string{}})
	if err != nil {
		t.Fatalf("SaveSettings(empty events) error = %v", err)
	}
	if len(resp.EventTypes) != 0 {
		t.Fatalf("EventTypes = %+v, want explicit empty selection preserved", resp.EventTypes)
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
	if _, err := svc.SaveSettings(Settings{
		Enabled: true, WebhookURL: replacementURL, EventTypes: []string{EventScheduleRunFailed},
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
	if settings.WebhookURL != replacementURL || len(settings.EventTypes) != 1 || settings.EventTypes[0] != EventScheduleRunFailed {
		t.Fatalf("settings = %+v, want concurrent replacement preserved", settings)
	}
	if settings.LastDelivery == nil || !settings.LastDelivery.Success {
		t.Fatalf("last delivery = %+v, want recorded outcome", settings.LastDelivery)
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
