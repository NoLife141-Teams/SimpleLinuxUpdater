package main

import (
	"context"
	"database/sql"
	"net/url"

	auditpkg "debian-updater/internal/audit"
	"debian-updater/internal/jobs"
	serverpkg "debian-updater/internal/servers"
)

type legacyAPTIncident struct {
	jobID, name, identity, endpoint string
}

func preserveLegacyAPTIncidents(ctx context.Context, live *sql.Tx, staged *sql.DB) error {
	// The installed decision wins over an older archive. A resolved legacy
	// incident must not reappear, and a pending incident must keep its report.
	incidents, err := live.QueryContext(ctx, `SELECT job_id,server_name,identity,endpoint,resolved FROM legacy_apt_incidents`)
	if err != nil {
		return err
	}
	defer incidents.Close()
	stagedJobs := jobs.NewSQLiteRepository(staged)
	for incidents.Next() {
		var incident legacyAPTIncident
		var resolved bool
		if err := incidents.Scan(&incident.jobID, &incident.name, &incident.identity, &incident.endpoint, &resolved); err != nil {
			return err
		}
		if !resolved {
			record, err := jobs.ReadAPTRecoveryJob(ctx, live, incident.jobID)
			if err != nil {
				return err
			}
			if err := stagedJobs.ImportAPTRecoveryEvidence(ctx, record, live); err != nil {
				return err
			}
		}
		if _, err := staged.ExecContext(ctx, `INSERT INTO legacy_apt_incidents(job_id,server_name,identity,endpoint,resolved)
			VALUES(?,?,?,?,?) ON CONFLICT(job_id) DO UPDATE SET
				server_name=excluded.server_name,identity=excluded.identity,endpoint=excluded.endpoint,resolved=excluded.resolved`,
			incident.jobID, incident.name, incident.identity, incident.endpoint, resolved); err != nil {
			return err
		}
	}
	return incidents.Err()
}

func (i legacyAPTIncident) matches(server Server) bool {
	return i.name == server.Name || (i.identity != "" && i.identity == server.Identity) ||
		(i.endpoint != "" && i.endpoint == serverpkg.NormalizeServerEndpoint(server.Host, server.Port).String())
}

func (i legacyAPTIncident) canVerify(server Server) bool {
	return i.endpoint == "" || i.endpoint == serverpkg.NormalizeServerEndpoint(server.Host, server.Port).String()
}

func migrateLegacyAPTIncidents(db *sql.DB) error {
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		WITH unresolved AS (
			SELECT j.id, j.server_name, s.identity, s.host, s.port,
				row_number() OVER (PARTITION BY j.server_name ORDER BY j.created_at DESC, j.id DESC) AS position
			FROM jobs j LEFT JOIN servers s ON s.name=j.server_name
			WHERE j.apt_target='' AND j.server_name<>''
				AND j.kind IN ('update','autoremove','apt_repair')
				AND ((j.status='failed' AND j.error_class='reconciliation_required') OR
					(j.status='running' AND ((j.kind='update' AND j.phase='apt_upgrade') OR
						(j.kind='autoremove' AND j.phase='autoremove') OR (j.kind='apt_repair' AND j.phase='reconcile'))))
				AND NOT EXISTS (SELECT 1 FROM jobs newer
					WHERE newer.server_name=j.server_name AND newer.status='succeeded' AND newer.apt_target=''
						AND newer.kind IN ('update','autoremove','apt_repair') AND newer.created_at>j.created_at)
		)
		SELECT id,server_name,COALESCE(identity,''),COALESCE(host,''),COALESCE(port,0)
		FROM unresolved WHERE position=1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var incidents []legacyAPTIncident
	for rows.Next() {
		var incident legacyAPTIncident
		var host string
		var port int
		if err := rows.Scan(&incident.jobID, &incident.name, &incident.identity, &host, &port); err != nil {
			return err
		}
		if host != "" {
			incident.endpoint = serverpkg.NormalizeServerEndpoint(host, port).String()
		}
		incidents = append(incidents, incident)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, incident := range incidents {
		if _, err := tx.ExecContext(ctx, `INSERT INTO legacy_apt_incidents(job_id,server_name,identity,endpoint)
			VALUES(?,?,?,?) ON CONFLICT(job_id) DO NOTHING`, incident.jobID, incident.name, incident.identity, incident.endpoint); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func pendingLegacyAPTIncidents(ctx context.Context, db *sql.DB) ([]legacyAPTIncident, error) {
	rows, err := db.QueryContext(ctx, `SELECT job_id,server_name,identity,endpoint FROM legacy_apt_incidents
		WHERE resolved=0 ORDER BY job_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var incidents []legacyAPTIncident
	for rows.Next() {
		var incident legacyAPTIncident
		if err := rows.Scan(&incident.jobID, &incident.name, &incident.identity, &incident.endpoint); err != nil {
			return nil, err
		}
		incidents = append(incidents, incident)
	}
	return incidents, rows.Err()
}

func legacyAPTIncidentForServer(incidents []legacyAPTIncident, server Server) legacyAPTIncident {
	for _, incident := range incidents {
		if incident.matches(server) {
			return incident
		}
	}
	return legacyAPTIncident{}
}

func (i legacyAPTIncident) facts(facts restoreAPTHostFacts) restoreAPTHostFacts {
	facts.LegacyJobID = i.jobID
	facts.LegacyReportURL = "/api/reports/jobs/" + url.PathEscape(i.jobID)
	return facts
}

// Record the proof and its audit together so a partial write cannot lift the guard.
func (v *restoreAPTValidation) resolveLegacyIncident(ctx context.Context, service *auditpkg.Service, actor, ip string,
	server Server, incident legacyAPTIncident, observation restoreAPTObservation) error {
	tx, err := v.db().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE legacy_apt_incidents SET resolved=1 WHERE job_id=? AND resolved=0`, incident.jobID)
	if err != nil {
		return err
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return serverpkg.ErrRestoreAPTValidation
	}
	endpoint := serverpkg.NormalizeServerEndpoint(server.Host, server.Port).String()
	if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation_targets(endpoint) VALUES (?)
		ON CONFLICT(endpoint) DO NOTHING`, endpoint); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO restore_apt_validation(identity,target,host_key,result,checked_at,approved)
		VALUES (?,?,?,?,?,?) ON CONFLICT(identity) DO UPDATE SET target=excluded.target,host_key=excluded.host_key,
			result=excluded.result,checked_at=excluded.checked_at,approved=excluded.approved`,
		server.Identity, observation.target, observation.key, observation.result, observation.checkedAt, observation.approved); err != nil {
		return err
	}
	const action = "apt.legacy_incident.resolve"
	event := service.PrepareEvent("", actor, ip, action, "server", server.Name, "success",
		"Legacy APT incident resolved after operator confirmation and read-only validation",
		map[string]any{"job_id": incident.jobID, "server_identity": server.Identity, "host_key_fingerprint": observation.key})
	if err := auditpkg.WriteTx(tx, event); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	service.NotifyCommitted(action, event)
	return nil
}
