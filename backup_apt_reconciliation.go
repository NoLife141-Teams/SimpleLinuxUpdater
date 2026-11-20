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
	live, err := beginAPTRestoreSnapshot(ctx, liveDB)
	if err != nil {
		return err
	}
	defer live.Rollback()
	evidence, err := captureAPTRestoreEvidence(ctx, live)
	if err != nil {
		return err
	}
	return evidence.compare(ctx, stagedDB, now, live)
}

func previewAPTRestore(ctx context.Context, deps AppDeps, stagedDB *sql.DB, now string) error {
	evidence, err := captureAPTPreviewEvidence(ctx, deps)
	if err != nil || (len(evidence.open) == 0 && len(evidence.repairs) == 0 && len(evidence.legacy) == 0) {
		return err
	}
	return evidence.compare(ctx, stagedDB, now, nil)
}

func captureAPTPreviewEvidence(ctx context.Context, deps AppDeps) (aptRestoreEvidence, error) {
	// Keep the live connection and maintenance lease only while reading the
	// snapshot. Archive work can be slow and uses its own isolated database.
	lease, _, stable := deps.MaintenanceCoordinator.TryProbe()
	if !stable {
		return aptRestoreEvidence{}, errors.New("live APT evidence is unavailable during maintenance")
	}
	defer lease.Close()
	live, err := beginAPTRestoreSnapshot(ctx, deps.DB())
	if err != nil {
		return aptRestoreEvidence{}, err
	}
	defer live.Rollback()
	return captureAPTRestoreEvidence(ctx, live)
}

func beginAPTRestoreSnapshot(ctx context.Context, liveDB *sql.DB) (*sql.Tx, error) {
	if liveDB == nil {
		return nil, errors.New("live APT evidence is unavailable")
	}
	return liveDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
}

type aptRestoreEvidence struct {
	open    []jobs.Record
	repairs []jobs.Record
	legacy  []legacyAPTIncidentEvidence
	targets aptRestoreTargets
}

func captureAPTRestoreEvidence(ctx context.Context, live *sql.Tx) (aptRestoreEvidence, error) {
	open, err := jobs.ListUnresolvedAPTJobs(ctx, live)
	if err != nil {
		return aptRestoreEvidence{}, err
	}
	repairs, err := jobs.LatestVerifiedAPTRepairs(ctx, live)
	if err != nil {
		return aptRestoreEvidence{}, err
	}
	legacy, err := captureLegacyAPTIncidents(ctx, live)
	if err != nil {
		return aptRestoreEvidence{}, err
	}
	evidence := aptRestoreEvidence{open: open, repairs: repairs, legacy: legacy}
	if len(open) != 0 || len(repairs) != 0 {
		evidence.targets, err = aptRestoreInventory(ctx, live)
	}
	return evidence, err
}

func (evidence aptRestoreEvidence) compare(ctx context.Context, stagedDB *sql.DB, now string, logSource *sql.Tx) error {
	staged := jobs.NewSQLiteRepository(stagedDB)
	if err := staged.MarkInterruptedAPTMutations(ctx, now); err != nil {
		return err
	}
	legacyConflicts, err := preserveLegacyAPTIncidents(ctx, evidence.legacy, stagedDB, logSource)
	if err != nil {
		return err
	}
	if len(evidence.open) == 0 && len(evidence.repairs) == 0 {
		return sortedAPTRestoreConflicts(legacyConflicts)
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
		record.ServerName = target.Name
		err := staged.ImportAPTRecoveryEvidence(ctx, record, logSource)
		if errors.Is(err, jobs.ErrAPTRecoveryEvidenceConflict) {
			if original, found := evidence.targets.byTarget[record.APTTarget]; found {
				target = original
			}
			addConflict(record, target, "apt_evidence_conflict", "The archive contains conflicting APT job evidence for this server.")
			return nil
		}
		return err
	}

	for _, incident := range evidence.open {
		server, matches := stagedTargets.byTarget[incident.APTTarget]
		if !matches {
			if original, attached := evidence.targets.byTarget[incident.APTTarget]; attached {
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
	for _, repair := range evidence.repairs {
		if server, matches := stagedTargets.byTarget[repair.APTTarget]; matches {
			if err := importEvidence(repair, server); err != nil {
				return err
			}
		}
	}
	remaining, err := staged.ListReconciliationRequiredContext(ctx)
	if err != nil {
		return err
	}
	for _, incident := range remaining {
		delete(importedOpen, incident.ID)
	}
	for _, incident := range importedOpen {
		server, liveMatch := evidence.targets.byTarget[incident.APTTarget]
		if !liveMatch {
			server = stagedTargets.byTarget[incident.APTTarget]
		}
		addConflict(incident, server, "apt_evidence_conflict", "The archive contradicts the current APT reconciliation state for this server.")
	}
	result := legacyConflicts
	for _, conflict := range conflicts {
		result = append(result, conflict)
	}
	return sortedAPTRestoreConflicts(result)
}

func sortedAPTRestoreConflicts(result aptRestoreConflicts) error {
	if len(result) == 0 {
		return nil
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
