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

// SuccessfulAPTRepairAfter returns only explicit repairs with recorded host
// key proof. A completed repair also resolves its own in-flight archive copy.
func (r *SQLiteRepository) SuccessfulAPTRepairAfter(incident Record) (string, error) {
	var id string
	err := r.db.QueryRow(`
		SELECT id FROM jobs
		WHERE apt_target = ? AND apt_target <> '' AND kind = ? AND status = ?
		  AND apt_mutation_host_key <> '' AND (created_at > ? OR id = ?)
		ORDER BY created_at DESC, id DESC LIMIT 1
	`, incident.APTTarget, KindAptRepair, StatusSucceeded, incident.CreatedAt, incident.ID).Scan(&id)
	return id, err
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
