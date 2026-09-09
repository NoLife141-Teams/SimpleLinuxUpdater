package auth

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
	"github.com/alexedwards/scs/v2"
)

// Session deletion records a durable tombstone in the same transaction. The
// insert guard prevents an older request from restoring the deleted token.
// Tombstones intentionally outlive requests, session managers and restarts.
func ensureSessionRevocationSchema(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_login_generation (id INTEGER PRIMARY KEY CHECK(id = 1), generation INTEGER NOT NULL);
		INSERT OR IGNORE INTO auth_login_generation VALUES(1, 0);
		CREATE TRIGGER IF NOT EXISTS advance_login_generation AFTER UPDATE OF password_hash ON auth_users
		BEGIN
			UPDATE auth_login_generation SET generation = generation + 1 WHERE id = 1;
		END;
		CREATE TABLE IF NOT EXISTS auth_session_revocations (token TEXT PRIMARY KEY);
		CREATE TRIGGER IF NOT EXISTS revoke_deleted_session AFTER DELETE ON sessions
		BEGIN
			INSERT OR IGNORE INTO auth_session_revocations(token) VALUES(OLD.token);
		END;
		CREATE TRIGGER IF NOT EXISTS prevent_revoked_session_insert BEFORE INSERT ON sessions
		WHEN EXISTS(SELECT 1 FROM auth_session_revocations WHERE token = NEW.token)
		BEGIN
			SELECT RAISE(IGNORE);
		END;
	`)
	return err
}

type revocationAwareSessionStore struct {
	*sqlite3store.SQLite3Store
	db *sql.DB
}

func (s *revocationAwareSessionStore) Commit(token string, data []byte, expiry time.Time) error {
	_, values, err := (scs.GobCodec{}).Decode(data)
	if err != nil {
		return err
	}
	generation, guarded := values[sessionLoginGenerationKey].(int64)
	// Unlike REPLACE, UPSERT does not delete the previous row (which would
	// revoke the token when SQLite recursive triggers are enabled).
	result, err := s.db.Exec(`INSERT INTO sessions(token, data, expiry)
		SELECT ?, ?, julianday(?) WHERE
			EXISTS(SELECT 1 FROM sessions WHERE token = ?) OR ? = 0 OR
			EXISTS(SELECT 1 FROM auth_login_generation WHERE id = 1 AND generation = ?)
		ON CONFLICT(token) DO UPDATE SET data=excluded.data, expiry=excluded.expiry`,
		token, data, expiry.UTC().Format("2006-01-02T15:04:05.999"), token, guarded, generation)
	if err == nil && guarded {
		affected, countErr := result.RowsAffected()
		if countErr != nil {
			return countErr
		}
		if affected == 0 {
			return ErrAuthenticationChanged
		}
	}
	return err
}

const sessionLoginGenerationKey = "auth_login_generation"

type loginGenerationContextKey struct{}

var ErrAuthenticationChanged = errors.New("authentication changed during login")

// Capture before credential verification; the store enforces this generation
// in the same SQLite statement that commits a newly issued session.
func PrepareAuthentication(ctx context.Context, sm *scs.SessionManager) (context.Context, error) {
	if sm == nil {
		return ctx, errors.New("session manager not initialized")
	}
	store, ok := sm.Store.(*revocationAwareSessionStore)
	if !ok {
		return ctx, nil
	}
	var generation int64
	if err := store.db.QueryRow("SELECT generation FROM auth_login_generation WHERE id = 1").Scan(&generation); err != nil {
		return ctx, err
	}
	return context.WithValue(ctx, loginGenerationContextKey{}, generation), nil
}

func StageAuthentication(ctx context.Context, sm *scs.SessionManager) error {
	generation, ok := ctx.Value(loginGenerationContextKey{}).(int64)
	if !ok {
		prepared, err := PrepareAuthentication(ctx, sm)
		if err != nil {
			return err
		}
		generation, ok = prepared.Value(loginGenerationContextKey{}).(int64)
	}
	if ok {
		sm.Put(ctx, sessionLoginGenerationKey, generation)
	}
	return nil
}
