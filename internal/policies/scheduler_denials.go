package policies

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
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
	persisted := false
	err := deps.Maintenance.WithDeniedPersistence(decision, func() error {
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
		err = store.SaveDenial(SchedulerDenial{
			Tick: tick, ObservedAt: observedAt,
			Fingerprint: fingerprint,
			Blocker:     blocker,
		})
		persisted = err == nil
		return err
	})
	if !persisted {
		// The in-memory fallback has no durable scheduling and inventory
		// proof. Keep the skip, without attributing its blocker to new state.
		s.clearMissedTickBlocker(tick)
	}
	return err
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

func (denial SchedulerDenial) recoveryRequest(current time.Time, fingerprint, schedulingFingerprint string) (ScheduleRequest, bool) {
	observedAt, blocker := denial.ObservedAt, denial.Blocker
	if fingerprint == "" || denial.Fingerprint != fingerprint {
		matchingSchedule := schedulingFingerprint != "" && strings.HasPrefix(denial.Fingerprint, schedulingFingerprint+":targets:")
		if !matchingSchedule || !denial.Tick.Equal(current) {
			// Ordinary historical reconstruction may still cover this slot
			// without attributing the obsolete observation.
			return ScheduleRequest{}, false
		}
		// The observation matches the scheduling rules, but its inventory
		// proof cannot establish the exact target or cause.
		observedAt, blocker = denial.Tick, maintenance.Blocker{}
	}
	if !observedAt.UTC().Truncate(time.Minute).Equal(denial.Tick) {
		observedAt, blocker = denial.Tick, maintenance.Blocker{}
	}
	return ScheduleRequest{Now: observedAt, MaintenanceActive: true, MaintenanceBlocker: blocker, Admitted: true}, true
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
