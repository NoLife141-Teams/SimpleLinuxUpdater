package auth

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/audit"
	"github.com/alexedwards/scs/v2"
)

func recoveryDB(t *testing.T) (*sql.DB, *Service) {
	t.Helper()
	db := newTestDB(t)
	if err := audit.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	s := NewService(ServiceOptions{DB: func() *sql.DB { return db }})
	if err := s.CreateInitialUser("admin", "OldPassword123!"); err != nil {
		t.Fatal(err)
	}
	return db, s
}

func TestLocalRecoveryIsAtomicAndRejectsStaleSessions(t *testing.T) {
	db, s := recoveryDB(t)
	if _, err := db.Exec("CREATE TABLE business(secret TEXT); INSERT INTO business VALUES('encrypted-ssh-data')"); err != nil {
		t.Fatal(err)
	}
	sm, err := NewSessionManager(db, SessionManagerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := PrepareAuthentication(context.Background(), sm)
	if err != nil {
		t.Fatal(err)
	}
	generation := prepared.Value(loginGenerationContextKey{}).(int64)
	expiry := time.Now().Add(time.Hour)
	data, err := (scs.GobCodec{}).Encode(expiry, map[string]any{SessionUserKey: "admin", sessionLoginGenerationKey: generation})
	if err != nil {
		t.Fatal(err)
	}
	if err = sm.Store.Commit("old-token", data, expiry); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO auth_session_metadata(token,created_at,last_seen_at,client_ip,client_label) VALUES('old-token','','','','fixture')"); err != nil {
		t.Fatal(err)
	}
	var created string
	if err = db.QueryRow("SELECT created_at FROM auth_users WHERE id=1").Scan(&created); err != nil {
		t.Fatal(err)
	}
	result, err := ResetLocalPassword(context.Background(), db, "admin", "NewPassword456!", "NewPassword456!")
	if err != nil || result.RevokedSessions != 1 {
		t.Fatalf("%+v %v", result, err)
	}
	if ok, err := s.Authenticate("admin", "OldPassword123!"); err != nil || ok {
		t.Fatalf("old authentication %v %v", ok, err)
	}
	if ok, err := s.Authenticate("admin", "NewPassword456!"); err != nil || !ok {
		t.Fatalf("new authentication %v %v", ok, err)
	}
	if setup, err := s.SetupRequired(); err != nil || setup {
		t.Fatalf("setup %v %v", setup, err)
	}
	for _, table := range []string{"sessions", "auth_session_metadata"} {
		var n int
		if err = db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s %d %v", table, n, err)
		}
	}
	for _, token := range []string{"old-token", "late-login"} {
		if err = sm.Store.Commit(token, data, expiry); !errors.Is(err, ErrAuthenticationChanged) {
			t.Fatalf("stale %s commit: %v", token, err)
		}
	}
	var after int64
	var afterCreated, secret, actor, action, meta string
	if err = db.QueryRow("SELECT generation FROM auth_login_generation WHERE id=1").Scan(&after); err != nil || after != generation+1 {
		t.Fatalf("generation %d %v", after, err)
	}
	if err = db.QueryRow("SELECT created_at FROM auth_users WHERE id=1").Scan(&afterCreated); err != nil || afterCreated != created {
		t.Fatal("account recreated", err)
	}
	if err = db.QueryRow("SELECT secret FROM business").Scan(&secret); err != nil || secret != "encrypted-ssh-data" {
		t.Fatal("business data changed", err)
	}
	if err = db.QueryRow("SELECT actor,action,meta_json FROM audit_events").Scan(&actor, &action, &meta); err != nil || actor != "local-recovery" || action != "auth.password.recover" || strings.Contains(meta, "Password") {
		t.Fatalf("audit %s/%s/%s %v", actor, action, meta, err)
	}
	// Repeating recovery with already-revoked sessions remains a valid atomic operation.
	if result, err = ResetLocalPassword(context.Background(), db, "admin", "ThirdPassword789!", "ThirdPassword789!"); err != nil || result.RevokedSessions != 0 {
		t.Fatalf("repeat %+v %v", result, err)
	}
}

func TestLocalRecoveryFailuresRollbackAccountSessionsGenerationAndAudit(t *testing.T) {
	for _, failure := range []string{"audit", "session deletion", "missing generation trigger"} {
		t.Run(failure, func(t *testing.T) {
			db, s := recoveryDB(t)
			if _, err := db.Exec("INSERT INTO sessions VALUES('retained',X'00',9999999)"); err != nil {
				t.Fatal(err)
			}
			var hash string
			var generation int64
			if err := db.QueryRow("SELECT password_hash FROM auth_users").Scan(&hash); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRow("SELECT generation FROM auth_login_generation").Scan(&generation); err != nil {
				t.Fatal(err)
			}
			query := "CREATE TRIGGER fail_recovery BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'SECRET-DETAIL'); END"
			if failure == "session deletion" {
				query = "CREATE TRIGGER fail_recovery BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT,'SECRET-DETAIL'); END"
			}
			if failure == "missing generation trigger" {
				query = "DROP TRIGGER advance_login_generation"
			}
			if _, err := db.Exec(query); err != nil {
				t.Fatal(err)
			}
			_, err := ResetLocalPassword(context.Background(), db, "admin", "NewPassword456!", "NewPassword456!")
			if !errors.Is(err, ErrRecoveryFailed) || strings.Contains(err.Error(), "SECRET") {
				t.Fatal(err)
			}
			var afterHash string
			var afterGeneration, n int64
			if err = db.QueryRow("SELECT password_hash FROM auth_users").Scan(&afterHash); err != nil || afterHash != hash {
				t.Fatal("hash changed", err)
			}
			if err = db.QueryRow("SELECT generation FROM auth_login_generation").Scan(&afterGeneration); err != nil || afterGeneration != generation {
				t.Fatal("generation changed", err)
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&n); err != nil || n != 1 {
				t.Fatal("sessions changed", err)
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM audit_events").Scan(&n); err != nil || n != 0 {
				t.Fatal("audit committed", err)
			}
			if ok, err := s.Authenticate("admin", "OldPassword123!"); err != nil || !ok {
				t.Fatal(ok, err)
			}
		})
	}
}

func TestLocalRecoveryRejectsInvalidInputs(t *testing.T) {
	db, _ := recoveryDB(t)
	for _, tc := range []struct{ username, password, confirm string }{
		{"admin", "weak", "weak"}, {"admin", "NewPassword456!", "Different123!"}, {"absent", "NewPassword456!", "NewPassword456!"}, {"bad user", "NewPassword456!", "NewPassword456!"},
	} {
		if _, err := ResetLocalPassword(context.Background(), db, tc.username, tc.password, tc.confirm); err == nil {
			t.Fatal("accepted invalid request")
		}
	}
	if _, err := ResetLocalPassword(context.Background(), nil, "admin", "NewPassword456!", "NewPassword456!"); err == nil {
		t.Fatal("accepted nil DB")
	}
}
