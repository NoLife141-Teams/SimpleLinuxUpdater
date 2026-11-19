package main

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"debian-updater/internal/jobs"
)

type aptRestoreConflict struct {
	Code      string `json:"code"`
	Server    string `json:"server"`
	Message   string `json:"message"`
	ReportURL string `json:"report_url,omitempty"`
	jobID     string
}

type aptRestoreConflicts []aptRestoreConflict

func (aptRestoreConflicts) Error() string { return jobs.ErrAPTRecoveryEvidenceConflict.Error() }
func (aptRestoreConflicts) Unwrap() error { return jobs.ErrAPTRecoveryEvidenceConflict }

// prepareAPTRestore applies the same reconciliation checks as verification.
// Only the isolated replacement database is modified.
func prepareAPTRestore(ctx context.Context, liveDB, stagedDB *sql.DB, now string) error {
	return reconcileAPTRestore(ctx, liveDB, stagedDB, now, true)
}

func reconcileAPTRestore(ctx context.Context, liveDB, stagedDB *sql.DB, now string, includeLogs bool) error {
	if liveDB == nil {
		return errors.New("live APT evidence is unavailable")
	}
	live, err := liveDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer live.Rollback()
	staged := jobs.NewSQLiteRepository(stagedDB)
	open, err := jobs.ListUnresolvedAPTJobs(ctx, live)
	if err != nil {
		return err
	}
	if err := staged.MarkInterruptedAPTMutations(now); err != nil {
		return err
	}
	repairs, err := jobs.LatestVerifiedAPTRepairs(ctx, live)
	if err != nil || (len(open) == 0 && len(repairs) == 0) {
		return err
	}
	liveTargets, err := aptRestoreInventory(ctx, live)
	if err != nil {
		return err
	}
	stagedTargets, err := aptRestoreInventory(ctx, stagedDB)
	if err != nil {
		return err
	}

	latestOpen := make(map[string]string)
	importedOpen := make(map[string]jobs.Record)
	conflicts := make(map[string]aptRestoreConflict)
	addConflict := func(record jobs.Record, target Server, code, message string) {
		if _, exists := conflicts[record.APTTarget]; !exists {
			conflicts[record.APTTarget] = aptRestoreConflict{Code: code, Server: target.Name, Message: message, jobID: record.ID}
		}
	}
	importEvidence := func(record jobs.Record, target Server) error {
		err := staged.ImportAPTRecoveryEvidence(ctx, live, record.ID, target.Name, includeLogs)
		if errors.Is(err, jobs.ErrAPTRecoveryEvidenceConflict) {
			if original, found := liveTargets.byTarget[record.APTTarget]; found {
				target = original
			}
			addConflict(record, target, "apt_evidence_conflict", "The archive contains conflicting APT job evidence for this server.")
			return nil
		}
		return err
	}

	for _, incident := range open {
		server, matches := stagedTargets.byTarget[incident.APTTarget]
		if !matches {
			if original, attached := liveTargets.byTarget[incident.APTTarget]; attached {
				if stagedTargets.names[original.Name] || stagedTargets.identities[original.Identity] {
					addConflict(incident, original, "apt_target_changed", "The archive changes the identity or SSH address of this server.")
				} else {
					addConflict(incident, original, "apt_target_missing", "The archive omits this server with an open APT incident.")
				}
			}
			continue
		}
		if err := importEvidence(incident, server); err != nil {
			return err
		}
		// The latest evidence controls repair admission. Preserve earlier
		// unresolved jobs for their reports as well.
		if latest, seen := latestOpen[incident.APTTarget]; !seen || latest == incident.CreatedAt {
			latestOpen[incident.APTTarget] = incident.CreatedAt
			importedOpen[incident.ID] = incident
		}
	}
	for _, repair := range repairs {
		if server, matches := stagedTargets.byTarget[repair.APTTarget]; matches {
			if err := importEvidence(repair, server); err != nil {
				return err
			}
		}
	}
	remaining, err := staged.ListReconciliationRequired()
	if err != nil {
		return err
	}
	for _, incident := range remaining {
		delete(importedOpen, incident.ID)
	}
	for _, incident := range importedOpen {
		server, liveMatch := liveTargets.byTarget[incident.APTTarget]
		if !liveMatch {
			server = stagedTargets.byTarget[incident.APTTarget]
		}
		addConflict(incident, server, "apt_evidence_conflict", "The archive contradicts the current APT reconciliation state for this server.")
	}
	if len(conflicts) == 0 {
		return nil
	}
	result := make(aptRestoreConflicts, 0, len(conflicts))
	for _, conflict := range conflicts {
		result = append(result, conflict)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Server != result[j].Server {
			return result[i].Server < result[j].Server
		}
		return result[i].jobID < result[j].jobID
	})
	return result
}

type aptRestoreTargets struct {
	byTarget   map[string]Server
	names      map[string]bool
	identities map[string]bool
}

func aptRestoreInventory(ctx context.Context, db interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (aptRestoreTargets, error) {
	rows, err := db.QueryContext(ctx, `SELECT name, identity, host, port FROM servers`)
	if err != nil {
		return aptRestoreTargets{}, err
	}
	defer rows.Close()
	inventory := aptRestoreTargets{byTarget: map[string]Server{}, names: map[string]bool{}, identities: map[string]bool{}}
	for rows.Next() {
		var server Server
		if err := rows.Scan(&server.Name, &server.Identity, &server.Host, &server.Port); err != nil {
			return aptRestoreTargets{}, err
		}
		inventory.names[server.Name] = true
		inventory.identities[server.Identity] = true
		if target := server.APTTarget(); target != "" {
			inventory.byTarget[target] = server
		}
	}
	return inventory, rows.Err()
}
