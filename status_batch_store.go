package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

const statusBatchSchema = `
CREATE TABLE IF NOT EXISTS status_action_batches (
	id TEXT PRIMARY KEY, account_id INTEGER NOT NULL, actor TEXT NOT NULL,
	action TEXT NOT NULL, created_at TEXT NOT NULL, fingerprint TEXT NOT NULL,
	skipped_count INTEGER NOT NULL, retry_of TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS status_action_batches_owner ON status_action_batches(account_id, created_at DESC, id);
CREATE TABLE IF NOT EXISTS status_action_batch_hosts (
	batch_id TEXT NOT NULL REFERENCES status_action_batches(id) ON DELETE CASCADE,
	name TEXT NOT NULL, server_identity TEXT NOT NULL,
	approval_job_id TEXT NOT NULL, approval_generation INTEGER NOT NULL,
	deferral_revision TEXT NOT NULL, remediation_digest TEXT NOT NULL,
	state TEXT NOT NULL, message TEXT NOT NULL DEFAULT '', job_id TEXT NOT NULL DEFAULT '',
	PRIMARY KEY(batch_id, name)
);`

type statusBatchTarget struct {
	Name           string                       `json:"name"`
	ServerIdentity string                       `json:"server_identity"`
	Approval       serverActionApprovalIdentity `json:"approval"`
}

type statusBatchRequest struct {
	ID           string              `json:"id"`
	Action       string              `json:"action"`
	Targets      []statusBatchTarget `json:"targets"`
	SkippedCount int                 `json:"skipped_count"`
	RetryOf      string              `json:"retry_of,omitempty"`
}

type statusBatchHost struct {
	Name           string `json:"name"`
	ServerIdentity string `json:"server_identity"`
	State          string `json:"state"`
	Message        string `json:"message,omitempty"`
	JobID          string `json:"job_id,omitempty"`
	JobStatus      string `json:"job_status,omitempty"`
	Retryable      bool   `json:"retryable,omitempty"`
}

type statusBatch struct {
	ID           string            `json:"id"`
	Actor        string            `json:"actor"`
	Action       string            `json:"action"`
	CreatedAt    string            `json:"created_at"`
	SkippedCount int               `json:"skipped_count"`
	RetryOf      string            `json:"retry_of,omitempty"`
	Hosts        []statusBatchHost `json:"hosts"`
}

var errBatchKeyConflict = errors.New("This batch identifier already belongs to a different request")

func storeStatusBatch(ctx context.Context, db *sql.DB, request statusBatchRequest, fingerprint string, account int64, actor, created string) (bool, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `INSERT INTO status_action_batches
		(id, account_id, actor, action, created_at, fingerprint, skipped_count, retry_of)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(id) DO NOTHING`,
		request.ID, account, actor, request.Action, created, fingerprint, request.SkippedCount, request.RetryOf)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows == 0 {
		var existing string
		err := tx.QueryRowContext(ctx, `SELECT fingerprint FROM status_action_batches WHERE id = ? AND account_id = ?`, request.ID, account).Scan(&existing)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && existing != fingerprint) {
			return false, errBatchKeyConflict
		}
		return false, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO status_action_batch_hosts
		(batch_id, name, server_identity, approval_job_id, approval_generation, deferral_revision, remediation_digest, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'queued')`)
	if err != nil {
		return false, err
	}
	defer stmt.Close()
	for _, target := range request.Targets {
		if _, err := stmt.ExecContext(ctx, request.ID, target.Name, target.ServerIdentity,
			target.Approval.JobID, target.Approval.Generation, target.Approval.DeferralRevision, target.Approval.RemediationDigest); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

func readStatusBatch(ctx context.Context, db *sql.DB, id string, account int64) (statusBatch, error) {
	batch := statusBatch{Hosts: []statusBatchHost{}}
	query := `SELECT id, actor, action, created_at, skipped_count, retry_of FROM status_action_batches WHERE account_id = ?`
	var row *sql.Row
	if id == "" {
		row = db.QueryRowContext(ctx, query+` ORDER BY created_at DESC, rowid DESC LIMIT 1`, account)
	} else {
		row = db.QueryRowContext(ctx, query+` AND id = ?`, account, id)
	}
	if err := row.Scan(&batch.ID, &batch.Actor, &batch.Action, &batch.CreatedAt, &batch.SkippedCount, &batch.RetryOf); err != nil {
		return statusBatch{}, err
	}
	rows, err := db.QueryContext(ctx, `SELECT h.name, h.server_identity, h.state, h.message, h.job_id, COALESCE(j.status, '')
		FROM status_action_batch_hosts h LEFT JOIN jobs j ON j.id = h.job_id
		WHERE h.batch_id = ? ORDER BY h.name`, batch.ID)
	if err != nil {
		return statusBatch{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var host statusBatchHost
		if err := rows.Scan(&host.Name, &host.ServerIdentity, &host.State, &host.Message, &host.JobID, &host.JobStatus); err != nil {
			return statusBatch{}, err
		}
		batch.Hosts = append(batch.Hosts, host)
	}
	return batch, rows.Err()
}

func setStatusBatchHost(db *sql.DB, batchID, name, from, state, message, jobID string) error {
	result, err := db.Exec(`UPDATE status_action_batch_hosts SET state = ?, message = ?, job_id = ?
		WHERE batch_id = ? AND name = ? AND state = ?`, state, message, jobID, batchID, name, from)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("batch host is no longer in state %s", from)
	}
	return nil
}
