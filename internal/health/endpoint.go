package health

import (
	"database/sql"
	"errors"
	"time"
)

func ensureEndpointSchema(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_health_endpoints (
		server_name TEXT PRIMARY KEY, endpoint TEXT NOT NULL, changed_at TEXT NOT NULL)`); err != nil {
		return err
	}
	return ensureColumn(db, "server_health_snapshots", "endpoint", "TEXT NOT NULL DEFAULT ''")
}

func (r SQLiteObservation) InvalidateEndpointTx(tx *sql.Tx, name, oldEndpoint, endpoint string) error {
	if _, err := tx.Exec(`UPDATE server_health_snapshots SET endpoint = ? WHERE server_name = ? AND endpoint = ''`, oldEndpoint, name); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO server_health_endpoints(server_name, endpoint, changed_at) VALUES(?, ?, ?)
		ON CONFLICT(server_name) DO UPDATE SET endpoint=excluded.endpoint, changed_at=excluded.changed_at`,
		name, endpoint, r.now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM server_facts WHERE server_name = ?", name)
	return err
}

func validateCollectedEndpoint(tx *sql.Tx, record CollectedFacts) error {
	var endpoint, changedAt string
	err := tx.QueryRow("SELECT endpoint, changed_at FROM server_health_endpoints WHERE server_name = ?", record.ServerName).Scan(&endpoint, &changedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if record.Endpoint != "" && record.Endpoint != endpoint {
		return errors.New("collected facts belong to a different server endpoint")
	}
	collected, err := time.Parse(time.RFC3339Nano, record.CollectedAt)
	if err != nil {
		return err
	}
	changed, err := time.Parse(time.RFC3339Nano, changedAt)
	if err != nil {
		return err
	}
	if collected.Before(changed) {
		return errors.New("collected facts predate the current server endpoint")
	}
	return nil
}

func (r SQLiteObservation) addEndpointBoundaries(facts map[string]CollectedFacts) error {
	rows, err := r.dbConn().Query("SELECT server_name, changed_at FROM server_health_endpoints")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name, changedAt string
		if err := rows.Scan(&name, &changedAt); err != nil {
			return err
		}
		fact := facts[name]
		fact.ValidAfter = changedAt
		facts[name] = fact
	}
	return rows.Err()
}
