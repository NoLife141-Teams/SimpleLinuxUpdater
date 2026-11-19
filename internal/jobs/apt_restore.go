package jobs

import (
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
func (r *SQLiteRepository) LatestVerifiedAPTRepairs() ([]Record, error) {
	rows, err := r.db.Query(`
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

// ImportAPTRecoveryEvidence carries an exact job into an isolated restore
// database. A conflicting ID must never overwrite a different job or target.
func (r *SQLiteRepository) ImportAPTRecoveryEvidence(record Record) error {
	existing, err := r.Get(record.ID)
	if err == nil && (existing.APTTarget != record.APTTarget || existing.Kind != record.Kind ||
		existing.CreatedAt != record.CreatedAt) {
		return ErrAPTRecoveryEvidenceConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return r.upsert(record, true)
}
