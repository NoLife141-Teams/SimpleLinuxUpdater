package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	notificationpkg "debian-updater/internal/notifications"
)

func TestNotificationDeliveryHistoryIsScopedRedactedAndPaged(t *testing.T) {
	const endpoint = "/api/notifications/delivery-history"
	job := strings.Repeat("a", 32)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "notification-history.db"), AppDeps{
		Now: func() time.Time { return now },
	})
	admin := app.authenticate(t)
	db := app.Deps.DB()
	seed := func(id int, destination, state, jobID, reason string) {
		t.Helper()
		metadata, err := json.Marshal(map[string]any{
			"job_id": jobID, "approval_generation": 7, "approval_url": "https://secret.example/?token=secret-value",
		})
		if err != nil {
			t.Fatal(err)
		}
		created := now.Format(time.RFC3339Nano)
		next := now.Add(time.Hour).Format(time.RFC3339Nano)
		if _, err := db.Exec(`INSERT INTO notification_outbox
			(id,destination,event_type,action,message,destination_fingerprint,meta_json,state,attempts,created_at,updated_at,
			next_attempt_at,claimed_at,attempted_at,completed_at,status_code,error)
			VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("%064x", id), destination, "approval.published", "approval.published",
			"secret-value", "secret-fingerprint", string(metadata), state, id, created, created, next, next, created, created, 503, reason); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 22; i++ {
		seed(i, "telegram", "succeeded", "", "")
	}
	seed(23, "webhook", "retrying", job, "webhook returned HTTP 503 https://secret.example/?token=secret-value")
	seed(24, "discord", "succeeded", job, "")
	seed(25, "telegram", "skipped", job, "destination configuration changed after notification admission")
	query := func(filters url.Values) notificationpkg.DeliveryHistoryPage {
		t.Helper()
		rec := identityRequest(app, "GET", endpoint+"?"+filters.Encode(), admin, "", nil)
		var result notificationpkg.DeliveryHistoryPage
		if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") || json.Unmarshal(rec.Body.Bytes(), &result) != nil {
			t.Fatalf("read delivery history: %d %s", rec.Code, rec.Body.String())
		}
		for _, secret := range []string{"secret.example", "secret-value", "secret-fingerprint", "meta_json", "approval_url"} {
			if strings.Contains(rec.Body.String(), secret) {
				t.Fatalf("history exposed %q", secret)
			}
		}
		return result
	}
	byJob := query(url.Values{"job_id": {job}})
	if len(byJob.Items) != 3 || byJob.RetentionSeconds != int64(30*24*time.Hour/time.Second) {
		t.Fatalf("unexpected result for the job: %+v", byJob)
	}
	skipped, delivered, retrying := byJob.Items[0], byJob.Items[1], byJob.Items[2]
	if skipped.State != "skipped" || skipped.ReasonCode != "destination_configuration_changed" || skipped.Reason != "Destination configuration changed after admission." ||
		delivered.State != "succeeded" || delivered.Destination != "discord" || delivered.ApprovalGeneration != 7 ||
		retrying.State != "retrying" || retrying.Reason != "Destination returned HTTP 503." || retrying.NextAttemptAt == "" {
		t.Fatalf("delivery states or evidence were lost: %+v", byJob.Items)
	}
	filtered := query(url.Values{"job_id": {job}, "state": {"retrying"}, "destination": {"webhook"}})
	if len(filtered.Items) != 1 || filtered.Items[0] != retrying {
		t.Fatalf("combined filters failed: %+v", filtered)
	}
	first := query(url.Values{})
	if len(first.Items) != 20 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	seed(26, "telegram", "succeeded", "", "")
	if _, err := db.Exec(`UPDATE notification_outbox SET created_at = ? WHERE id = ?`,
		now.Add(100*time.Millisecond).Format(time.RFC3339Nano), fmt.Sprintf("%064x", 26)); err != nil {
		t.Fatal(err)
	}
	older := query(url.Values{"before": {first.NextCursor}})
	if len(older.Items) != 5 || older.NextCursor != "" || older.Items[0].Attempts != 5 || older.Items[4].Attempts != 1 {
		t.Fatalf("pagination drifted after a new delivery: %+v", older)
	}
	var retained int
	if err := db.QueryRow(`SELECT COUNT(*) FROM notification_outbox`).Scan(&retained); err != nil || retained != 26 {
		t.Fatalf("read history changed the outbox: rows=%d err=%v", retained, err)
	}
	seed(27, "discord", "skipped", "", "")
	seed(28, "discord", "skipped", "", "legacy error https://secret.example/?token=secret-value")
	legacy := query(url.Values{"destination": {"discord"}, "state": {"skipped"}})
	if len(legacy.Items) != 2 {
		t.Fatalf("missing legacy skips: %+v", legacy)
	}
	for _, item := range legacy.Items {
		if item.ReasonCode != "" || item.Reason != "Delivery was no longer eligible." {
			t.Fatalf("unexpected legacy skip reason: %+v", item)
		}
	}
	for _, generation := range []string{`"7unknown"`, "7.9", "true", "9223372036854775808"} {
		metadata := fmt.Sprintf(`{"job_id":%q,"approval_generation":%s}`, job, generation)
		if _, err := db.Exec("UPDATE notification_outbox SET meta_json = ? WHERE id = ?", metadata, fmt.Sprintf("%064x", 23)); err != nil {
			t.Fatal(err)
		}
		rows := query(url.Values{"job_id": {job}, "destination": {"webhook"}}).Items
		if len(rows) != 1 || rows[0].JobID != job || rows[0].ApprovalGeneration != 0 {
			t.Fatalf("inexact generation %s was exposed: %+v", generation, rows)
		}
	}
	if _, err := db.Exec("UPDATE notification_outbox SET meta_json = ? WHERE id = ?", `{"malformed":`, fmt.Sprintf("%064x", 23)); err != nil {
		t.Fatal(err)
	}
	malformed := query(url.Values{"destination": {"webhook"}}).Items
	if len(malformed) != 1 || malformed[0].JobID != "" || malformed[0].ApprovalGeneration != 0 {
		t.Fatalf("malformed metadata prevented safe history: %+v", malformed)
	}
	for _, invalid := range []string{"destination=unknown", "state=sent", "job_id=short", "before=garbage"} {
		if rec := identityRequest(app, "GET", endpoint+"?"+invalid, admin, "", nil); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid filter %s: %d", invalid, rec.Code)
		}
	}
	for _, role := range []string{"operator", "reader"} {
		if _, err := app.Deps.AuthService.CreateAccount(role, testPasswordStrong, role); err != nil {
			t.Fatal(err)
		}
		cookie := identityLogin(t, app, role)
		if rec := identityRequest(app, "GET", endpoint, cookie, "", nil); rec.Code != http.StatusForbidden {
			t.Fatalf("%s could read delivery history: %d", role, rec.Code)
		}
	}
	if rec := identityRequest(app, "GET", endpoint, nil, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous delivery history access: %d", rec.Code)
	}
}

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

	if err := app.Deps.AuditService.Record("admin", "127.0.0.1", updateCompleteAction, "server", "srv-current", "success", "Final status: done", map[string]any{
		"upgrade_completed":      false,
		"approved_package_count": 0,
	}); err != nil {
		t.Fatalf("Record(no-op update) error = %v", err)
	}
	select {
	case payload := <-received:
		t.Fatalf("unexpected no-op update webhook: %+v", payload)
	case <-time.After(150 * time.Millisecond):
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
