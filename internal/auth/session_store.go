package auth

import (
	"database/sql"
	"time"

	"github.com/alexedwards/scs/sqlite3store"
)

// Session deletion records a durable tombstone in the same transaction. The
// insert guard prevents an older request from restoring the deleted token.
// Tombstones intentionally outlive requests, session managers and restarts.
func ensureSessionRevocationSchema(db *sql.DB) error {
	_, err := db.Exec(`
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
	// Unlike REPLACE, UPSERT does not delete the previous row (which would
	// revoke the token when SQLite recursive triggers are enabled).
	_, err := s.db.Exec(`INSERT INTO sessions(token, data, expiry)
		VALUES (?, ?, julianday(?))
		ON CONFLICT(token) DO UPDATE SET data=excluded.data, expiry=excluded.expiry`,
		token, data, expiry.UTC().Format("2006-01-02T15:04:05.999"))
	return err
}
