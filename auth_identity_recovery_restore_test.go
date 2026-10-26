package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/json"
	"errors"
	"github.com/alexedwards/scs/v2"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	authpkg "debian-updater/internal/auth"
	"debian-updater/internal/installationlock"
	observabilitypkg "debian-updater/internal/observability"
)

func TestIdentityLocalRecoveryCLITargetsSelectedAccountAndFactors(t *testing.T) {
	for _, factors := range []string{"password", "mfa", "password,mfa"} {
		t.Run(factors, func(t *testing.T) {
			preserveEncryptionState(t)
			path := prepareRecoveryCommandDB(t)
			t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
			db, err := openRecoveryDatabase(path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			service := NewAuthService(func() *sql.DB { return db })
			target, err := service.CreateAccount("target", "TargetOriginal123!", "operator")
			if err != nil {
				t.Fatal(err)
			}
			enroll, err := service.BeginMFA(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enroll.Secret)
			if _, err = service.ConfirmMFA(target.ID, authpkg.HOTP(raw, uint64(time.Now().Unix()/30), 6)); err != nil {
				t.Fatal(err)
			}
			before, _ := service.Account(target.ID)
			administrator, _ := service.Account(1)
			sm, err := newSessionManager(db)
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"admin", "target"} {
				ctx, err := sm.Load(context.Background(), "")
				if err != nil {
					t.Fatal(err)
				}
				if err = (scsAuthSessionLifecycle{current: func() *scs.SessionManager { return sm }}).Stage(ctx, name); err != nil {
					t.Fatal(err)
				}
				if _, _, err = sm.Commit(ctx); err != nil {
					t.Fatal(err)
				}
			}
			_, targetSecret, err := service.CreatePersonalToken(target.ID, "target", []string{"operational:read"}, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			_, adminSecret, err := service.CreatePersonalToken(1, "admin", []string{"operational:read"}, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"admin", "reset-password", "--username", "target", "--password-stdin", "--factors", factors}
			input := "NewTargetPassword123!\nNewTargetPassword123!\n"
			if factors == "mfa" {
				input = ""
			}
			lease, err := installationlock.Acquire(path, false)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = commandRecovery(t, args, input); !errors.Is(err, installationlock.ErrBusy) {
				t.Fatal("CLI bypassed installation lock", err)
			}
			lease.Close()
			output, err := commandRecovery(t, args, input)
			if err != nil || !strings.Contains(output, "recovery committed") {
				t.Fatal("CLI recovery", output, err)
			}
			after, err := service.Account(target.ID)
			if err != nil {
				t.Fatal(err)
			}
			unchanged, _ := service.Account(1)
			if after.Role != before.Role || after.Disabled != before.Disabled || unchanged.PasswordHash != administrator.PasswordHash || unchanged.Generation != administrator.Generation {
				t.Fatal("recovery changed account roles/state or another account")
			}
			if strings.Contains(factors, "password") {
				if ok, err := service.Authenticate("target", "NewTargetPassword123!"); err != nil || !ok {
					t.Fatal("selected password not reset", err)
				}
			} else if after.PasswordHash != before.PasswordHash {
				t.Fatal("MFA-only recovery changed password")
			}
			if after.MFAEnabled != !strings.Contains(factors, "mfa") {
				t.Fatal("selected factor reset incorrect")
			}
			if n, err := service.ForAccount(1).CountSessions(); err != nil || n != 1 {
				t.Fatal("another account sessions removed", n, err)
			}
			if n, err := service.ForAccount(target.ID).CountSessions(); err != nil || n != 0 {
				t.Fatal("target sessions retained", n, err)
			}
			if _, _, err = service.VerifyPersonalToken(targetSecret, ""); !errors.Is(err, authpkg.ErrTokenInvalid) {
				t.Fatal("recovery retained target token", err)
			}
			if _, _, err = service.VerifyPersonalToken(adminSecret, ""); err != nil {
				t.Fatal("recovery revoked another account token", err)
			}
			var meta string
			if err = db.QueryRow("SELECT meta_json FROM audit_events WHERE action='auth.password.recover' ORDER BY id DESC LIMIT 1").Scan(&meta); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(meta, `"account_id":`+formatAccountID(target.ID)) || !strings.Contains(meta, `"recovery_method":"local_cli"`) || strings.Contains(meta, targetSecret) || strings.Contains(meta, "NewTargetPassword123!") {
				t.Fatal("recovery audit lost identity or leaked credentials", meta)
			}
		})
	}
}

func TestIdentityRealBackupArchiveRestoreInvalidatesArchivedCookiesPATAndRecoveryPreservesMetrics(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "restore-identities.db"))
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", filepath.Join(t.TempDir(), "known_hosts"))
	if err := os.WriteFile(os.Getenv("DEBIAN_UPDATER_KNOWN_HOSTS"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	deps := appDepsWithDefaultsForTest(t, AppDeps{})
	router, err := setupRouterWithDeps(deps)
	if err != nil {
		t.Fatal(err)
	}
	app := &testApp{Router: router, Handler: sessionHandler(router), Deps: deps}
	cookie := app.authenticate(t)
	if rec := identityRequest(app, "POST", "/api/servers", cookie, "", map[string]any{"name": "archived", "host": "synthetic.invalid", "user": "root", "pass": "SyntheticSSH123!"}); rec.Code != 201 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	// Include a confirmed encrypted factor and recovery hashes in the archive.
	enrollment, err := deps.AuthService.BeginMFA(1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	codes, err := deps.AuthService.ConfirmMFA(1, authpkg.HOTP(raw, uint64(time.Now().Unix()/30), 6))
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("POST", "/api/auth/login", strings.NewReader(`{"username":"admin","password":"`+testPasswordStrong+`"}`))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	pending := httptest.NewRecorder()
	app.Handler.ServeHTTP(pending, req)
	if pending.Code != 202 {
		t.Fatal(pending.Code, pending.Body.String())
	}
	body, _ := json.Marshal(map[string]any{"challenge": decodeContractJSON(t, pending)["challenge"], "code": codes[0]})
	req = httptest.NewRequest("POST", "/api/auth/mfa/challenge", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	complete := httptest.NewRecorder()
	app.Handler.ServeHTTP(complete, req)
	if complete.Code != 200 {
		t.Fatal(complete.Code, complete.Body.String())
	}
	cookie = testSessionCookieFromRecorder(t, complete)
	token, personalSecret, err := deps.AuthService.CreatePersonalToken(1, "archived", []string{"operational:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	archivedMetrics, err := deps.MetricsAccessCredential.Rotate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	exported := identityRequest(app, "POST", "/api/backup/export", cookie, "", map[string]any{"passphrase": "SyntheticArchivePassphrase123!", "include_known_hosts": false})
	if exported.Code != 200 {
		t.Fatal(exported.Code, exported.Body.String())
	}
	archive := append([]byte(nil), exported.Body.Bytes()...)
	// Revoke after export: restoring this older archive must not resurrect either credential.
	if _, err = deps.AuthService.RevokePersonalToken(1, token.ID); err != nil {
		t.Fatal(err)
	}
	staleMetrics, err := deps.MetricsAccessCredential.Rotate(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "identities.slubkp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = part.Write(archive); err != nil {
		t.Fatal(err)
	}
	if err = writer.WriteField("passphrase", "SyntheticArchivePassphrase123!"); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest("POST", "/api/backup/restore", &upload)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.AddCookie(cookie)
	markSameOriginAuthRequest(req)
	restored := httptest.NewRecorder()
	app.Handler.ServeHTTP(restored, req)
	if restored.Code != 200 {
		t.Fatal(restored.Code, restored.Body.String())
	}
	if rec := identityRequest(app, "GET", "/api/servers", cookie, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatal("archived cookie resurrected", rec.Code, rec.Body.String())
	}
	if rec := identityRequest(app, "GET", "/api/servers", nil, personalSecret, nil); rec.Code != http.StatusUnauthorized {
		t.Fatal("archived token resurrected", rec.Code, rec.Body.String())
	}
	for _, table := range []string{"sessions", "auth_personal_tokens", "auth_challenges", "auth_recovery_codes"} {
		var n int
		if err = deps.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatal("restored ephemeral authentication retained", table, n, err)
		}
	}
	factor, err := deps.AuthService.Account(1)
	if err != nil || !factor.MFAEnabled {
		t.Fatal("restored factor lost", err)
	}
	var encrypted string
	if err = deps.DB().QueryRow("SELECT totp_secret_enc FROM auth_users WHERE id=1").Scan(&encrypted); err != nil {
		t.Fatal(err)
	}
	plain, err := decryptSecret(encrypted)
	if err != nil || plain != enrollment.Secret {
		t.Fatal("factor rewrap failed", err)
	}
	if result, err := deps.MetricsAccessCredential.Verify(context.Background(), archivedMetrics); err != nil || result != observabilitypkg.MetricsAccessAccepted {
		t.Fatal("archived metrics contract changed", result, err)
	}
	if result, err := deps.MetricsAccessCredential.Verify(context.Background(), staleMetrics); err != nil || result != observabilitypkg.MetricsAccessRejected {
		t.Fatal("stale metrics resurrected", result, err)
	}
	if ok, err := deps.AuthService.Authenticate("admin", testPasswordStrong); err != nil || !ok {
		t.Fatal("restored password lost", err)
	}
	var inventory int
	if err = deps.DB().QueryRow("SELECT count(*) FROM servers WHERE name='archived'").Scan(&inventory); err != nil || inventory != 1 {
		t.Fatal("inventory lost", err)
	}
}
