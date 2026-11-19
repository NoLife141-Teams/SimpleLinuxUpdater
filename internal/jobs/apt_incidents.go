package jobs

import (
	"context"
	"database/sql"
)

type APTReconciliationIncident struct {
	JobID     string
	Target    string
	CreatedAt string
}

// ListAPTReconciliationIncidents returns the originating unresolved mutation
// for each exact inventory target. Failed retries belong to the same incident.
func ListAPTReconciliationIncidents(ctx context.Context, tx *sql.Tx) ([]APTReconciliationIncident, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, apt_target, created_at FROM (
			SELECT candidate.id, candidate.apt_target, candidate.created_at,
			       row_number() OVER (PARTITION BY candidate.apt_target ORDER BY candidate.created_at, candidate.id) AS position
			  FROM jobs AS candidate
			`+unresolvedAPTJobPredicate+`
		) WHERE position = 1
	`, unresolvedAPTJobArgs()...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []APTReconciliationIncident
	for rows.Next() {
		var incident APTReconciliationIncident
		if err := rows.Scan(&incident.JobID, &incident.Target, &incident.CreatedAt); err != nil {
			return nil, err
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}
