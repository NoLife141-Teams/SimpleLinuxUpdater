package main

import (
	"context"
	"database/sql"
	"errors"

	"debian-updater/internal/jobs"
)

// Preserve live APT evidence in the isolated replacement database before the
// restore can change the running installation.
func prepareAPTRestore(ctx context.Context, liveDB, stagedDB *sql.DB, manager *JobManager, now string) error {
	live := jobs.NewSQLiteRepository(liveDB)
	staged := jobs.NewSQLiteRepository(stagedDB)
	open, err := live.ListReconciliationRequired()
	if err != nil {
		return err
	}
	if err := staged.MarkInterruptedAPTMutations(now); err != nil {
		return err
	}
	archived, err := staged.ListReconciliationRequired()
	if err != nil || (len(open) == 0 && len(archived) == 0) {
		return err
	}
	liveTargets, err := aptRestoreInventory(ctx, liveDB)
	if err != nil {
		return err
	}
	stagedTargets, err := aptRestoreInventory(ctx, stagedDB)
	if err != nil {
		return err
	}

	importJob := func(id, name string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		record, err := manager.GetJobWithLogs(id)
		if err != nil {
			return err
		}
		record.ServerName = name
		return staged.ImportAPTRecoveryEvidence(record)
	}
	liveOpenTargets := make(map[string]bool)
	importedOpen := make(map[string]bool)
	for _, incident := range open {
		name, matches := stagedTargets[incident.APTTarget]
		if !matches {
			if _, attached := liveTargets[incident.APTTarget]; attached {
				return jobs.ErrAPTRecoveryEvidenceConflict
			}
			continue
		}
		if err := importJob(incident.ID, name); err != nil {
			return err
		}
		liveOpenTargets[incident.APTTarget] = true
		importedOpen[incident.ID] = true
	}
	for _, incident := range archived {
		name, matches := stagedTargets[incident.APTTarget]
		if !matches || liveOpenTargets[incident.APTTarget] {
			continue
		}
		id, err := live.SuccessfulAPTRepairAfter(incident)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if err := importJob(id, name); err != nil {
			return err
		}
	}
	remaining, err := staged.ListReconciliationRequired()
	if err != nil {
		return err
	}
	for _, incident := range remaining {
		delete(importedOpen, incident.ID)
	}
	if len(importedOpen) != 0 {
		return jobs.ErrAPTRecoveryEvidenceConflict
	}
	return nil
}

func aptRestoreInventory(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, identity, host, port FROM servers`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make(map[string]string)
	for rows.Next() {
		var server Server
		if err := rows.Scan(&server.Name, &server.Identity, &server.Host, &server.Port); err != nil {
			return nil, err
		}
		if target := server.APTTarget(); target != "" {
			targets[target] = server.Name
		}
	}
	return targets, rows.Err()
}
