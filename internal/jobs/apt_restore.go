package jobs

import (
	"context"
	"database/sql"
	"errors"
)

var ErrAPTRecoveryEvidenceConflict = errors.New("APT reconciliation evidence conflicts with the archive")

func markInterruptedAPTMutations(db interface {
	Exec(string, ...any) (sql.Result, error)
}, now string) error {
	_, err := db.Exec(`
		UPDATE jobs
		   SET status = ?, phase = ?, summary = ?, error_class = ?, finished_at = ?, updated_at = ?, revision = revision + 1
		 WHERE status = ?
		   AND ((kind = ? AND phase = ?) OR (kind = ? AND phase = ?) OR (kind = ? AND phase = ?))
	`,
		StatusFailed, PhaseComplete, "APT reconciliation required after restart interruption", "reconciliation_required", now, now,
		StatusRunning, KindUpdate, PhaseAptUpgrade, KindAutoremove, PhaseAutoremove, KindAptRepair, PhaseReconcile,
	)
	return err
}

// MarkInterruptedAPTMutations prepares an isolated restore database so an
// archived mutation in flight is included when comparing recovery evidence.
func (r *SQLiteRepository) MarkInterruptedAPTMutations(now string) error {
	return markInterruptedAPTMutations(r.db, now)
}

// LatestVerifiedAPTRepairs returns the newest successful repair with host key
// proof for each APT target. Carrying this evidence through intervening restores
// keeps a later restore from reopening an older, already repaired incident.
func LatestVerifiedAPTRepairs(ctx context.Context, read *sql.Tx) ([]Record, error) {
	rows, err := read.QueryContext(ctx, `
		SELECT id, apt_target FROM (
			SELECT id, apt_target,
			       row_number() OVER (PARTITION BY apt_target ORDER BY created_at DESC, id DESC) AS position
			  FROM jobs
			 WHERE apt_target <> '' AND kind = ? AND status = ? AND apt_mutation_host_key <> ''
		) WHERE position = 1
	`, KindAptRepair, StatusSucceeded)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var repairs []Record
	for rows.Next() {
		var repair Record
		if err := rows.Scan(&repair.ID, &repair.APTTarget); err != nil {
			return nil, err
		}
		repairs = append(repairs, repair)
	}
	return repairs, rows.Err()
}

// ImportAPTRecoveryEvidence carries a terminal job and its retained log stream
// from the live database into an isolated restore database.
func (r *SQLiteRepository) ImportAPTRecoveryEvidence(ctx context.Context, read *sql.Tx, id, serverName string, includeLogs bool) error {
	record, err := getJobRow(ctx, read, id)
	if err != nil {
		return err
	}
	record.ServerName = serverName

	write, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer write.Rollback()
	existing, err := getJobRow(ctx, write, id)
	if err == nil && (existing.APTTarget != record.APTTarget || existing.Kind != record.Kind ||
		existing.CreatedAt != record.CreatedAt) {
		return ErrAPTRecoveryEvidenceConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := upsertJobRow(ctx, write, record, true); err != nil {
		return err
	}
	if includeLogs {
		if err := copyAPTRecoveryLogs(ctx, read, write, record); err != nil {
			return err
		}
	}
	return write.Commit()
}

func copyAPTRecoveryLogs(ctx context.Context, read, write *sql.Tx, record Record) error {
	var nextSequence, sourceBytes int64
	err := read.QueryRowContext(ctx, `SELECT log_next_sequence, log_source_bytes FROM jobs WHERE id = ?`, record.ID).
		Scan(&nextSequence, &sourceBytes)
	if err != nil {
		return err
	}
	if _, err := write.ExecContext(ctx, `UPDATE jobs
		SET logs_text = ?, logs_expired = ?, logs_truncated = ?, log_next_sequence = ?, log_source_bytes = ?
		WHERE id = ?`, record.LogsText, record.LogsExpired, record.LogsTruncated, nextSequence, sourceBytes, record.ID); err != nil {
		return err
	}
	if _, err := write.ExecContext(ctx, `DELETE FROM job_log_chunks WHERE job_id = ?`, record.ID); err != nil {
		return err
	}
	rows, err := read.QueryContext(ctx, `
		SELECT sequence, stream, data, created_at FROM job_log_chunks WHERE job_id = ? ORDER BY sequence
	`, record.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	insert, err := write.PrepareContext(ctx, `
		INSERT INTO job_log_chunks (job_id, sequence, stream, data, created_at) VALUES (?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for rows.Next() {
		var sequence int64
		var stream, createdAt string
		var data []byte
		if err := rows.Scan(&sequence, &stream, &data, &createdAt); err != nil {
			return err
		}
		if _, err := insert.ExecContext(ctx, record.ID, sequence, stream, data, createdAt); err != nil {
			return err
		}
	}
	return rows.Err()
}
