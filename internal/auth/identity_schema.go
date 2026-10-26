package auth

import "database/sql"

// Migration is transactional: an old installation keeps account 1, its hash and
// setup state. Only ephemeral sessions are discarded when identity is introduced.
func ensureIdentitySchema(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('auth_users') WHERE name='role'`).Scan(&version); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if version == 0 {
		if _, err = tx.Exec(`
		DROP TRIGGER IF EXISTS advance_login_generation;
		ALTER TABLE auth_users RENAME TO auth_users_legacy;
		CREATE TABLE auth_users (
		 id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE COLLATE BINARY,
		 password_hash TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
		 role TEXT NOT NULL DEFAULT 'admin' CHECK(role IN ('reader','operator','admin')),
		 login_generation INTEGER NOT NULL DEFAULT 0,
		 disabled INTEGER NOT NULL DEFAULT 0 CHECK(disabled IN (0,1)), generation INTEGER NOT NULL DEFAULT 0,
		 totp_secret_enc TEXT NOT NULL DEFAULT '', totp_pending_enc TEXT NOT NULL DEFAULT '',
		 totp_pending_until INTEGER NOT NULL DEFAULT 0, totp_last_step INTEGER NOT NULL DEFAULT -1
		);
		INSERT INTO auth_users(id,username,password_hash,created_at,updated_at)
		 SELECT id,username,password_hash,created_at,updated_at FROM auth_users_legacy;
		DROP TABLE auth_users_legacy;
		DELETE FROM sessions; DELETE FROM auth_session_metadata;
		UPDATE auth_login_generation SET generation=generation+1 WHERE id=1;
		ALTER TABLE sessions ADD COLUMN account_id INTEGER NOT NULL DEFAULT 1;
		`); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`
	CREATE TABLE IF NOT EXISTS auth_installation_epoch(id INTEGER PRIMARY KEY CHECK(id=1),epoch TEXT NOT NULL);
	INSERT OR IGNORE INTO auth_installation_epoch VALUES(1,lower(hex(randomblob(32))));
	DROP TRIGGER IF EXISTS advance_login_generation;
	CREATE TRIGGER advance_login_generation AFTER UPDATE OF password_hash ON auth_users
	BEGIN
	 UPDATE auth_login_generation SET generation=generation+1 WHERE id=1;
	 UPDATE auth_users SET login_generation=login_generation+1 WHERE id=NEW.id;
	END;
	CREATE TRIGGER IF NOT EXISTS protect_last_admin_update BEFORE UPDATE OF role,disabled ON auth_users
	WHEN OLD.role='admin' AND OLD.disabled=0 AND (NEW.role!='admin' OR NEW.disabled!=0)
	 AND (SELECT count(*) FROM auth_users WHERE role='admin' AND disabled=0)=1
	BEGIN SELECT RAISE(ABORT,'last administrator must remain enabled'); END;
	CREATE TRIGGER IF NOT EXISTS protect_last_admin_delete BEFORE DELETE ON auth_users
	WHEN OLD.role='admin' AND OLD.disabled=0 AND (SELECT count(*) FROM auth_users WHERE role='admin' AND disabled=0)=1
	BEGIN SELECT RAISE(ABORT,'last administrator must remain enabled'); END;
	CREATE TRIGGER IF NOT EXISTS revoke_changed_identity AFTER UPDATE OF role,disabled ON auth_users
	WHEN OLD.role!=NEW.role OR OLD.disabled!=NEW.disabled
	BEGIN
	 UPDATE auth_users SET generation=generation+1,login_generation=login_generation+1 WHERE id=NEW.id;
	 DELETE FROM sessions WHERE account_id=NEW.id;
	 UPDATE auth_personal_tokens SET revoked=1 WHERE account_id=NEW.id AND NEW.disabled=1;
	 UPDATE auth_login_generation SET generation=generation+1 WHERE id=1;
	END;
	CREATE TABLE IF NOT EXISTS auth_recovery_codes(account_id INTEGER NOT NULL, code_hash TEXT NOT NULL, PRIMARY KEY(account_id,code_hash));
	CREATE TABLE IF NOT EXISTS auth_factor_attempts(account_id INTEGER PRIMARY KEY, attempts INTEGER NOT NULL, window_until INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS auth_challenges(hash TEXT PRIMARY KEY, account_id INTEGER NOT NULL, generation INTEGER NOT NULL, login_generation INTEGER NOT NULL, expires INTEGER NOT NULL);
	CREATE TABLE IF NOT EXISTS auth_personal_tokens(
	 id TEXT PRIMARY KEY, account_id INTEGER NOT NULL, name TEXT NOT NULL,
	 secret_hash TEXT NOT NULL UNIQUE, scopes TEXT NOT NULL, created_at TEXT NOT NULL,
	 expires_at TEXT NOT NULL, revoked INTEGER NOT NULL DEFAULT 0,
	 last_used_at TEXT NOT NULL DEFAULT '', last_used_origin TEXT NOT NULL DEFAULT ''
	);
	CREATE INDEX IF NOT EXISTS auth_tokens_account ON auth_personal_tokens(account_id);
	CREATE INDEX IF NOT EXISTS sessions_account ON sessions(account_id);
	`)
	if err != nil {
		return err
	}
	return tx.Commit()
}
