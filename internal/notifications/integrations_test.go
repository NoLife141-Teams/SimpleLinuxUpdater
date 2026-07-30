package notifications

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

type firstDestinationTimeoutClient struct {
	successes chan string
}

func (c *firstDestinationTimeoutClient) Do(req *http.Request) (*http.Response, error) {
	if req.URL.Hostname() == "hooks.example.test" {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	c.successes <- req.URL.Hostname()
	return &http.Response{
		StatusCode: http.StatusNoContent,
		Body:       io.NopCloser(bytes.NewReader(nil)),
		Header:     make(http.Header),
	}, nil
}

func TestNativeIntegrationsConfigureFanOutAndProtectCredentials(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "native-integrations.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
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

	requests := make([]capturedNotificationRequest, 0, 2)
	for len(requests) < 2 {
		select {
		case request := <-client.requests:
			requests = append(requests, request)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for native integration delivery")
		}
	}
	if !strings.Contains(requests[0].URL, "discord.com/api/webhooks/") || !strings.Contains(requests[0].URL, "wait=true") {
		t.Fatalf("Discord request URL = %q", requests[0].URL)
	}
	var discordBody struct {
		Embeds          []json.RawMessage `json:"embeds"`
		AllowedMentions struct {
			Parse []string `json:"parse"`
		} `json:"allowed_mentions"`
	}
	if err := json.Unmarshal(requests[0].Body, &discordBody); err != nil {
		t.Fatalf("decode Discord body: %v", err)
	}
	if len(discordBody.Embeds) != 1 || discordBody.AllowedMentions.Parse == nil || len(discordBody.AllowedMentions.Parse) != 0 {
		t.Fatalf("Discord body = %s, want one embed and disabled mentions", requests[0].Body)
	}
	if !strings.Contains(requests[1].URL, "api.telegram.org/bot"+telegramToken+"/sendMessage") {
		t.Fatalf("Telegram request URL = %q", requests[1].URL)
	}
	var telegramBody telegramMessageBody
	if err := json.Unmarshal(requests[1].Body, &telegramBody); err != nil {
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

func TestNativeIntegrationFanOutUsesIndependentDestinationTimeouts(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "native-integration-timeouts.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	client := &firstDestinationTimeoutClient{successes: make(chan string, 2)}
	encrypt, decrypt := testWebhookCodec()
	svc := NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		HTTPClient:      client,
		EncryptSecret:   encrypt,
		DecryptSecret:   decrypt,
		Backoff:         func(int) time.Duration { return 0 },
		DeliveryTimeout: 10 * time.Millisecond,
		Logf:            func(string, ...any) {},
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = svc.Close(ctx)
	})

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

	if admission := svc.Accept(DeliveryIntent{
		CreatedAt:  "2026-07-30T12:00:00Z",
		Action:     EventUpdateComplete,
		Status:     "success",
		TargetType: "server",
		TargetName: "srv-timeout",
		Message:    "Update completed",
		MetaJSON:   `{}`,
	}); admission.State != AdmissionAdmitted {
		t.Fatalf("Accept() = %+v, want admitted", admission)
	}

	got := map[string]bool{}
	for len(got) < 2 {
		select {
		case host := <-client.successes:
			got[host] = true
		case <-time.After(time.Second):
			t.Fatalf("successful destinations = %v, want Discord and Telegram after webhook timeout", got)
		}
	}
	if !got["discord.com"] || !got["api.telegram.org"] {
		t.Fatalf("successful destinations = %v, want Discord and Telegram", got)
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
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "validation.db"))
			if err != nil {
				t.Fatalf("sql.Open() error = %v", err)
			}
			defer db.Close()
			if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
				t.Fatalf("create settings table: %v", err)
			}
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
