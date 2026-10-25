package vulnerabilities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type Repository struct{ DB func() *sql.DB }

func EnsureSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS server_vulnerability_observations (server_name TEXT PRIMARY KEY, observation TEXT NOT NULL)`)
	return err
}

func (r Repository) Load(ctx context.Context, name string) (Observation, error) {
	var raw string
	err := r.DB().QueryRowContext(ctx, `SELECT observation FROM server_vulnerability_observations WHERE server_name = ?`, name).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return Observation{}, nil
	}
	if err != nil {
		return Observation{}, err
	}
	var out Observation
	err = json.Unmarshal([]byte(raw), &out)
	return out, err
}

func (r Repository) All(ctx context.Context) (map[string]Observation, error) {
	rows, err := r.DB().QueryContext(ctx, `SELECT server_name, observation FROM server_vulnerability_observations`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Observation{}
	for rows.Next() {
		var name, raw string
		if err := rows.Scan(&name, &raw); err != nil {
			return nil, err
		}
		var observation Observation
		if err := json.Unmarshal([]byte(raw), &observation); err != nil {
			return nil, err
		}
		out[name] = observation
	}
	return out, rows.Err()
}

// Caller owns Server admission and a Maintenance Coordination shared lease.
func (r Repository) Save(ctx context.Context, name string, scan Scan) (Observation, error) {
	previous, err := r.Load(ctx, name)
	if err != nil {
		return Observation{}, err
	}
	next := Accept(previous, scan)
	raw, err := json.Marshal(next)
	if err != nil {
		return Observation{}, err
	}
	_, err = r.DB().ExecContext(ctx, `INSERT INTO server_vulnerability_observations(server_name, observation) VALUES (?, ?) ON CONFLICT(server_name) DO UPDATE SET observation = excluded.observation`, name, string(raw))
	return next, err
}

func RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	_, err := tx.Exec(`UPDATE server_vulnerability_observations SET server_name = ? WHERE server_name = ?`, newName, oldName)
	return err
}
func DeleteServerTx(tx *sql.Tx, name string) error {
	_, err := tx.Exec(`DELETE FROM server_vulnerability_observations WHERE server_name = ?`, name)
	return err
}
