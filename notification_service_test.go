package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	notificationpkg "debian-updater/internal/notifications"
)

func TestNotificationSettingsAPIAndAuditDelivery(t *testing.T) {
	received := make(chan notificationpkg.WebhookPayload, 4)
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload notificationpkg.WebhookPayload
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode webhook payload: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		received <- payload
		w.WriteHeader(http.StatusAccepted)
	}))
	t.Cleanup(webhook.Close)

	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "notification-api.db")})
	sessionCookie := app.authenticate(t)

	updateBody := bytes.NewBufferString(`{
		"enabled":true,
		"webhook_url":"` + webhook.URL + `",
		"event_types":["update.complete","backup.restore"]
	}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", updateBody)
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT /api/notifications/settings status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var settings NotificationSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal notification settings: %v", err)
	}
	if !settings.Enabled || settings.WebhookURL != webhook.URL || len(settings.EventTypes) != 2 || len(settings.SupportedEvents) == 0 {
		t.Fatalf("settings = %+v, want enabled webhook with supported events", settings)
	}

	testRec := httptest.NewRecorder()
	testReq := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	testReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(testReq)
	app.Handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("POST /api/notifications/test status = %d, want %d (body=%s)", testRec.Code, http.StatusOK, testRec.Body.String())
	}
	select {
	case payload := <-received:
		if payload.EventType != notificationpkg.EventTest || payload.TargetName != "webhook" {
			t.Fatalf("test payload = %+v, want notification test", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for test webhook")
	}

	if err := app.Deps.AuditService.Record("admin", "127.0.0.1", updateCompleteAction, "server", "srv-notify", "success", "Update completed", map[string]any{
		"package_count": 2,
		"password":      "do-not-send",
	}); err != nil {
		t.Fatalf("Record() error = %v", err)
	}
	select {
	case payload := <-received:
		if payload.EventType != updateCompleteAction || payload.TargetName != "srv-notify" {
			t.Fatalf("audit payload = %+v, want update.complete for srv-notify", payload)
		}
		if _, exists := payload.Meta["password"]; exists {
			t.Fatalf("audit payload meta = %+v, want password removed by audit sanitization", payload.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for audit webhook")
	}
}

func TestNotificationRoutesRequireAuthentication(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "notification-auth.db")})
	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{name: "settings status", method: http.MethodGet, path: "/api/notifications/settings"},
		{name: "settings update", method: http.MethodPut, path: "/api/notifications/settings", body: `{"enabled":false}`},
		{name: "test", method: http.MethodPost, path: "/api/notifications/test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, bytes.NewBufferString(tc.body))
			req.Header.Set("Content-Type", "application/json")
			app.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusSeeOther && rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s status = %d, want auth rejection", tc.method, tc.path, rec.Code)
			}
		})
	}
}
