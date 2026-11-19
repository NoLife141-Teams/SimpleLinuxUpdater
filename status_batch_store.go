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
CREATE INDEX IF NOT EXISTS status_action_batches_attempts ON status_action_batches(account_id, retry_of);
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
	Name                   string `json:"name"`
	ServerIdentity         string `json:"server_identity"`
	State                  string `json:"state"`
	Message                string `json:"message,omitempty"`
	JobID                  string `json:"job_id,omitempty"`
	JobStatus              string `json:"job_status,omitempty"`
	ReconciliationRequired bool   `json:"reconciliation_required,omitempty"`
	DiagnosticAvailable    bool   `json:"diagnostic_available,omitempty"`
	Retryable              bool   `json:"retryable,omitempty"`
	RetryBatchID           string `json:"retry_batch_id,omitempty"`
	RetryNeedsVerification bool   `json:"retry_needs_verification,omitempty"`
	approval               serverActionApprovalIdentity
	aptTarget              string
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

var errBatchKeyConflict = errors.New("batch identifier conflict")
var errBatchRetryPlanChanged = errors.New("maintenance batch retry plan changed")

type statusBatchRetryConflict struct {
	batchID string
}

func (*statusBatchRetryConflict) Error() string {
	return "maintenance batch target is no longer eligible for retry"
}

func (host statusBatchHost) canRetry(active bool, action string) bool {
	if batchActionScope(action) == "approvals:write" && (host.approval.JobID == "" || host.approval.Generation == 0) {
		return false
	}
	return (host.State == "refused" || (host.State == "queued" && !active)) &&
		host.RetryBatchID == "" && !host.RetryNeedsVerification
}

func storeStatusBatch(ctx context.Context, db *sql.DB, request statusBatchRequest, fingerprint string, account int64, actor, created string, sourceActive bool) (bool, error) {
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
	if request.RetryOf != "" {
		// The header insert obtains the SQLite write lock before checking for
		// previous attempts. The new child rows are the reservation and become
		// visible atomically with the header at commit.
		if err := checkStatusBatchRetry(ctx, tx, request, account, sourceActive); err != nil {
			return false, err
		}
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

func checkStatusBatchRetry(ctx context.Context, tx *sql.Tx, req statusBatchRequest, account int64, active bool) error {
	source, err := readStatusBatch(ctx, tx, req.RetryOf, account)
	if errors.Is(err, sql.ErrNoRows) {
		return &statusBatchRetryConflict{}
	}
	if err != nil {
		return err
	}
	if source.Action != req.Action {
		return &statusBatchRetryConflict{}
	}
	hosts := make(map[string]statusBatchHost, len(source.Hosts))
	for _, host := range source.Hosts {
		hosts[host.Name] = host
	}
	for _, target := range req.Targets {
		host := hosts[target.Name]
		if host.ServerIdentity != target.ServerIdentity {
			return &statusBatchRetryConflict{}
		}
		if !host.canRetry(active, source.Action) {
			return &statusBatchRetryConflict{batchID: host.RetryBatchID}
		}
		if batchActionScope(source.Action) == "approvals:write" && host.approval != target.Approval {
			return errBatchRetryPlanChanged
		}
	}
	return nil
}

type statusBatchQuerier interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

type statusBatchWriter interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func recordStatusBatchOutcome(ctx context.Context, db statusBatchWriter, batchID string, host statusBatchTarget, from, state, message, jobID string) error {
	result, err := db.ExecContext(ctx, `UPDATE status_action_batch_hosts SET state = ?, message = ?, job_id = ?
		WHERE batch_id = ? AND name = ? AND server_identity = ? AND state = ?`,
		state, message, jobID, batchID, host.Name, host.ServerIdentity, from)
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

func readStatusBatch(ctx context.Context, db statusBatchQuerier, id string, account int64) (statusBatch, error) {
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
	// A pre-upgrade batch could already have competing children. A child in
	// such a lineage cannot prove that its sibling never reached admission.
	rows, err := db.QueryContext(ctx, `
		WITH RECURSIVE lineage(id, retry_of) AS (
			SELECT id, retry_of FROM status_action_batches WHERE id = ?
			UNION
			SELECT p.id, p.retry_of FROM status_action_batches p JOIN lineage l ON p.id = l.retry_of
			WHERE p.account_id = ? AND p.action = ?
		)
		SELECT h.name, h.server_identity, h.state, h.message, h.job_id, COALESCE(j.status, ''),
			COALESCE(j.status = 'failed' AND j.error_class = 'reconciliation_required', 0),
			COALESCE(j.apt_target, ''),
			h.approval_job_id, h.approval_generation, h.deferral_revision, h.remediation_digest,
			COALESCE((
				SELECT a.id FROM status_action_batches a JOIN status_action_batch_hosts ah ON ah.batch_id = a.id
				WHERE a.account_id = ? AND a.retry_of = h.batch_id AND a.action = ? AND ah.server_identity = h.server_identity
				ORDER BY (ah.state NOT IN ('refused', 'queued')) DESC, a.rowid DESC LIMIT 1
			), ''),
			EXISTS (
				SELECT 1 FROM lineage l
				JOIN status_action_batches a ON a.account_id = ? AND a.retry_of = l.retry_of
				JOIN status_action_batch_hosts ah ON ah.batch_id = a.id
				WHERE l.retry_of <> '' AND a.id <> l.id AND a.action = ? AND ah.server_identity = h.server_identity
			)
		FROM status_action_batch_hosts h LEFT JOIN jobs j ON j.id = h.job_id
		WHERE h.batch_id = ? ORDER BY h.name`, batch.ID, account, batch.Action, account, batch.Action, account, batch.Action, batch.ID)
	if err != nil {
		return statusBatch{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var host statusBatchHost
		if err := rows.Scan(&host.Name, &host.ServerIdentity, &host.State, &host.Message, &host.JobID, &host.JobStatus, &host.ReconciliationRequired, &host.aptTarget,
			&host.approval.JobID, &host.approval.Generation, &host.approval.DeferralRevision, &host.approval.RemediationDigest,
			&host.RetryBatchID, &host.RetryNeedsVerification); err != nil {
			return statusBatch{}, err
		}
		batch.Hosts = append(batch.Hosts, host)
	}
	return batch, rows.Err()
}
