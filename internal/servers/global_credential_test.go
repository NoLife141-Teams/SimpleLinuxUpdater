package servers

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

func newGlobalCredentialTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "global-credential.db"))
	if err != nil {
		t.Fatalf("open global credential DB: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	return db
}

func testGlobalCredentialCodec() (func(string) (string, error), func(string) (string, error)) {
	return func(value string) (string, error) {
			return "encrypted:" + value, nil
		}, func(value string) (string, error) {
			const prefix = "encrypted:"
			if len(value) < len(prefix) || value[:len(prefix)] != prefix {
				return "", errors.New("invalid ciphertext")
			}
			return value[len(prefix):], nil
		}
}

func allowAnyGlobalCredential(string) error { return nil }

func validGlobalCredential(t *testing.T) string {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate private key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(privateKey, "global credential test")
	if err != nil {
		t.Fatalf("marshal private key: %v", err)
	}
	return string(pem.EncodeToMemory(block))
}

func TestGlobalSSHCredentialLiveLifecycle(t *testing.T) {
	db := newGlobalCredentialTestDB(t)
	encrypt, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
		Store:    SQLiteGlobalSSHCredentialStore{DB: func() *sql.DB { return db }},
		Encrypt:  encrypt,
		Decrypt:  decrypt,
		Validate: allowAnyGlobalCredential,
	})

	status, err := credential.Status(context.Background())
	if err != nil {
		t.Fatalf("Status() absent error = %v", err)
	}
	if status.Configured {
		t.Fatal("Status() absent Configured = true, want false")
	}

	replaced := credential.Replace(context.Background(), "global-private-key")
	if !replaced.Succeeded() {
		t.Fatalf("Replace() = %+v, want success", replaced)
	}
	status, err = credential.Status(context.Background())
	if err != nil || !status.Configured {
		t.Fatalf("Status() after replace = %+v, %v, want configured", status, err)
	}

	global, err := credential.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve() global error = %v", err)
	}
	if global.Key != "global-private-key" || global.Source != GlobalSSHCredentialSourceGlobal || global.Degraded {
		t.Fatalf("Resolve() global = %+v", global)
	}

	perServer, err := credential.Resolve(context.Background(), "server-private-key")
	if err != nil {
		t.Fatalf("Resolve() per-server error = %v", err)
	}
	if perServer.Key != "server-private-key" || perServer.Source != GlobalSSHCredentialSourceServer {
		t.Fatalf("Resolve() per-server = %+v", perServer)
	}

	cleared := credential.Clear(context.Background())
	if !cleared.Succeeded() {
		t.Fatalf("Clear() = %+v, want success", cleared)
	}
	if second := credential.Clear(context.Background()); !second.Succeeded() {
		t.Fatalf("second Clear() = %+v, want idempotent success", second)
	}
	absent, err := credential.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve() after clear error = %v", err)
	}
	if absent.Key != "" || absent.Source != GlobalSSHCredentialSourceNone {
		t.Fatalf("Resolve() after clear = %+v", absent)
	}
}

func TestGlobalSSHCredentialBlocksMutationDuringServerActions(t *testing.T) {
	db := newGlobalCredentialTestDB(t)
	encrypt, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
		Store:               SQLiteGlobalSSHCredentialStore{DB: func() *sql.DB { return db }},
		Encrypt:             encrypt,
		Decrypt:             decrypt,
		ActiveServerActions: func() []string { return []string{"alpha", "beta"} },
		Validate:            allowAnyGlobalCredential,
	})

	result := credential.Replace(context.Background(), "private-key")
	if result.Outcome != CommandOutcomeConflict || !reflect.DeepEqual(result.ActiveServers, []string{"alpha", "beta"}) {
		t.Fatalf("Replace() active actions = %+v", result)
	}
	status, err := credential.Status(context.Background())
	if err != nil || status.Configured {
		t.Fatalf("Status() after blocked replace = %+v, %v", status, err)
	}
}

type controllableGlobalCredentialStore struct {
	mu      sync.Mutex
	value   string
	readErr error
}

func (s *controllableGlobalCredentialStore) Read(context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.value, s.readErr
}

func (s *controllableGlobalCredentialStore) Write(_ context.Context, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = value
	return nil
}

func (s *controllableGlobalCredentialStore) Delete(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.value = ""
	return nil
}

func (s *controllableGlobalCredentialStore) setReadError(err error) {
	s.mu.Lock()
	s.readErr = err
	s.mu.Unlock()
}

func TestGlobalSSHCredentialUsesLastKnownGoodValueOnReadFailure(t *testing.T) {
	store := &controllableGlobalCredentialStore{}
	encrypt, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
		Store:        store,
		Encrypt:      encrypt,
		Decrypt:      decrypt,
		RetryDelay:   time.Nanosecond,
		Sleep:        func(time.Duration) {},
		Logf:         func(string, ...any) {},
		ReadAttempts: 1,
		Validate:     allowAnyGlobalCredential,
	})
	if result := credential.Replace(context.Background(), "last-known-good"); !result.Succeeded() {
		t.Fatalf("Replace() = %+v", result)
	}
	store.setReadError(errors.New("database is locked"))

	resolved, err := credential.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve() cached error = %v", err)
	}
	if resolved.Key != "last-known-good" || !resolved.Degraded {
		t.Fatalf("Resolve() cached = %+v", resolved)
	}
	if _, err := credential.Status(context.Background()); err == nil {
		t.Fatal("Status() read failure error = nil, want persistence error")
	}
}

func TestGlobalSSHCredentialUsesKnownAbsenceOnReadFailure(t *testing.T) {
	store := &controllableGlobalCredentialStore{}
	_, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
		Store:        store,
		Decrypt:      decrypt,
		RetryDelay:   time.Nanosecond,
		Sleep:        func(time.Duration) {},
		Logf:         func(string, ...any) {},
		ReadAttempts: 1,
	})
	if resolved, err := credential.Resolve(context.Background(), ""); err != nil || resolved.Source != GlobalSSHCredentialSourceNone {
		t.Fatalf("initial Resolve() = %+v, %v, want known absence", resolved, err)
	}
	store.setReadError(errors.New("database is locked"))

	resolved, err := credential.Resolve(context.Background(), "")
	if err != nil {
		t.Fatalf("Resolve() cached absence error = %v", err)
	}
	if resolved.Key != "" || resolved.Source != GlobalSSHCredentialSourceNone || !resolved.Degraded {
		t.Fatalf("Resolve() cached absence = %+v", resolved)
	}
	if _, err := BuildAuthMethods(Server{Pass: "password"}); err != nil {
		t.Fatalf("BuildAuthMethods() password fallback error = %v", err)
	}
}

func TestGlobalSSHCredentialConcurrentResolutionAndReplacement(t *testing.T) {
	store := &controllableGlobalCredentialStore{}
	encrypt, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{Store: store, Encrypt: encrypt, Decrypt: decrypt, Validate: allowAnyGlobalCredential})
	if result := credential.Replace(context.Background(), "initial"); !result.Succeeded() {
		t.Fatalf("Replace(initial) = %+v", result)
	}

	errCh := make(chan error, 32)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			resolved, err := credential.Resolve(context.Background(), "")
			if err != nil || resolved.Key == "" {
				errCh <- fmt.Errorf("resolve = %+v, %w", resolved, err)
			}
		}()
		go func(index int) {
			defer wg.Done()
			if result := credential.Replace(context.Background(), fmt.Sprintf("key-%d", index)); !result.Succeeded() {
				errCh <- fmt.Errorf("replace = %+v", result)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}
}

func TestGlobalSSHCredentialRejectsUnusableReplacementBeforePersistence(t *testing.T) {
	tests := []struct {
		name string
		key  func(*testing.T) string
	}{
		{name: "malformed", key: func(*testing.T) string { return "not a private key" }},
		{name: "public only", key: func(t *testing.T) string {
			privateKey := validGlobalCredential(t)
			signer, err := ssh.ParsePrivateKey([]byte(privateKey))
			if err != nil {
				t.Fatalf("parse generated private key: %v", err)
			}
			return string(ssh.MarshalAuthorizedKey(signer.PublicKey()))
		}},
		{name: "passphrase protected", key: func(t *testing.T) string {
			_, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatalf("generate private key: %v", err)
			}
			block, err := ssh.MarshalPrivateKeyWithPassphrase(privateKey, "test", []byte("secret"))
			if err != nil {
				t.Fatalf("marshal protected private key: %v", err)
			}
			return string(pem.EncodeToMemory(block))
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newGlobalCredentialTestDB(t)
			encrypt, decrypt := testGlobalCredentialCodec()
			credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
				Store:   SQLiteGlobalSSHCredentialStore{DB: func() *sql.DB { return db }},
				Encrypt: encrypt,
				Decrypt: decrypt,
			})

			result := credential.Replace(context.Background(), tt.key(t))
			if result.Outcome != CommandOutcomeInvalid {
				t.Fatalf("Replace() = %+v, want invalid", result)
			}
			status, err := credential.Status(context.Background())
			if err != nil || status.Configured {
				t.Fatalf("Status() after rejection = %+v, %v, want absent", status, err)
			}
		})
	}
}

func TestGlobalSSHCredentialAcceptsUsablePrivateKey(t *testing.T) {
	db := newGlobalCredentialTestDB(t)
	encrypt, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{
		Store:   SQLiteGlobalSSHCredentialStore{DB: func() *sql.DB { return db }},
		Encrypt: encrypt,
		Decrypt: decrypt,
	})

	if result := credential.Replace(context.Background(), validGlobalCredential(t)); !result.Succeeded() {
		t.Fatalf("Replace() = %+v, want success", result)
	}
}

func TestGlobalSSHCredentialPerServerKeyWinsOverHistoricalInvalidGlobalData(t *testing.T) {
	store := &controllableGlobalCredentialStore{value: "encrypted:not a private key"}
	_, decrypt := testGlobalCredentialCodec()
	credential := NewGlobalSSHCredential(GlobalSSHCredentialDeps{Store: store, Decrypt: decrypt})

	resolved, err := credential.Resolve(context.Background(), validGlobalCredential(t))
	if err != nil {
		t.Fatalf("Resolve() per-server error = %v", err)
	}
	if resolved.Source != GlobalSSHCredentialSourceServer {
		t.Fatalf("Resolve() source = %q, want server", resolved.Source)
	}
	if _, err := BuildAuthMethods(Server{Key: resolved.Key}); err != nil {
		t.Fatalf("BuildAuthMethods() valid per-server key error = %v", err)
	}
}
