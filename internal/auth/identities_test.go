package auth

import (
	"context"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alexedwards/argon2id"
)

func identityTestService(t *testing.T) (*Service, *sql.DB, *time.Time) {
	t.Helper()
	db := newTestDB(t)
	// Runtime Composition uses one SQLite connection plus a busy timeout.
	// Match that admission contract while issuing genuinely concurrent commands.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2000000000, 0).UTC()
	s := NewService(ServiceOptions{DB: func() *sql.DB { return db }, Now: func() time.Time { return now }, Encrypt: func(v string) (string, error) {
		return "test-envelope:" + base64.StdEncoding.EncodeToString([]byte(v)), nil
	}, Decrypt: func(v string) (string, error) {
		b, e := base64.StdEncoding.DecodeString(strings.TrimPrefix(v, "test-envelope:"))
		return string(b), e
	}})
	if err := s.CreateInitialUser("admin", "IdentityTest123!"); err != nil {
		t.Fatal(err)
	}
	return s, db, &now
}
func TestIdentityMigrationPreservesInitialAccountAndOnlyPurgesOnce(t *testing.T) {
	db := openAuthSchemaTestDB(t, "migration.db")
	hash, err := argon2id.CreateHash("MigrationTest123!", argon2id.DefaultParams)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`CREATE TABLE auth_users(id INTEGER PRIMARY KEY CHECK(id=1),username TEXT NOT NULL,password_hash TEXT NOT NULL,created_at TEXT NOT NULL,updated_at TEXT NOT NULL);
 CREATE TABLE sessions(token TEXT PRIMARY KEY,data BLOB NOT NULL,expiry REAL NOT NULL);
 CREATE TABLE business(secret TEXT NOT NULL);INSERT INTO business VALUES('synthetic-encrypted-inventory');
 INSERT INTO sessions VALUES('old-session',x'00',9999999999);`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO auth_users VALUES(1,'original',?,'created','updated')", hash); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	s := NewService(ServiceOptions{DB: func() *sql.DB { return db }})
	a, err := s.Account(1)
	if err != nil || a.Username != "original" || a.PasswordHash != hash || a.Role != "admin" || a.Disabled {
		t.Fatalf("migration changed identity: %+v %v", a, err)
	}
	if required, err := s.SetupRequired(); required || err != nil {
		t.Fatalf("setup reopened: %v %v", required, err)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM sessions").Scan(&n); err != nil || n != 0 {
		t.Fatal("legacy session retained", n, err)
	}
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = PrepareAuthentication(ctx, sm)
	if err != nil {
		t.Fatal(err)
	}
	sm.Put(ctx, SessionUserKey, a.Username)
	if err = StageAuthentication(ctx, sm); err != nil {
		t.Fatal(err)
	}
	if err = StageIdentity(ctx, sm, a.Username); err != nil {
		t.Fatal(err)
	}
	token, _, err := sm.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	next, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := next.Load(context.Background(), token)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := SessionAccount(loaded, next)
	if err != nil || restored.ID != 1 {
		t.Fatalf("new session did not survive migration restart: %+v %v", restored, err)
	}
	var inventory string
	if err = db.QueryRow("SELECT secret FROM business").Scan(&inventory); err != nil || inventory != "synthetic-encrypted-inventory" {
		t.Fatal("inventory changed")
	}
	if ok, err := s.Authenticate("original", "MigrationTest123!"); !ok || err != nil {
		t.Fatal("original password lost", err)
	}
}
func TestLastAdministratorAtomicConcurrentChanges(t *testing.T) {
	s, db, _ := identityTestService(t)
	other, err := s.CreateAccount("other", "IdentityTest123!", "admin")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, id := range []int64{1, other.ID} {
		wg.Add(1)
		go func(id int64) { defer wg.Done(); results <- s.UpdateAccount(id, "reader", false) }(id)
	}
	wg.Wait()
	close(results)
	rejected := 0
	for err := range results {
		if err != nil {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("rejected %d concurrent demotions, want one", rejected)
	}
	var n int
	if err = db.QueryRow("SELECT count(*) FROM auth_users WHERE role='admin' AND disabled=0").Scan(&n); err != nil || n != 1 {
		t.Fatal("last administrator invariant lost", n, err)
	}
	if _, err = db.Exec("DELETE FROM auth_users WHERE role='admin'"); err == nil {
		t.Fatal("last administrator deletion accepted")
	}
	var id int64
	if err = db.QueryRow("SELECT id FROM auth_users WHERE role='admin'").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err = s.UpdateAccount(id, "admin", true); err == nil {
		t.Fatal("last administrator disabled")
	}
}
func TestRFC4226And6238Vectors(t *testing.T) {
	key := []byte("12345678901234567890")
	for i, want := range []string{"755224", "287082", "359152", "969429", "338314", "254676", "287922", "162583", "399871", "520489"} {
		if got := HOTP(key, uint64(i), 6); got != want {
			t.Fatalf("RFC4226 counter %d: %s != %s", i, got, want)
		}
	}
	for _, tt := range []struct {
		at   int64
		want string
	}{{59, "94287082"}, {1111111109, "07081804"}, {1111111111, "14050471"}, {1234567890, "89005924"}, {2000000000, "69279037"}, {20000000000, "65353130"}} {
		if got := HOTP(key, uint64(tt.at/30), 8); got != tt.want {
			t.Fatalf("RFC6238 time %d: %s != %s", tt.at, got, tt.want)
		}
	}
	secret := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key)
	if got := totpStep(secret, "287082", time.Unix(59, 0), -1); got != 1 {
		t.Fatal("six-digit TOTP", got)
	}
	if got := totpStep(secret, "287082", time.Unix(59, 0), 1); got != -1 {
		t.Fatal("replayed step accepted")
	}
	if got := totpStep(secret, "nototp", time.Unix(59, 0), -1); got != -1 {
		t.Fatal("invalid code accepted")
	}
}
func enableTestMFA(t *testing.T, s *Service, now *time.Time) (Enrollment, []string) {
	t.Helper()
	enrollment, err := s.BeginMFA(1)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	codes, err := s.ConfirmMFA(1, HOTP(raw, uint64(now.Unix()/30), 6))
	if err != nil {
		t.Fatal(err)
	}
	return enrollment, codes
}
func TestMFAEnrollmentEncryptionConfirmationReplayRecoveryAndChallenge(t *testing.T) {
	s, db, now := identityTestService(t)
	enrollment, err := s.BeginMFA(1)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := s.Account(1)
	if a.MFAEnabled {
		t.Fatal("unconfirmed enrollment activated MFA")
	}
	if _, err = s.ConfirmMFA(1, "invalid"); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("invalid enrollment accepted", err)
	}
	var encrypted string
	if err = db.QueryRow("SELECT totp_pending_enc FROM auth_users WHERE id=1").Scan(&encrypted); err != nil || strings.Contains(encrypted, enrollment.Secret) {
		t.Fatal("enrollment secret stored plaintext", err)
	}
	raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(enrollment.Secret)
	code := HOTP(raw, uint64(now.Unix()/30), 6)
	codes, err := s.ConfirmMFA(1, code)
	if err != nil || len(codes) != 10 {
		t.Fatal("confirmation failed", err)
	}
	if err = s.ConsumeFactor(1, code); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("enrollment OTP replayed", err)
	}
	if err = s.ConsumeFactor(1, codes[0]); err != nil {
		t.Fatal(err)
	}
	if err = s.ConsumeFactor(1, codes[0]); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("recovery replayed", err)
	}
	for _, code := range codes {
		var n int
		if err = db.QueryRow("SELECT count(*) FROM auth_recovery_codes WHERE code_hash=?", code).Scan(&n); err != nil || n != 0 {
			t.Fatal("plaintext recovery code stored")
		}
	}
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := PrepareAuthentication(context.Background(), sm)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := s.NewLoginChallenge(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CompleteLoginChallenge(challenge, code); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("OTP replay completed challenge", err)
	}
	if _, err = s.CompleteLoginChallenge(challenge, codes[1]); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("rejected challenge reused", err)
	}
	challenge, err = s.NewLoginChallenge(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	a, err = s.CompleteLoginChallenge(challenge, codes[1])
	if err != nil || a.ID != 1 {
		t.Fatal("recovery login failed", err)
	}
	if _, err = s.CompleteLoginChallenge(challenge, codes[2]); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("challenge reused", err)
	}
	challenge, err = s.NewLoginChallenge(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	*now = now.Add(6 * time.Minute)
	if _, err = s.CompleteLoginChallenge(challenge, codes[2]); !errors.Is(err, ErrFactorInvalid) {
		t.Fatal("expired challenge accepted", err)
	}
}
func TestMFAConcurrentRecoveryAndTOTPAreSingleUse(t *testing.T) {
	for _, kind := range []string{"totp", "recovery"} {
		t.Run(kind, func(t *testing.T) {
			s, _, now := identityTestService(t)
			e, codes := enableTestMFA(t, s, now)
			*now = now.Add(time.Minute)
			raw, _ := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(e.Secret)
			code := codes[0]
			if kind == "totp" {
				code = HOTP(raw, uint64(now.Unix()/30), 6)
			}
			var wg sync.WaitGroup
			results := make(chan error, 2)
			for i := 0; i < 2; i++ {
				wg.Add(1)
				go func() { defer wg.Done(); results <- s.ConsumeFactor(1, code) }()
			}
			wg.Wait()
			close(results)
			accepted := 0
			for err := range results {
				if err == nil {
					accepted++
				} else if !errors.Is(err, ErrFactorInvalid) {
					t.Fatalf("unexpected concurrent factor error: %v", err)
				}
			}
			if accepted != 1 {
				t.Fatalf("concurrent %s accepted %d times", kind, accepted)
			}
		})
	}
}
func TestPersonalTokenHashExpiryIntersectionRevocationAndIsolation(t *testing.T) {
	s, db, now := identityTestService(t)
	a, err := s.CreateAccount("operator", "IdentityTest123!", "operator")
	if err != nil {
		t.Fatal(err)
	}
	token, secret, err := s.CreatePersonalToken(a.ID, "Automation", []string{"operational:read", "maintenance:write"}, now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	if err = db.QueryRow("SELECT secret_hash FROM auth_personal_tokens WHERE id=?", token.ID).Scan(&stored); err != nil || stored == secret || len(stored) != 64 {
		t.Fatal("invalid token storage", err)
	}
	owner, verified, err := s.VerifyPersonalToken(secret, "192.0.2.x")
	if err != nil || !verified.Allows(owner, "maintenance:write") {
		t.Fatal("token invalid", err)
	}
	if err = s.UpdateAccount(a.ID, "reader", false); err != nil {
		t.Fatal(err)
	}
	owner, verified, err = s.VerifyPersonalToken(secret, "192.0.2.x")
	if err != nil || verified.Allows(owner, "maintenance:write") || !verified.Allows(owner, "operational:read") {
		t.Fatal("role intersection failed", err)
	}
	if yes, err := s.RevokePersonalToken(1, token.ID); err != nil || yes {
		t.Fatal("cross-account token revoke", err)
	}
	listed, err := s.PersonalTokens(a.ID)
	if err != nil || len(listed) != 1 || listed[0].LastUsedOrigin != "192.0.2.x" {
		t.Fatal("usage metadata", listed, err)
	}
	*now = now.Add(2 * time.Hour)
	if _, _, err = s.VerifyPersonalToken(secret, ""); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("expired token accepted", err)
	}
	*now = now.Add(-2 * time.Hour)
	if err = s.UpdateAccount(a.ID, "reader", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.VerifyPersonalToken(secret, ""); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("disabled account token accepted", err)
	}
	if err = s.UpdateAccount(a.ID, "reader", false); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RevokePersonalToken(a.ID, token.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.VerifyPersonalToken(secret, ""); !errors.Is(err, ErrTokenInvalid) {
		t.Fatal("revoked token accepted", err)
	}
	if _, _, err = s.CreatePersonalToken(a.ID, "escalate", []string{"settings:write"}, now.Add(time.Hour)); !IsValidationError(err) {
		t.Fatal("token scopes escalated", err)
	}
}
func TestScopedSessionRepositoryAndStaleIdentityCommit(t *testing.T) {
	s, db, _ := identityTestService(t)
	other, err := s.CreateAccount("reader", "IdentityTest123!", "reader")
	if err != nil {
		t.Fatal(err)
	}
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var otherRaw []byte
	var otherExpiry time.Time
	for _, a := range []Account{{ID: 1, Username: "admin"}, other} {
		ctx, err := sm.Load(context.Background(), "")
		if err != nil {
			t.Fatal(err)
		}
		sm.Put(ctx, SessionUserKey, a.Username)
		if err = StageAuthentication(ctx, sm); err != nil {
			t.Fatal(err)
		}
		if err = StageIdentity(ctx, sm, a.Username); err != nil {
			t.Fatal(err)
		}
		token, expiry, err := sm.Commit(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if a.ID == other.ID {
			data, _, findErr := sm.Store.Find(token)
			if findErr != nil {
				t.Fatal(findErr)
			}
			otherRaw = data
			otherExpiry = expiry
		}
	}
	mine, err := s.ForAccount(1).ListSessions("")
	if err != nil || len(mine) != 1 {
		t.Fatal("session isolation", len(mine), err)
	}
	theirs, err := s.ForAccount(other.ID).ListSessions("")
	if err != nil || len(theirs) != 1 {
		t.Fatal("session isolation", len(theirs), err)
	}
	if found, err := s.ForAccount(1).RevokeSession(theirs[0].ID); err != nil || found {
		t.Fatal("cross-account session revoke accepted", err)
	}
	if _, err = s.ForAccount(1).ClearSessions(); err != nil {
		t.Fatal(err)
	}
	if n, err := s.ForAccount(other.ID).CountSessions(); err != nil || n != 1 {
		t.Fatal("cleared another account's sessions", n, err)
	}
	if err = s.UpdateAccount(other.ID, "reader", true); err != nil {
		t.Fatal(err)
	}
	if err = sm.Store.Commit("racing-refresh", otherRaw, otherExpiry); !errors.Is(err, ErrAuthenticationChanged) {
		t.Fatal("disabled session resurrected", err)
	}
}

func TestAccountCredentialEpochIsolatesLoginAndMFAFromOtherAccounts(t *testing.T) {
	s, db, now := identityTestService(t)
	_, codes := enableTestMFA(t, s, now)
	other, err := s.CreateAccount("other", "IdentityTest123!", "operator")
	if err != nil {
		t.Fatal(err)
	}
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = s.PrepareAccountAuthentication(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = PrepareAuthentication(ctx, sm)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := s.NewLoginChallenge(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForAccount(other.ID).ChangePassword("IdentityTest123!", "OtherChanged123!", "OtherChanged123!"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ForAccount(other.ID).ClearSessions(); err != nil {
		t.Fatal(err)
	}
	a, err := s.CompleteLoginChallenge(challenge, codes[0])
	if err != nil || a.ID != 1 {
		t.Fatalf("another account invalidated MFA challenge: %+v %v", a, err)
	}
	sm.Put(ctx, SessionUserKey, "admin")
	if err = StageAuthentication(ctx, sm); err != nil {
		t.Fatal(err)
	}
	if err = StageIdentity(ctx, sm, "admin"); err != nil {
		t.Fatal(err)
	}
	if _, _, err = sm.Commit(ctx); err != nil {
		t.Fatalf("another account invalidated staged login: %v", err)
	}
	stale, err := s.PrepareAccountAuthentication(context.Background(), "admin")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ForAccount(1).ChangePassword("IdentityTest123!", "AdminChanged123!", "AdminChanged123!"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.NewLoginChallenge(stale, "admin"); !errors.Is(err, ErrAuthenticationChanged) {
		t.Fatalf("own obsolete verification accepted: %v", err)
	}
}

func TestInstallationReplacementEpochRejectsMatchingOldAccountAndLoginEpoch(t *testing.T) {
	s, db, now := identityTestService(t)
	_, codes := enableTestMFA(t, s, now)
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err := sm.Load(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = s.PrepareAccountAuthentication(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = PrepareAuthentication(ctx, sm)
	if err != nil {
		t.Fatal(err)
	}
	challenge, err := s.NewLoginChallenge(ctx, "admin")
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.CompleteLoginChallenge(challenge, codes[0])
	if err != nil {
		t.Fatal(err)
	}
	sm.Put(ctx, SessionUserKey, "admin")
	if err = StageAuthentication(ctx, sm); err != nil {
		t.Fatal(err)
	}
	if err = StageIdentity(ctx, sm, "admin"); err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	data, err := sm.Codec.Encode(expiry, map[string]any{SessionUserKey: "admin", SessionAccountKey: a.ID, SessionGenerationKey: a.Generation, sessionAccountLoginGenerationKey: a.LoginGeneration, sessionInstallationEpochKey: sm.GetString(ctx, sessionInstallationEpochKey), sessionLoginGenerationKey: sm.Get(ctx, sessionLoginGenerationKey)})
	if err != nil {
		t.Fatal(err)
	}
	// Model restored persistence with deliberately identical account counters.
	if _, err = db.Exec("UPDATE auth_installation_epoch SET epoch=lower(hex(randomblob(32))) WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	if err = sm.Store.Commit("old-mfa-completion", data, expiry); !errors.Is(err, ErrAuthenticationChanged) {
		t.Fatal("restored matching counters accepted old MFA completion", err)
	}
	if _, err = s.NewLoginChallenge(ctx, "admin"); !errors.Is(err, ErrAuthenticationChanged) {
		t.Fatal("replacement accepted old password verification", err)
	}
}
