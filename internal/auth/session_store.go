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
	accountID, identity := values[SessionAccountKey].(int64)
	accountGeneration, _ := values[SessionGenerationKey].(int64)
	accountLoginGeneration, _ := values[sessionAccountLoginGenerationKey].(int64)
	installationEpoch, _ := values[sessionInstallationEpochKey].(string)
	if !identity {
		accountID = 1
	}
	// Unlike REPLACE, UPSERT does not delete the previous row (which would
	// revoke the token when SQLite recursive triggers are enabled).
	result, err := s.db.Exec(`INSERT INTO sessions(token, data, expiry, account_id)
		SELECT ?, ?, julianday(?), ? WHERE
			(EXISTS(SELECT 1 FROM sessions WHERE token = ?) OR
			 (? = 1 AND EXISTS(SELECT 1 FROM auth_users WHERE id=? AND login_generation=?)) OR
			 (? = 0 AND (?=0 OR EXISTS(SELECT 1 FROM auth_login_generation WHERE id=1 AND generation=?))))
			AND (? = 0 OR EXISTS(SELECT 1 FROM auth_users WHERE id=? AND generation=? AND disabled=0))
			AND (? = 0 OR EXISTS(SELECT 1 FROM auth_installation_epoch WHERE id=1 AND epoch=?))
		ON CONFLICT(token) DO UPDATE SET data=excluded.data, expiry=excluded.expiry`,
		token, data, expiry.UTC().Format("2006-01-02T15:04:05.999"), accountID, token, identity, accountID, accountLoginGeneration, identity, guarded, generation, identity, accountID, accountGeneration, identity, installationEpoch)
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

// StageIdentity binds a session to an immutable account ID and current epoch.
func StageIdentity(ctx context.Context, sm *scs.SessionManager, username string) error {
	store, ok := sm.Store.(*revocationAwareSessionStore)
	if !ok {
		return errors.New("identity session store unavailable")
	}
	var id, generation, loginGeneration int64
	if err := store.db.QueryRow("SELECT id,generation,login_generation FROM auth_users WHERE username=? AND disabled=0", username).Scan(&id, &generation, &loginGeneration); err != nil {
		return err
	}
	if prepared, ok := ctx.Value(preparedAccountKey{}).(Account); ok {
		if prepared.ID != id || prepared.Generation != generation || prepared.LoginGeneration != loginGeneration {
			return ErrAuthenticationChanged
		}
	}
	sm.Put(ctx, SessionAccountKey, id)
	sm.Put(ctx, SessionGenerationKey, generation)
	sm.Put(ctx, sessionAccountLoginGenerationKey, loginGeneration)
	sm.Put(ctx, "auth_reauthenticated_at", time.Now().Unix())
	return nil
}

func SessionAccount(ctx context.Context, sm *scs.SessionManager) (Account, error) {
	store, ok := sm.Store.(*revocationAwareSessionStore)
	if !ok {
		return Account{}, nil
	}
	id, _ := sm.Get(ctx, SessionAccountKey).(int64)
	generation, _ := sm.Get(ctx, SessionGenerationKey).(int64)
	if id == 0 {
		return Account{}, nil
	}
	a, err := scanAccount(store.db.QueryRow("SELECT "+accountColumns+" FROM auth_users WHERE id=?", id))
	if err != nil {
		return Account{}, err
	}
	if a.Disabled || a.Generation != generation || a.Username != sm.GetString(ctx, SessionUserKey) {
		return Account{}, nil
	}
	var epoch string
	if err = store.db.QueryRow("SELECT epoch FROM auth_installation_epoch WHERE id=1").Scan(&epoch); err != nil {
		return Account{}, err
	}
	if epoch != sm.GetString(ctx, sessionInstallationEpochKey) {
		return Account{}, nil
	}
	return a, nil
}

const sessionLoginGenerationKey = "auth_login_generation"
const sessionAccountLoginGenerationKey = "auth_account_login_generation"
const sessionInstallationEpochKey = "auth_installation_epoch"

type installationEpochContextKey struct{}

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
	var epoch string
	if err := store.db.QueryRow("SELECT epoch FROM auth_installation_epoch WHERE id=1").Scan(&epoch); err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, installationEpochContextKey{}, epoch)
	return context.WithValue(ctx, loginGenerationContextKey{}, generation), nil
}

func StageAuthentication(ctx context.Context, sm *scs.SessionManager) error {
	generation, ok := ctx.Value(loginGenerationContextKey{}).(int64)
	epoch, _ := ctx.Value(installationEpochContextKey{}).(string)
	if !ok {
		prepared, err := PrepareAuthentication(ctx, sm)
		if err != nil {
			return err
		}
		generation, ok = prepared.Value(loginGenerationContextKey{}).(int64)
		epoch, _ = prepared.Value(installationEpochContextKey{}).(string)
	}
	if ok {
		sm.Put(ctx, sessionLoginGenerationKey, generation)
		sm.Put(ctx, sessionInstallationEpochKey, epoch)
	}
	return nil
}
