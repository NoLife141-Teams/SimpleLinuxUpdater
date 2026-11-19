package policies

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

type SchedulerDenial struct {
	Tick        time.Time
	ObservedAt  time.Time
	Fingerprint string
	Blocker     maintenance.Blocker
}

func (s *Service) recordSchedulerDenial(tick time.Time, decision maintenance.Decision, store SchedulerWatermarkStore) error {
	req := s.rememberMaintenanceDenial(tick, decision.Blocker)
	if store.SaveDenial == nil || store.LoadStateRevision == nil {
		return nil
	}
	deps := s.EnsureDeps()
	return deps.Maintenance.WithDeniedPersistence(decision, func() error {
		snapshot, revision, err := s.captureSchedulerState(store)
		if err != nil {
			return err
		}
		fingerprint, err := snapshot.fingerprint()
		if err != nil {
			return err
		}
		fingerprint, err = snapshot.denialFingerprint(fmt.Sprintf("%s:rev:%d", fingerprint, revision))
		if err != nil {
			return err
		}
		observedAt, blocker := req.Now, req.MaintenanceBlocker
		if blocker.Cause == "" || blocker != decision.Blocker {
			observedAt, blocker = deps.Now().UTC(), maintenance.Blocker{}
		}
		return store.SaveDenial(SchedulerDenial{
			Tick: tick, ObservedAt: observedAt,
			Fingerprint: fingerprint,
			Blocker:     blocker,
		})
	})
}

func (snapshot schedulerRecoverySnapshot) denialFingerprint(schedulingState string) (string, error) {
	if schedulingState == "" {
		return "", nil
	}
	targets := make(map[string][2]string, len(snapshot.Servers))
	for _, server := range snapshot.Servers {
		targets[server.Name] = [2]string{server.Identity, servers.NormalizeServerEndpoint(server.Host, server.Port).String()}
	}
	encoded, err := json.Marshal(targets)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s:targets:%x", schedulingState, sha256.Sum256(encoded)), nil
}

func (s *Service) recoverSchedulerDenials(denials []SchedulerDenial, current time.Time, fingerprint string, processed map[string]struct{}) (bool, error) {
	layout := s.EnsureDeps().TimestampLayout
	currentDenied := false
	for _, denial := range denials {
		if denial.Tick.Equal(current) {
			currentDenied = true
		}
		if fingerprint == "" || denial.Fingerprint != fingerprint {
			// The slot remains covered by ordinary historical reconstruction
			// where possible, without attributing the obsolete observation.
			continue
		}
		observedAt, blocker := denial.ObservedAt, denial.Blocker
		if !observedAt.UTC().Truncate(time.Minute).Equal(denial.Tick) {
			observedAt, blocker = denial.Tick, maintenance.Blocker{}
		}
		if err := s.ProcessDueSlot(ScheduleRequest{Now: observedAt, MaintenanceActive: true, MaintenanceBlocker: blocker, Admitted: true}); err != nil {
			return false, err
		}
		processed[MissedTickKey(denial.Tick, layout)] = struct{}{}
	}
	return currentDenied, nil
}

func ensureSchedulerDenials(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS update_policy_scheduler_denials (
		tick TEXT PRIMARY KEY,
		observed_at TEXT NOT NULL,
		state_fingerprint TEXT NOT NULL,
		cause TEXT NOT NULL,
		job_id TEXT NOT NULL
	)`)
	return err
}

func schedulerDenialMinute(t time.Time) string {
	return t.UTC().Truncate(time.Minute).Format(DefaultTimestampLayout)
}

func (r *SQLiteRepository) SaveSchedulerDenial(denial SchedulerDenial) error {
	db := r.database()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM update_policy_scheduler_denials WHERE tick < ?`,
		schedulerDenialMinute(denial.Tick.Add(-DefaultSchedulerRecoveryHorizon))); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO update_policy_scheduler_denials(tick, observed_at, state_fingerprint, cause, job_id)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(tick) DO NOTHING`,
		schedulerDenialMinute(denial.Tick), denial.ObservedAt.UTC().Format(DefaultTimestampLayout),
		denial.Fingerprint, denial.Blocker.Cause, denial.Blocker.JobID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *SQLiteRepository) LoadSchedulerDenials(from, through time.Time) ([]SchedulerDenial, error) {
	rows, err := r.database().Query(`SELECT tick, observed_at, state_fingerprint, cause, job_id
		FROM update_policy_scheduler_denials WHERE tick BETWEEN ? AND ? ORDER BY tick`,
		schedulerDenialMinute(from), schedulerDenialMinute(through))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var denials []SchedulerDenial
	for rows.Next() {
		var tick, observedAt string
		var denial SchedulerDenial
		if err := rows.Scan(&tick, &observedAt, &denial.Fingerprint, &denial.Blocker.Cause, &denial.Blocker.JobID); err != nil {
			return nil, err
		}
		denial.Tick, err = time.Parse(DefaultTimestampLayout, tick)
		if err != nil {
			return nil, err
		}
		denial.ObservedAt, err = time.Parse(DefaultTimestampLayout, observedAt)
		if err != nil {
			return nil, err
		}
		denials = append(denials, denial)
	}
	return denials, rows.Err()
}

func (r *SQLiteRepository) DeleteSchedulerDenialsBefore(tick time.Time) error {
	_, err := r.database().Exec(`DELETE FROM update_policy_scheduler_denials WHERE tick < ?`, schedulerDenialMinute(tick))
	return err
}

// ClearSchedulerDenials runs on the staged database before installing an archive.
func ClearSchedulerDenials(db *sql.DB) error {
	_, err := db.Exec(`DELETE FROM update_policy_scheduler_denials`)
	return err
}
