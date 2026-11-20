package notifications

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

type capturedNotificationRequest struct {
	URL  string
	Body []byte
}

type capturingNotificationClient struct {
	requests chan capturedNotificationRequest
}

func (c *capturingNotificationClient) Do(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	c.requests <- capturedNotificationRequest{URL: req.URL.String(), Body: body}
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     make(http.Header),
	}, nil
}

type notificationHTTPFunc func(*http.Request) (*http.Response, error)

func (f notificationHTTPFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

func openNativeIntegrationTestDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatalf("set SQLite busy timeout: %v", err)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatalf("set SQLite journal mode: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	return db
}

func TestNativeIntegrationsConfigureFanOutAndProtectCredentials(t *testing.T) {
	db := openNativeIntegrationTestDB(t, "native-integrations.db")
	client := &capturingNotificationClient{requests: make(chan capturedNotificationRequest, 4)}
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB:            func() *sql.DB { return db },
		HTTPClient:    client,
		EncryptSecret: encrypt,
		DecryptSecret: decrypt,
		Backoff:       func(int) time.Duration { return 0 },
		Logf:          func(string, ...any) {},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})

	const (
		discordURL    = "https://discord.com/api/webhooks/123456/discord-secret"
		telegramToken = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"
		telegramChat  = "-1001234567890"
	)
	settings, err := svc.SaveSettings(SettingsUpdate{
		Enabled:    false,
		EventTypes: []string{EventUpdateComplete},
		Discord: &DiscordUpdate{
			Enabled:          true,
			WebhookURL:       discordURL,
			WebhookURLIntent: WebhookURLReplace,
		},
		Telegram: &TelegramUpdate{
			Enabled:           true,
			BotToken:          telegramToken,
			ChatID:            telegramChat,
			CredentialsIntent: WebhookURLReplace,
		},
	})
	if err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	if !settings.Discord.Enabled || !settings.Discord.Configured ||
		!settings.Telegram.Enabled || !settings.Telegram.Configured {
		t.Fatalf("settings = %+v, want configured native integrations", settings)
	}
	var persisted string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persisted); err != nil {
		t.Fatalf("load persisted settings: %v", err)
	}
	for _, secret := range []string{discordURL, "discord-secret", telegramToken, telegramChat} {
		if strings.Contains(persisted, secret) {
			t.Fatalf("persisted settings leaked %q: %s", secret, persisted)
		}
	}

	admission := svc.Accept(DeliveryIntent{
		CreatedAt:  "2026-07-30T12:00:00Z",
		Action:     EventUpdateComplete,
		Status:     "success",
		TargetType: "server",
		TargetName: "srv-native",
		Message:    "Update completed",
		MetaJSON:   `{}`,
	})
	if admission.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", admission)
	}

	requests := make(map[string]capturedNotificationRequest, 2)
	for len(requests) < 2 {
		select {
		case request := <-client.requests:
			parsed, err := url.Parse(request.URL)
			if err != nil {
				t.Fatal(err)
			}
			requests[parsed.Hostname()] = request
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for native integration delivery")
		}
	}
	if !strings.Contains(requests["discord.com"].URL, "discord.com/api/webhooks/") || !strings.Contains(requests["discord.com"].URL, "wait=true") {
		t.Fatalf("Discord request URL = %q", requests["discord.com"].URL)
	}
	var discordBody struct {
		Embeds          []json.RawMessage `json:"embeds"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(requests["discord.com"].Body, &discordBody); err != nil {
		t.Fatalf("decode Discord body: %v", err)
	}
	if len(discordBody.Embeds) != 1 || discordBody.AllowedMentions.Parse == nil || len(discordBody.AllowedMentions.Parse) != 0 {
		t.Fatalf("Discord body = %s, want one embed and disabled mentions", requests["discord.com"].Body)
	}
	if !strings.Contains(requests["api.telegram.org"].URL, "api.telegram.org/bot"+telegramToken+"/sendMessage") {
		t.Fatalf("Telegram request URL = %q", requests["api.telegram.org"].URL)
	}
	var telegramBody telegramMessageBody
	if err := json.Unmarshal(requests["api.telegram.org"].Body, &telegramBody); err != nil {
		t.Fatalf("decode Telegram body: %v", err)
	}
	if telegramBody.ChatID != telegramChat || !strings.Contains(telegramBody.Text, "srv-native") {
		t.Fatalf("Telegram body = %+v", telegramBody)
	}

	deadline := time.Now().Add(time.Second)
	var diagnostics DeliveryDiagnostics
	for time.Now().Before(deadline) {
		diagnostics, err = svc.DeliveryDiagnostics()
		if err != nil {
			t.Fatalf("DeliveryDiagnostics() error = %v", err)
		}
		if diagnostics.LastAttempts[DestinationDiscord] != nil &&
			diagnostics.LastAttempts[DestinationTelegram] != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if diagnostics.LastAttempts[DestinationDiscord] == nil || diagnostics.LastAttempts[DestinationTelegram] == nil {
		t.Fatalf("last attempts = %+v, want Discord and Telegram outcomes", diagnostics.LastAttempts)
	}
}

func TestQueuedDestinationsAndConditionsProgressDuringTelegramTimeout(t *testing.T) {
	db := openNativeIntegrationTestDB(t, "parallel-destinations.db")
	telegramStarted := make(chan struct{})
	telegramTimedOut := make(chan struct{})
	telegramDeliveries := make(chan string, 4)
	var telegramAttempts atomic.Int32
	client := notificationHTTPFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Hostname() == "api.telegram.org" {
			if telegramAttempts.Add(1) == 1 {
				close(telegramStarted)
				<-req.Context().Done()
				close(telegramTimedOut)
				return nil, req.Context().Err()
			}
			var payload telegramMessageBody
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				return nil, err
			}
			telegramDeliveries <- payload.Text
		}
		return &http.Response{
			StatusCode: http.StatusNoContent,
			Body:       io.NopCloser(bytes.NewReader(nil)),
		}, nil
	})
	start := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	var elapsed atomic.Int64
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB:                    func() *sql.DB { return db },
		HTTPClient:            client,
		EncryptSecret:         encrypt,
		DecryptSecret:         decrypt,
		Now:                   func() time.Time { return start.Add(time.Duration(elapsed.Load())) },
		Backoff:               func(int) time.Duration { return time.Minute },
		DeliveryTimeout:       3 * time.Second,
		ClaimTTL:              100 * time.Millisecond,
		ConditionPollInterval: 10 * time.Millisecond,
		ReadConditions: func(context.Context, string, []Condition) (ConditionBatch, error) {
			return ConditionBatch{}, nil
		},
		Logf: func(string, ...any) {},
	})
	t.Cleanup(func() { closeOutboxService(t, svc) })
	if _, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          true,
		WebhookURL:       "https://hooks.example.test/notify",
		WebhookURLIntent: WebhookURLReplace,
		EventTypes:       []string{EventUpdateComplete},
		Discord: &DiscordUpdate{
			Enabled:          true,
			WebhookURL:       "https://discord.com/api/webhooks/123456/discord-secret",
			WebhookURLIntent: WebhookURLReplace,
		},
		Telegram: &TelegramUpdate{
			Enabled:           true,
			BotToken:          "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi",
			ChatID:            "-1001234567890",
			CredentialsIntent: WebhookURLReplace,
		},
	}); err != nil {
		t.Fatalf("SaveSettings() error = %v", err)
	}
	svc.StartConditionMonitoring()

	accept := func(id string) {
		t.Helper()
		if result := svc.Accept(DeliveryIntent{
			ID: id, TargetName: id, Action: EventUpdateComplete, Status: "success",
			TargetType: "server", Message: "Update completed", MetaJSON: `{}`,
		}); result.State != AdmissionAdmitted {
			t.Fatalf("Accept(%q) = %+v, want admitted", id, result)
		}
	}
	accept("first")
	select {
	case <-telegramStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("Telegram did not start")
	}
	accept("second")
	// Advance the clock past claim expiry while the first HTTP request is active.
	// This must update condition health without recovering the worker's own claim.
	elapsed.Store(int64(time.Second))
	waitForFacts := func(match func(InstallationHealthFacts) bool, until <-chan struct{}) InstallationHealthFacts {
		t.Helper()
		deadline := time.NewTimer(5 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			_, facts, err := svc.InstallationHealth(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-until:
				t.Fatalf("Telegram timed out before the other destinations and conditions progressed: %+v", facts)
			default:
			}
			if match(facts) {
				return facts
			}
			select {
			case <-deadline.C:
				t.Fatalf("notification health did not converge: %+v", facts)
			case <-tick.C:
			}
		}
	}
	waitForFacts(func(facts InstallationHealthFacts) bool {
		return facts.LastConditionEvaluationAt != nil && facts.LastConditionEvaluationAt.Equal(start.Add(time.Second)) &&
			facts.Destinations[DestinationWebhook].OldestPendingAgeSeconds == nil && facts.Destinations[DestinationWebhook].LastSuccessfulDeliveryAt != nil &&
			facts.Destinations[DestinationDiscord].OldestPendingAgeSeconds == nil && facts.Destinations[DestinationDiscord].LastSuccessfulDeliveryAt != nil &&
			facts.Pending == 2 && facts.Retrying == 0
	}, telegramTimedOut)
	if attempts := telegramAttempts.Load(); attempts != 1 {
		t.Fatalf("Telegram attempts before the first request finished = %d, want 1", attempts)
	}

	awaitTelegram := func(target string) {
		t.Helper()
		select {
		case message := <-telegramDeliveries:
			if !strings.Contains(message, "\nTarget: "+target+"\n") {
				t.Fatalf("Telegram delivery = %q, want target %q", message, target)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("Telegram did not deliver %q", target)
		}
	}
	// The second row is due before the first row's retry.
	awaitTelegram("second")
	waitForFacts(func(facts InstallationHealthFacts) bool {
		telegram := facts.Destinations[DestinationTelegram]
		return facts.Retrying == 1 && facts.Pending == 0 && telegram.LastSuccessfulDeliveryAt != nil
	}, nil)
	elapsed.Store(int64(61 * time.Second))
	svc.signalWorker()
	awaitTelegram("first")
	waitForFacts(func(facts InstallationHealthFacts) bool {
		return facts.Retrying == 0 && facts.LastSuccessfulDeliveryAt != nil &&
			facts.LastSuccessfulDeliveryAt.Equal(start.Add(61*time.Second))
	}, nil)
	if attempts := telegramAttempts.Load(); attempts != 3 {
		t.Fatalf("Telegram attempts after retry = %d, want 3", attempts)
	}
}

func TestNativeIntegrationValidationRejectsUnsafeCredentials(t *testing.T) {
	tests := []struct {
		name   string
		update SettingsUpdate
	}{
		{
			name: "unofficial Discord host",
			update: SettingsUpdate{Discord: &DiscordUpdate{
				WebhookURL:       "https://discord.example.test/api/webhooks/1/token",
				WebhookURLIntent: WebhookURLReplace,
			}},
		},
		{
			name: "invalid Telegram token",
			update: SettingsUpdate{Telegram: &TelegramUpdate{
				BotToken:          "not-a-token",
				ChatID:            "-100123",
				CredentialsIntent: WebhookURLReplace,
			}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := openNativeIntegrationTestDB(t, "validation.db")
			encrypt, decrypt := testWebhookCodec()
			svc := NewService(ServiceDeps{
				DB:            func() *sql.DB { return db },
				EncryptSecret: encrypt,
				DecryptSecret: decrypt,
				Logf:          func(string, ...any) {},
			})
			defer func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = svc.Close(ctx)
			}()
			if _, err := svc.SaveSettings(tt.update); err == nil {
				t.Fatal("SaveSettings() accepted unsafe native integration credentials")
			}
		})
	}
}
