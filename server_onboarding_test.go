package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	serverpkg "debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestOnboardingHostKeyCallbackPinsIdentityAndPreservesExistingTrust(t *testing.T) {
	signer, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}
	other, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, line string
		accepted   bool
	}{
		{"new host", "", true},
		{"trusted", knownhosts.Line([]string{"192.0.2.24"}, signer.PublicKey()), true},
		{"changed", knownhosts.Line([]string{"192.0.2.24"}, other.PublicKey()), false},
		{"revoked", "@revoked " + knownhosts.Line([]string{"192.0.2.24"}, signer.PublicKey()), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "known_hosts")
			if err := os.WriteFile(path, []byte(tc.line+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			deps := serverpkg.KnownHostsDeps{Getenv: func(string) string { return path }, UserHomeDir: func() (string, error) { return t.TempDir(), nil }}
			callback, err := serverpkg.OnboardingHostKeyCallback(deps, ssh.FingerprintSHA256(signer.PublicKey()))
			if err != nil {
				t.Fatal(err)
			}
			remote := &net.TCPAddr{IP: net.ParseIP("192.0.2.24"), Port: 22}
			if accepted := callback("192.0.2.24:22", remote, signer.PublicKey()) == nil; accepted != tc.accepted {
				t.Fatalf("accepted = %v", accepted)
			}
			if !errors.Is(callback("192.0.2.24:22", remote, other.PublicKey()), serverpkg.ErrFingerprintMismatch) {
				t.Fatal("fingerprint mismatch accepted")
			}
		})
	}
}

func TestServerOnboardingAPIRequiresAuthAndPersistsOnlyAfterFreshChecks(t *testing.T) {
	app := newIsolatedTestApp(t)
	signer, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}
	originalScanner := scanHostKeyFunc
	scanHostKeyFunc = func(string, int) (ssh.PublicKey, error) { return signer.PublicKey(), nil }
	t.Cleanup(func() { scanHostKeyFunc = originalScanner })
	originalDial := getDialSSHConnection()
	dials := 0
	failPermissions := false
	setDialSSHConnection(func(server Server, config *ssh.ClientConfig) (sshConnection, error) {
		dials++
		if err := config.HostKeyCallback("192.0.2.24:22", &net.TCPAddr{IP: net.ParseIP("192.0.2.24"), Port: 22}, signer.PublicKey()); err != nil {
			return nil, err
		}
		connection := &scriptedSSHConnection{responses: map[string]scriptedResponse{
			serverpkg.OnboardingAPTCommand:     {},
			serverpkg.OnboardingSudoCommand:    {},
			serverpkg.OnboardingPackageCommand: {},
			serverpkg.OnboardingOSCommand:      {stdout: "ID=ubuntu\nVERSION_ID=24.04\n"},
			serverpkg.OnboardingDiskCommand:    {stdout: "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/root 9999999 1000 5000000 1% /\n/dev/root 9999999 1000 5000000 1% /\n/dev/root 9999999 1000 5000000 1% /\n"},
		}}
		if failPermissions {
			connection.responses[serverpkg.OnboardingSudoCommand] = scriptedResponse{err: errors.New("denied")}
		}
		return connection, nil
	})
	t.Cleanup(func() { setDialSSHConnection(originalDial) })
	draft := serverpkg.OnboardingDraft{Name: "checked-host", Host: "192.0.2.24", Port: 22, User: "root", AuthMethod: "per-server-key", Key: testPrivateKeyPEM(t), Fingerprint: ssh.FingerprintSHA256(signer.PublicKey()), Confirmed: true}
	body, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	post := func(path string, cookie *http.Cookie, data []byte) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(data))
		req.Header.Set("Content-Type", "application/json")
		markSameOriginAuthRequest(req)
		if cookie != nil {
			req.AddCookie(cookie)
		}
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	if rec := post("/api/servers/onboarding/check", nil, body); rec.Code != http.StatusUnauthorized || dials != 0 {
		t.Fatalf("unauthorized check: %d", rec.Code)
	}
	cookie := app.authenticate(t)
	rec := post("/api/servers/onboarding/check", cookie, body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ready":true`) {
		t.Fatalf("check: %d %s", rec.Code, rec.Body.String())
	}
	if len(app.Deps.ServerState.CloneServers()) != 0 {
		t.Fatal("checking persisted a server")
	}
	known, _ := os.ReadFile(app.KnownHostsPath)
	if len(known) != 0 {
		t.Fatal("checking persisted host trust")
	}
	failPermissions = true
	if rec := post("/api/servers/onboarding", cookie, body); rec.Code != http.StatusUnprocessableEntity || len(app.Deps.ServerState.CloneServers()) != 0 {
		t.Fatal("stale check bypassed verification")
	}
	failPermissions = false
	rec = post("/api/servers/onboarding", cookie, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "PRIVATE KEY") {
		t.Fatal("key leaked in response")
	}
	var encryptedKey, encryptedPass string
	if err := app.Deps.DB().QueryRow("SELECT key_enc, pass_enc FROM servers WHERE name = ?", draft.Name).Scan(&encryptedKey, &encryptedPass); err != nil {
		t.Fatal(err)
	}
	key, err := decryptSecret(encryptedKey)
	if err != nil || key != draft.Key || encryptedKey == draft.Key {
		t.Fatal("key was not encrypted and persisted correctly")
	}
	if dials != 3 {
		t.Fatalf("expected fresh connections, got %d", dials)
	}
	var jobs int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobs); err != nil || jobs != 0 {
		t.Fatalf("onboarding launched maintenance: jobs=%d, err=%v", jobs, err)
	}
	if rec := post("/api/servers/onboarding/check", cookie, []byte(strings.Repeat("x", 128*1024+1))); rec.Code != http.StatusBadRequest {
		t.Fatal("oversized request accepted")
	}
}
