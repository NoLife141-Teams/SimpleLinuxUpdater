package notifications

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func webhookSecurityCodec() (func(string) (string, error), func(string) (string, error)) {
	return func(value string) (string, error) {
		return base64.StdEncoding.EncodeToString([]byte(value)), nil
	}, func(value string) (string, error) {
		plain, err := base64.StdEncoding.DecodeString(value)
		return string(plain), err
	}
}

func openWebhookSecurityDB(t *testing.T, name string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		_ = db.Close()
		t.Fatalf("create settings table: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func closeWebhookSecurityService(t *testing.T, svc *Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := svc.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestNotificationHTTPClientRejects307And308Redirects(t *testing.T) {
	for _, statusCode := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(statusCode), func(t *testing.T) {
			var redirectedHits atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				redirectedHits.Add(1)
				w.WriteHeader(http.StatusNoContent)
			}))
			defer target.Close()

			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, target.URL+"/internal", statusCode)
			}))
			defer origin.Close()

			svc := NewService(ServiceDeps{
				HTTPClient: origin.Client(),
				Backoff:    func(int) time.Duration { return 0 },
				Logf:       func(string, ...any) {},
			})
			defer closeWebhookSecurityService(t, svc)

			status, err := svc.deliverToDestination(context.Background(), Settings{
				Enabled:    true,
				WebhookURL: origin.URL + "/hook",
			}, DeliveryIntent{
				CreatedAt:  "2026-09-04T22:00:00Z",
				Actor:      "admin",
				Action:     EventTest,
				TargetType: "notification",
				TargetName: DestinationWebhook,
				Status:     "test",
				Message:    "redirect boundary test",
			}, EventTest, DestinationWebhook)
			if err == nil {
				t.Fatal("deliverToDestination() error = nil, want redirect rejection")
			}
			if redirectedHits.Load() != 0 {
				t.Fatalf("redirect target received %d request(s), want 0", redirectedHits.Load())
			}
			if status.StatusCode != statusCode {
				t.Fatalf("StatusCode = %d, want %d", status.StatusCode, statusCode)
			}
			if status.Attempts != defaultAttempts {
				t.Fatalf("Attempts = %d, want %d", status.Attempts, defaultAttempts)
			}
		})
	}
}

func TestStoredWebhookValidationUsesCurrentDestinationPolicy(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{name: "public https", url: "https://hooks.example.test/notify"},
		{name: "loopback http", url: "http://127.0.0.1:9000/notify"},
		{name: "internal http", url: "http://hooks.example.internal/notify"},
		{name: "public http", url: "http://hooks.example.test/notify", wantErr: true},
		{name: "embedded credentials", url: "https://user:secret@hooks.example.test/notify", wantErr: true},
		{name: "unsupported scheme", url: "ftp://hooks.example.test/notify", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateStoredWebhookURL(tc.url)
			if (err != nil) != tc.wantErr {
				t.Fatalf("validateStoredWebhookURL(%q) error = %v, wantErr %t", tc.url, err, tc.wantErr)
			}
		})
	}
}

func TestLegacyUnsafeWebhookIsEncryptedAndDisabledOnLoad(t *testing.T) {
	db := openWebhookSecurityDB(t, "legacy-unsafe.db")
	const unsafeURL = "http://hooks.example.test/legacy"
	stored := persistedSettings{
		Enabled:          true,
		LegacyWebhookURL: unsafeURL,
		EventTypes:       []string{EventUpdateComplete},
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO settings(key, value) VALUES(?, ?)", SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}
	encrypt, decrypt := webhookSecurityCodec()
	svc := NewService(ServiceDeps{
		DB:            func() *sql.DB { return db },
		EncryptSecret: encrypt,
		DecryptSecret: decrypt,
		Logf:          func(string, ...any) {},
	})
	defer closeWebhookSecurityService(t, svc)

	settings, err := svc.Settings()
	if err != nil {
		t.Fatalf("Settings() error = %v", err)
	}
	if settings.Enabled {
		t.Fatal("Settings().Enabled = true, want unsafe legacy webhook disabled")
	}
	if !settings.WebhookConfigured {
		t.Fatal("Settings().WebhookConfigured = false, want preserved masked configuration")
	}

	var persistedRaw string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatal(err)
	}
	var persisted persistedSettings
	if err := json.Unmarshal([]byte(persistedRaw), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Enabled {
		t.Fatal("persisted unsafe legacy webhook remained enabled")
	}
	if persisted.LegacyWebhookURL != "" {
		t.Fatalf("persisted legacy plaintext URL = %q, want empty", persisted.LegacyWebhookURL)
	}
	if persisted.EncryptedWebhookURL == "" {
		t.Fatal("persisted encrypted webhook URL is empty")
	}
	decoded, err := decrypt(persisted.EncryptedWebhookURL)
	if err != nil || decoded != unsafeURL {
		t.Fatalf("decrypted webhook = %q, %v, want %q", decoded, err, unsafeURL)
	}

	if _, err := svc.SaveSettings(SettingsUpdate{
		Enabled:          true,
		WebhookURLIntent: WebhookURLPreserve,
		EventTypes:       []string{EventUpdateComplete},
	}); err == nil {
		t.Fatal("SaveSettings() re-enabled preserved unsafe webhook, want validation error")
	}
}

func TestRestoreRewrapDisablesUnsafeWebhook(t *testing.T) {
	db := openWebhookSecurityDB(t, "restore-unsafe.db")
	const unsafeURL = "https://user:secret@hooks.example.test/restored"
	encrypt, decrypt := webhookSecurityCodec()
	protected, err := encrypt(unsafeURL)
	if err != nil {
		t.Fatal(err)
	}
	stored := persistedSettings{
		Enabled:             true,
		EncryptedWebhookURL: protected,
		EventTypes:          []string{EventBackupRestore},
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO settings(key, value) VALUES(?, ?)", SettingsKey, string(raw)); err != nil {
		t.Fatal(err)
	}

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := ReencryptStoredWebhookURL(context.Background(), tx, decrypt, encrypt); err != nil {
		_ = tx.Rollback()
		t.Fatalf("ReencryptStoredWebhookURL() error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var persistedRaw string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&persistedRaw); err != nil {
		t.Fatal(err)
	}
	var persisted persistedSettings
	if err := json.Unmarshal([]byte(persistedRaw), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Enabled {
		t.Fatal("restored unsafe webhook remained enabled")
	}
	if persisted.LegacyWebhookURL != "" || persisted.EncryptedWebhookURL == "" {
		t.Fatalf("restored webhook persistence = %+v, want encrypted-only URL", persisted)
	}
	decoded, err := decrypt(persisted.EncryptedWebhookURL)
	if err != nil || decoded != unsafeURL {
		t.Fatalf("decrypted restored webhook = %q, %v, want %q", decoded, err, unsafeURL)
	}
}
