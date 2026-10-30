package vulnerabilities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

type Repository struct{ DB func() *sql.DB }

var ErrObservationChanged = errors.New("vulnerability observation changed during reconciliation")

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
	if err == nil {
		NormalizeLegacyAvailability(&out)
	}
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
		NormalizeLegacyAvailability(&observation)
		out[name] = observation
	}
	return out, rows.Err()
}

// Caller owns Server admission and a Maintenance Coordination shared lease.
func (r Repository) Save(ctx context.Context, name string, scan Scan) (Observation, error) {
	return r.SaveGuarded(ctx, name, scan, func(commit func() error) error { return commit() })
}

// SaveGuarded stages the observation under the operation context, then lets its
// admission owner serialize acceptance with cancellation at the commit boundary.
func (r Repository) SaveGuarded(ctx context.Context, name string, scan Scan, acceptCommit func(func() error) error) (Observation, error) {
	return r.save(ctx, name, scan, acceptCommit, nil)
}

// SaveReconciled updates only the attempt that was read for recovery. Inventory
// edits can rename or remove its observation while reconciliation is in flight.
func (r Repository) SaveReconciled(ctx context.Context, name string, scan Scan, expectedID string) (Observation, error) {
	return r.save(ctx, name, scan, func(commit func() error) error { return commit() }, &expectedID)
}

func (r Repository) save(ctx context.Context, name string, scan Scan, acceptCommit func(func() error) error, expectedID *string) (Observation, error) {
	tx, err := r.DB().BeginTx(ctx, nil)
	if err != nil {
		return Observation{}, err
	}
	defer tx.Rollback()
	var previous Observation
	var raw string
	err = tx.QueryRowContext(ctx, `SELECT observation FROM server_vulnerability_observations WHERE server_name = ?`, name).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Observation{}, err
	}
	if err == nil {
		if err := json.Unmarshal([]byte(raw), &previous); err != nil {
			return Observation{}, err
		}
	}
	if expectedID != nil && (errors.Is(err, sql.ErrNoRows) || previous.LastAttempt.ID != *expectedID) {
		return Observation{}, ErrObservationChanged
	}
	// Rename migrates the observation row and its job, while retaining the
	// collector's original Server name in historical evidence. Recovery may
	// retain that name only for the exact operation and owner already in this
	// row; ordinary writes and unrelated Server proofs still fail closed.
	renamedRecovery := expectedID != nil && scan.ID == *expectedID && scan.ServerName == previous.LastAttempt.ServerName
	if (scan.ServerName != "" && scan.ServerName != name && !renamedRecovery) || attemptIsOlder(previous, scan) {
		return Observation{}, ErrObservationChanged
	}
	if scanMayResolve(scan) && previous.LastAttempt.ID != "" && previous.LastAttempt.ID != scan.ID {
		// A proof-bearing terminal result must belong to the operation admitted
		// into this row. Never rebind late evidence to a different generation.
		return Observation{}, ErrObservationChanged
	}
	next := Accept(previous, scan)
	NormalizeLegacyAvailability(&next)
	blob, err := json.Marshal(next)
	if err != nil {
		return Observation{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO server_vulnerability_observations(server_name, observation) VALUES (?, ?) ON CONFLICT(server_name) DO UPDATE SET observation = excluded.observation`, name, string(blob)); err != nil {
		return Observation{}, err
	}
	if err := ctx.Err(); err != nil {
		return Observation{}, err
	}
	if err := acceptCommit(tx.Commit); err != nil {
		return Observation{}, err
	}
	return next, nil
}

func RenameServerTx(tx *sql.Tx, oldName, newName string) error {
	_, err := tx.Exec(`UPDATE server_vulnerability_observations SET server_name = ? WHERE server_name = ?`, newName, oldName)
	return err
}
func DeleteServerTx(tx *sql.Tx, name string) error {
	_, err := tx.Exec(`DELETE FROM server_vulnerability_observations WHERE server_name = ?`, name)
	return err
}

// MarkAudited retains operation audit deduplication after audit retention pruning.
// The identity guard cannot mark a later attempt as audited.
func (r Repository) MarkAudited(ctx context.Context, name, id string) error {
	_, err := r.DB().ExecContext(ctx, `UPDATE server_vulnerability_observations SET observation = json_set(observation, '$.audited_scan_id', ?) WHERE server_name = ? AND json_extract(observation, '$.last_attempt.scan_id') = ?`, id, name, id)
	return err
}
