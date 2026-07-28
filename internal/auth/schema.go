package auth

import (
	"database/sql"
)

func EnsureSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_users (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			username TEXT NOT NULL,
			password_hash TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS sessions (
			token TEXT PRIMARY KEY,
			data BLOB NOT NULL,
			expiry REAL NOT NULL
		)
	`); err != nil {
		return err
	}
	sessionRows, err := db.Query("PRAGMA table_info(sessions)")
	if err != nil {
		return err
	}
	defer sessionRows.Close()
	hasSessionToken := false
	hasSessionData := false
	hasSessionExpiry := false
	hasSessionExpires := false
	for sessionRows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := sessionRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return err
		}
		switch name {
		case "token":
			hasSessionToken = true
		case "data":
			hasSessionData = true
		case "expiry":
			hasSessionExpiry = true
		case "expires":
			hasSessionExpires = true
		}
	}
	if err := sessionRows.Err(); err != nil {
		return err
	}
	// Session rows are ephemeral: if legacy schema is incompatible, recreate table.
	if !hasSessionToken || !hasSessionData {
		if _, err := db.Exec("DROP TABLE IF EXISTS sessions"); err != nil {
			return err
		}
		if _, err := db.Exec(`
			CREATE TABLE sessions (
				token TEXT PRIMARY KEY,
				data BLOB NOT NULL,
				expiry REAL NOT NULL
			)
		`); err != nil {
			return err
		}
		hasSessionExpiry = true
	} else if !hasSessionExpiry {
		if _, err := db.Exec("ALTER TABLE sessions ADD COLUMN expiry REAL NOT NULL DEFAULT 0"); err != nil {
			return err
		}
		if hasSessionExpires {
			if _, err := db.Exec("UPDATE sessions SET expiry = CAST(expires AS REAL) WHERE expiry = 0"); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec("CREATE INDEX IF NOT EXISTS sessions_expiry_idx ON sessions(expiry)"); err != nil {
		return err
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS auth_session_metadata (
			token TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			client_ip TEXT NOT NULL,
			client_ip_encrypted TEXT NOT NULL DEFAULT '',
			client_label TEXT NOT NULL
		)
	`); err != nil {
		return err
	}
	metadataRows, err := db.Query("PRAGMA table_info(auth_session_metadata)")
	if err != nil {
		return err
	}
	hasEncryptedIP := false
	for metadataRows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := metadataRows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			metadataRows.Close()
			return err
		}
		hasEncryptedIP = hasEncryptedIP || name == "client_ip_encrypted"
	}
	if err := metadataRows.Err(); err != nil {
		metadataRows.Close()
		return err
	}
	if err := metadataRows.Close(); err != nil {
		return err
	}
	if !hasEncryptedIP {
		if _, err := db.Exec("ALTER TABLE auth_session_metadata ADD COLUMN client_ip_encrypted TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
	}
	return nil
}
