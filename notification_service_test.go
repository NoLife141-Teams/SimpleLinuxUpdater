package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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
	webhookURL := webhook.URL + "/hook?token=secret-value"

	updateBody := bytes.NewBufferString(`{
		"enabled":true,
		"webhook_url":"` + webhookURL + `",
		"webhook_url_intent":"replace",
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
	if !settings.Enabled || !settings.WebhookConfigured || settings.WebhookURLMasked == "" ||
		strings.Contains(rec.Body.String(), "secret-value") || strings.Contains(rec.Body.String(), `"webhook_url":`) ||
		len(settings.EventTypes) != 2 || len(settings.SupportedEvents) == 0 {
		t.Fatalf("settings = %+v, want enabled masked webhook with supported events", settings)
	}
	var persistedRaw string
	if err := app.Deps.DB().QueryRow("SELECT value FROM settings WHERE key = ?", notificationpkg.SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatalf("load notification persistence: %v", err)
	}
	if strings.Contains(persistedRaw, webhookURL) || strings.Contains(persistedRaw, "secret-value") {
		t.Fatalf("notification persistence leaked webhook URL: %s", persistedRaw)
	}

	preserveBody := bytes.NewBufferString(`{
		"enabled":true,
		"webhook_url_intent":"preserve",
		"event_types":["update.complete"]
	}`)
	preserveRec := httptest.NewRecorder()
	preserveReq := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", preserveBody)
	preserveReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(preserveReq)
	preserveReq.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(preserveRec, preserveReq)
	if preserveRec.Code != http.StatusOK || strings.Contains(preserveRec.Body.String(), "secret-value") {
		t.Fatalf("preserve status = %d body=%s", preserveRec.Code, preserveRec.Body.String())
	}
	if err := json.Unmarshal(preserveRec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal preserved settings: %v", err)
	}
	if !settings.WebhookConfigured || settings.WebhookURLIntent != notificationpkg.WebhookURLPreserve {
		t.Fatalf("preserved settings = %+v", settings)
	}

	getRec := httptest.NewRecorder()
	getReq := httptest.NewRequest(http.MethodGet, "/api/notifications/settings", nil)
	getReq.AddCookie(sessionCookie)
	app.Handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK || getRec.Header().Get("Cache-Control") != "no-store" ||
		strings.Contains(getRec.Body.String(), "secret-value") || strings.Contains(getRec.Body.String(), `"webhook_url":`) ||
		strings.Contains(getRec.Body.String(), `"last_delivery"`) {
		t.Fatalf("GET settings status=%d cache=%q body=%s", getRec.Code, getRec.Header().Get("Cache-Control"), getRec.Body.String())
	}

	testRec := httptest.NewRecorder()
	testReq := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	testReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(testReq)
	app.Handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusOK {
		t.Fatalf("POST /api/notifications/test status = %d, want %d (body=%s)", testRec.Code, http.StatusOK, testRec.Body.String())
	}
	var testOutcome struct {
		LastAttempt NotificationDeliveryStatus `json:"last_attempt"`
	}
	if err := json.Unmarshal(testRec.Body.Bytes(), &testOutcome); err != nil {
		t.Fatalf("unmarshal notification test outcome: %v", err)
	}
	if testOutcome.LastAttempt.Outcome != notificationpkg.DeliveryOutcomeSucceeded ||
		testOutcome.LastAttempt.AttemptedAt == "" || testOutcome.LastAttempt.CompletedAt == "" ||
		testOutcome.LastAttempt.ConsecutiveFailures != 0 || testOutcome.LastAttempt.StatusCode != http.StatusAccepted {
		t.Fatalf("test outcome = %+v, want successful canonical diagnostics", testOutcome.LastAttempt)
	}
	select {
	case payload := <-received:
		if payload.EventType != notificationpkg.EventTest || payload.TargetName != "webhook" {
			t.Fatalf("test payload = %+v, want notification test", payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for test webhook")
	}
	diagnosticsRec := httptest.NewRecorder()
	diagnosticsReq := httptest.NewRequest(http.MethodGet, "/api/notifications/delivery-diagnostics", nil)
	diagnosticsReq.AddCookie(sessionCookie)
	app.Handler.ServeHTTP(diagnosticsRec, diagnosticsReq)
	if diagnosticsRec.Code != http.StatusOK || diagnosticsRec.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("GET delivery diagnostics status=%d cache=%q body=%s", diagnosticsRec.Code, diagnosticsRec.Header().Get("Cache-Control"), diagnosticsRec.Body.String())
	}
	var diagnostics NotificationDeliveryDiagnostics
	if err := json.Unmarshal(diagnosticsRec.Body.Bytes(), &diagnostics); err != nil {
		t.Fatalf("unmarshal delivery diagnostics: %v", err)
	}
	if diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != notificationpkg.DeliveryOutcomeSucceeded ||
		diagnostics.LastAttempt.EventType != notificationpkg.EventTest {
		t.Fatalf("delivery diagnostics = %+v, want latest test outcome", diagnostics.LastAttempt)
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

	clearRec := httptest.NewRecorder()
	clearReq := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", bytes.NewBufferString(`{
		"enabled":false,
		"webhook_url_intent":"clear",
		"event_types":["update.complete"]
	}`))
	clearReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(clearReq)
	clearReq.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(clearRec, clearReq)
	if clearRec.Code != http.StatusOK {
		t.Fatalf("clear status = %d body=%s", clearRec.Code, clearRec.Body.String())
	}
	if err := json.Unmarshal(clearRec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("unmarshal cleared settings: %v", err)
	}
	if settings.WebhookConfigured || settings.WebhookURLIntent != notificationpkg.WebhookURLClear {
		t.Fatalf("cleared settings = %+v", settings)
	}
}

func TestNativeNotificationSettingsAPIProtectsSecrets(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "native-notification-api.db")})
	sessionCookie := app.authenticate(t)
	const (
		discordURL    = "https://discord.com/api/webhooks/123456/api-discord-secret"
		telegramToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"
		telegramChat  = "-1001234567890"
	)
	body := `{
		"enabled":false,
		"webhook_url_intent":"preserve",
		"event_types":["update.complete"],
		"discord":{
			"enabled":true,
			"webhook_url_intent":"replace",
			"webhook_url":"` + discordURL + `"
		},
		"telegram":{
			"enabled":true,
			"credentials_intent":"replace",
			"bot_token":"` + telegramToken + `",
			"chat_id":"` + telegramChat + `"
		}
	}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", bytes.NewBufferString(body))
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("native notification settings status=%d body=%s", rec.Code, rec.Body.String())
	}
	var settings NotificationSettingsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &settings); err != nil {
		t.Fatalf("decode native notification settings: %v", err)
	}
	if !settings.Discord.Enabled || !settings.Discord.Configured ||
		!settings.Telegram.Enabled || !settings.Telegram.Configured {
		t.Fatalf("settings = %+v, want enabled native destinations", settings)
	}
	for _, secret := range []string{discordURL, "api-discord-secret", telegramToken, telegramChat} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("settings response leaked %q: %s", secret, rec.Body.String())
		}
	}
	var persisted string
	if err := app.Deps.DB().QueryRow("SELECT value FROM settings WHERE key = ?", notificationpkg.SettingsKey).Scan(&persisted); err != nil {
		t.Fatalf("load native notification persistence: %v", err)
	}
	for _, secret := range []string{discordURL, "api-discord-secret", telegramToken, telegramChat} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("settings persistence leaked %q: %s", secret, persisted)
		}
	}
}

func TestNotificationDeliveryDiagnosticsRedactRemoteFailureDetails(t *testing.T) {
	const secret = "remote-body-secret"
	webhook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, secret, http.StatusBadGateway)
	}))
	t.Cleanup(webhook.Close)

	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "notification-diagnostics-api.db")})
	sessionCookie := app.authenticate(t)
	updateRec := httptest.NewRecorder()
	updateReq := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", bytes.NewBufferString(`{
		"enabled":true,
		"webhook_url_intent":"replace",
		"webhook_url":"`+webhook.URL+`/hook?token=url-secret",
		"event_types":["update.complete"]
	}`))
	updateReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(updateReq)
	updateReq.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("configure failing webhook status=%d body=%s", updateRec.Code, updateRec.Body.String())
	}

	testRec := httptest.NewRecorder()
	testReq := httptest.NewRequest(http.MethodPost, "/api/notifications/test", nil)
	testReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(testReq)
	app.Handler.ServeHTTP(testRec, testReq)
	if testRec.Code != http.StatusBadRequest {
		t.Fatalf("POST failing notification test status=%d body=%s", testRec.Code, testRec.Body.String())
	}
	if strings.Contains(testRec.Body.String(), secret) || strings.Contains(testRec.Body.String(), "url-secret") ||
		strings.Contains(testRec.Body.String(), webhook.URL) {
		t.Fatalf("failing notification response leaked remote details: %s", testRec.Body.String())
	}
	var outcome struct {
		LastAttempt NotificationDeliveryStatus `json:"last_attempt"`
	}
	if err := json.Unmarshal(testRec.Body.Bytes(), &outcome); err != nil {
		t.Fatalf("unmarshal failed diagnostics: %v", err)
	}
	if outcome.LastAttempt.Outcome != notificationpkg.DeliveryOutcomeFailed ||
		outcome.LastAttempt.StatusCode != http.StatusBadGateway ||
		outcome.LastAttempt.ConsecutiveFailures != 3 ||
		outcome.LastAttempt.NextRetryAt != "" ||
		outcome.LastAttempt.Error != "webhook returned HTTP 502" {
		t.Fatalf("failed outcome = %+v, want safe terminal diagnostics", outcome.LastAttempt)
	}

	diagnosticsRec := httptest.NewRecorder()
	diagnosticsReq := httptest.NewRequest(http.MethodGet, "/api/notifications/delivery-diagnostics", nil)
	diagnosticsReq.AddCookie(sessionCookie)
	app.Handler.ServeHTTP(diagnosticsRec, diagnosticsReq)
	if diagnosticsRec.Code != http.StatusOK || strings.Contains(diagnosticsRec.Body.String(), secret) ||
		strings.Contains(diagnosticsRec.Body.String(), "url-secret") || strings.Contains(diagnosticsRec.Body.String(), webhook.URL) {
		t.Fatalf("GET failed diagnostics status=%d body=%s", diagnosticsRec.Code, diagnosticsRec.Body.String())
	}

	audits, err := app.Deps.AuditService.List(AuditListFilter{Action: "notifications.test"})
	if err != nil {
		t.Fatalf("list notification test audits: %v", err)
	}
	auditJSON, _ := json.Marshal(audits)
	if strings.Contains(string(auditJSON), secret) || strings.Contains(string(auditJSON), "url-secret") ||
		strings.Contains(string(auditJSON), webhook.URL) {
		t.Fatalf("notification test audit leaked remote details: %s", auditJSON)
	}
}

func TestNotificationSettingsAPIRejectsUnsafeReplacementWithoutEchoingSecrets(t *testing.T) {
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "notification-validation-api.db")})
	sessionCookie := app.authenticate(t)
	const secret = "do-not-echo-secret"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/notifications/settings", bytes.NewBufferString(`{
		"enabled":true,
		"webhook_url_intent":"replace",
		"webhook_url":"https://admin:`+secret+`@example.test/hook",
		"event_types":["update.complete"]
	}`))
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("unsafe replacement status = %d, want %d body=%s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("unsafe replacement response leaked credentials: %s", rec.Body.String())
	}

	audits, err := app.Deps.AuditService.List(AuditListFilter{Action: "notifications.settings"})
	if err != nil {
		t.Fatalf("list notification audit events: %v", err)
	}
	auditJSON, _ := json.Marshal(audits)
	if strings.Contains(string(auditJSON), secret) || strings.Contains(string(auditJSON), "admin:") {
		t.Fatalf("notification audit leaked credentials: %s", auditJSON)
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
		{name: "delivery diagnostics", method: http.MethodGet, path: "/api/notifications/delivery-diagnostics"},
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
