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

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, <-chan WebhookPayload) {
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
		Backoff: func(int) time.Duration { return 0 },
		Logf:    func(string, ...any) {},
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

func TestDeliverAuditEventPostsRedactedPayloadAndStoresStatus(t *testing.T) {
	svc, payloads := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})

	status, err := svc.DeliverAuditEvent(context.Background(), AuditEvent{
		CreatedAt:  "2026-05-17T12:00:00Z",
		Actor:      "admin",
		Action:     EventUpdateComplete,
		TargetType: "server",
		TargetName: "srv-a",
		Status:     "success",
		Message:    "Update completed",
		MetaJSON:   `{"package_count":3,"password":"secret-value","nested":[{"apiKey":"nested-secret","safe":"visible"}]}`,
		ClientIP:   "127.0.0.1",
	}, false)
	if err != nil {
		t.Fatalf("DeliverAuditEvent() error = %v", err)
	}
	if !status.Success || status.Attempts != 1 || status.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %+v, want accepted success", status)
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
	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if settings.LastDelivery == nil || !settings.LastDelivery.Success || settings.LastDelivery.TargetName != "srv-a" {
		t.Fatalf("last delivery = %+v, want saved success for srv-a", settings.LastDelivery)
	}
}

func TestDeliverAuditEventRetriesAndStoresFailure(t *testing.T) {
	var attempts int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		http.Error(w, "down", http.StatusBadGateway)
	})

	status, err := svc.DeliverAuditEvent(context.Background(), AuditEvent{
		CreatedAt:  "2026-05-17T12:00:00Z",
		Action:     EventUpdateComplete,
		TargetType: "server",
		TargetName: "srv-b",
		Status:     "failure",
		Message:    "Update failed",
		MetaJSON:   `{}`,
	}, false)
	if err == nil {
		t.Fatalf("DeliverAuditEvent() error = nil, want failure")
	}
	if status.Success || status.Attempts != 3 || status.StatusCode != http.StatusBadGateway || atomic.LoadInt32(&attempts) != 3 {
		t.Fatalf("status=%+v attempts=%d, want three failed attempts", status, attempts)
	}
	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if settings.LastDelivery == nil || settings.LastDelivery.Success || !strings.Contains(settings.LastDelivery.Error, "502") {
		t.Fatalf("last delivery = %+v, want saved HTTP failure", settings.LastDelivery)
	}
}

func TestDeliverAuditEventSkipsDisabledEventTypes(t *testing.T) {
	var attempts int32
	svc, _ := newTestService(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusNoContent)
	})

	status, err := svc.DeliverAuditEvent(context.Background(), AuditEvent{
		Action:     EventBackupRestore,
		TargetType: "backup",
		TargetName: "state",
		Status:     "success",
		Message:    "Backup restored",
		MetaJSON:   `{}`,
	}, false)
	if err != nil {
		t.Fatalf("DeliverAuditEvent() error = %v", err)
	}
	if status != (DeliveryStatus{}) || atomic.LoadInt32(&attempts) != 0 {
		t.Fatalf("status=%+v attempts=%d, want skipped event", status, attempts)
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
