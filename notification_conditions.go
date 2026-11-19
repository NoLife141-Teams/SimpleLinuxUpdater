package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	backuppkg "debian-updater/internal/backup"
	"debian-updater/internal/jobs"
	notificationpkg "debian-updater/internal/notifications"
)

type notificationFactCursor struct {
	Audit  int64
	Health int64
}

type notificationFactQuerier interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// The adapter reads accepted server facts, never browser projections. Audit and
// health cursors belong to Notification Delivery Lifecycle and commit with admission.
func newNotificationConditionSource(dbProvider func() *sql.DB, now func() time.Time, origin *url.URL) notificationpkg.ConditionSource {
	recovery := backuppkg.NewService(backuppkg.ServiceDeps{DB: dbProvider, Now: now})
	return func(ctx context.Context, cursorText string, previous []notificationpkg.Condition) (notificationpkg.ConditionBatch, error) {
		batch := notificationpkg.ConditionBatch{}
		db := dbProvider()
		if db == nil {
			return batch, fmt.Errorf("notification fact database unavailable")
		}
		// Keep cursor reads, Server identities and the corresponding current job
		// and health facts in one SQLite snapshot. Mixing revisions can replay an
		// older approval generation or consume health under a temporary identity.
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return batch, err
		}
		defer func() { _ = tx.Rollback() }()
		cursor := notificationFactCursor{}
		restored := cursorText == "restore"
		batch.Rebased = restored
		if cursorText != "" && !restored {
			if err := json.Unmarshal([]byte(cursorText), &cursor); err != nil {
				return batch, err
			}
		}
		if restored {
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit_events`).Scan(&cursor.Audit); err != nil {
				return batch, err
			}
		}
		facts, auditID, err := notificationAuditFacts(ctx, tx, cursor.Audit)
		if err != nil {
			return batch, err
		}
		batch.Facts = append(batch.Facts, facts...)
		if restored {
			restoredFacts, _, err := notificationAuditFacts(ctx, tx, 0)
			if err != nil {
				return batch, err
			}
			latest := map[string]notificationpkg.ConditionFact{}
			for _, fact := range restoredFacts {
				if fact.ID == "backup/export" || fact.ID == "backup/verify" {
					latest[fact.ID] = fact
				}
			}
			for _, fact := range latest {
				batch.Facts = append(batch.Facts, fact)
			}
		}
		cursor.Audit = auditID

		// Stable Server identities are maintained by inventory rename/delete
		// transactions, independently of retained observations and restored row IDs.
		if restored {
			// Archives created before Server notification identities existed can inherit
			// the live identity for a matching name. Existing restored identities win.
			for _, condition := range previous {
				fact := condition.Fact
				if fact.EventType != notificationpkg.EventRebootRequired || !strings.HasPrefix(fact.ID, "reboot/") {
					continue
				}
				if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_server_identities(server_name,identity) SELECT ?,? WHERE EXISTS(SELECT 1 FROM servers WHERE name=?)`, fact.TargetName, strings.TrimPrefix(fact.ID, "reboot/"), fact.TargetName); err != nil {
					return batch, err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO notification_server_identities(server_name,identity) SELECT name,lower(hex(randomblob(16))) FROM servers WHERE name NOT IN (SELECT server_name FROM notification_server_identities)`); err != nil {
			return batch, err
		}
		identities, err := tx.QueryContext(ctx, `SELECT n.server_name,n.identity,COALESCE(e.changed_at,''),s.identity,s.host,s.port FROM notification_server_identities n JOIN servers s ON s.name=n.server_name LEFT JOIN server_health_endpoints e ON e.server_name=n.server_name`)
		if err != nil {
			return batch, err
		}
		rebootIDs := map[string]string{}
		names := map[string]string{}
		endpointChanges := map[string]string{}
		aptInventory := map[string]Server{}
		for identities.Next() {
			var server Server
			var id, changedAt string
			if err := identities.Scan(&server.Name, &id, &changedAt, &server.Identity, &server.Host, &server.Port); err != nil {
				_ = identities.Close()
				return batch, err
			}
			rebootIDs[server.Name] = "reboot/" + id
			names["reboot/"+id] = server.Name
			endpointChanges[server.Name] = changedAt
			if target := server.APTTarget(); target != "" {
				aptInventory[target] = server
			}
		}
		if err := identities.Err(); err != nil {
			_ = identities.Close()
			return batch, err
		}
		_ = identities.Close()
		for _, condition := range previous {
			fact := condition.Fact
			if fact.EventType != notificationpkg.EventRebootRequired {
				continue
			}
			name, exists := names[fact.ID]
			if !exists {
				fact.Active = false
			} else {
				fact.TargetName = name
				observed, observedErr := time.Parse(time.RFC3339Nano, fact.CreatedAt)
				changed, changedErr := time.Parse(time.RFC3339Nano, endpointChanges[name])
				if observedErr == nil && changedErr == nil && observed.Before(changed) {
					// An inventory endpoint replacement explicitly invalidates the
					// old host's evidence; this is not a failed or missing observation.
					fact.Active = false
					fact.CreatedAt = endpointChanges[name]
				}
			}
			batch.Facts = append(batch.Facts, fact)
		}

		healthFacts, healthID, err := notificationHealthFacts(ctx, tx, cursor.Health, rebootIDs, restored)
		if err != nil {
			return batch, err
		}
		batch.Facts = append(batch.Facts, healthFacts...)
		cursor.Health = healthID

		// Resolved, cancelled and interrupted jobs cannot retain an actionable alert.
		approvals := map[string]notificationpkg.ConditionFact{}
		for _, condition := range previous {
			if condition.Fact.EventType == notificationpkg.EventApprovalPublished {
				fact := condition.Fact
				fact.Active = false
				approvals[fact.ID] = fact
			}
		}
		waiting, err := tx.QueryContext(ctx, `SELECT j.id,j.server_name,j.meta_json,j.updated_at,s.identity
			FROM jobs j JOIN servers s ON s.name=j.server_name WHERE j.status='waiting_approval'`)
		if err != nil {
			return batch, err
		}
		for waiting.Next() {
			var id, name, raw, created, inventoryIdentity string
			if err := waiting.Scan(&id, &name, &raw, &created, &inventoryIdentity); err != nil {
				_ = waiting.Close()
				return batch, err
			}
			var meta struct {
				Generation     uint64 `json:"approval_generation"`
				ServerIdentity string `json:"server_identity"`
			}
			_ = json.Unmarshal([]byte(raw), &meta)
			if meta.Generation == 0 {
				continue
			}
			generation := strconv.FormatUint(meta.Generation, 10)
			link := approvalLink{ServerIdentity: meta.ServerIdentity, JobID: id, Generation: meta.Generation}
			conditionID := "approval/" + id
			active := meta.ServerIdentity == "" || meta.ServerIdentity == inventoryIdentity
			fact := notificationpkg.ConditionFact{ID: conditionID, EventType: notificationpkg.EventApprovalPublished, TargetType: "server", TargetName: name,
				ServerIdentity: meta.ServerIdentity, Generation: generation, Active: active, CreatedAt: created}
			safeMeta := map[string]any{"job_id": id, "approval_generation": meta.Generation}
			fact.Message = fmt.Sprintf("Approval pending for job %s, plan generation %s. Review the current plan in the authenticated Status page.", id, generation)
			if fact.Active && origin != nil && link.valid() {
				approvalURL := link.url(*origin)
				safeMeta["approval_url"] = approvalURL
				fact.Message = fmt.Sprintf("Approval pending for job %s, plan generation %s. Review the exact plan in Status: %s", id, generation, approvalURL)
			}
			encoded, _ := json.Marshal(safeMeta)
			fact.MetaJSON = string(encoded)
			approvals[conditionID] = fact
		}
		if err := waiting.Err(); err != nil {
			_ = waiting.Close()
			return batch, err
		}
		_ = waiting.Close()
		for _, fact := range approvals {
			batch.Facts = append(batch.Facts, fact)
		}
		aptFacts, err := notificationAPTReconciliationFacts(ctx, tx, aptInventory, previous)
		if err != nil {
			return batch, err
		}
		batch.Facts = append(batch.Facts, aptFacts...)
		if err := tx.Commit(); err != nil {
			return batch, err
		}
		// Recovery coverage has no cursor or Server identity dependency. Release
		// the connection before asking its service for this independent projection.
		health, err := recovery.RecoveryHealth(ctx)
		if err != nil || health.State == backuppkg.RecoveryStateUnavailable {
			batch.Incomplete = true
		} else {
			missing := health.Export.LastSuccessAt == "" || health.Verification.LastSuccessAt == ""
			stale := false
			for _, timestamp := range []string{health.Export.LastSuccessAt, health.Verification.LastSuccessAt} {
				when, parseErr := time.Parse(time.RFC3339, timestamp)
				checked, checkErr := time.Parse(time.RFC3339, health.CheckedAt)
				if parseErr == nil && checkErr == nil && checked.Sub(when) > time.Duration(health.StaleAfterHours)*time.Hour {
					stale = true
				}
			}
			batch.Facts = append(batch.Facts,
				notificationpkg.ConditionFact{ID: "backup/coverage/missing", EventType: notificationpkg.EventBackupCoverageMissing, TargetType: "backup", TargetName: "Application backup", Active: missing, Message: "No successful backup export and restore-ready verification have both been recorded."},
				notificationpkg.ConditionFact{ID: "backup/coverage/stale", EventType: notificationpkg.EventBackupCoverageStale, TargetType: "backup", TargetName: "Application backup", Active: !missing && stale, Message: fmt.Sprintf("Backup recovery evidence is older than %d hours.", health.StaleAfterHours)},
			)
		}
		raw, _ := json.Marshal(cursor)
		batch.Cursor = string(raw)
		return batch, nil
	}
}

func notificationAPTReconciliationFacts(ctx context.Context, tx *sql.Tx, inventory map[string]Server, previous []notificationpkg.Condition) ([]notificationpkg.ConditionFact, error) {
	incidents, err := jobs.ListAPTReconciliationIncidents(ctx, tx)
	if err != nil {
		return nil, err
	}
	facts := make([]notificationpkg.ConditionFact, 0, len(incidents))
	activeIDs := make(map[string]bool, len(incidents))
	for _, incident := range incidents {
		server, matches := inventory[incident.Target]
		if !matches {
			continue
		}
		id := notificationpkg.APTReconciliationConditionID(incident.Target, incident.JobID)
		activeIDs[id] = true
		meta, _ := json.Marshal(map[string]string{"job_id": incident.JobID})
		facts = append(facts, notificationpkg.ConditionFact{
			ID: id, EventType: notificationpkg.EventAPTReconciliationRequired,
			TargetType: "server", TargetName: server.Name, ServerIdentity: server.Identity,
			Active: true, CreatedAt: incident.CreatedAt, MetaJSON: string(meta),
			Message: "APT reconciliation is required before the next package operation. Review the incident in authenticated job history.",
		})
	}
	for _, condition := range previous {
		fact := condition.Fact
		if fact.EventType == notificationpkg.EventAPTReconciliationRequired && fact.Active && !activeIDs[fact.ID] {
			fact.Active = false
			facts = append(facts, fact)
		}
	}
	return facts, nil
}

func notificationAuditFacts(ctx context.Context, db notificationFactQuerier, after int64) ([]notificationpkg.ConditionFact, int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,created_at,action,target_name,status,meta_json FROM audit_events WHERE id>? ORDER BY id`, after)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	facts := []notificationpkg.ConditionFact{}
	for rows.Next() {
		var id int64
		var created, action, name, status, raw string
		if err := rows.Scan(&id, &created, &action, &name, &status, &raw); err != nil {
			return nil, after, err
		}
		after = id
		if _, err := time.Parse(time.RFC3339Nano, created); err != nil {
			continue
		}
		var meta struct {
			JobID        string `json:"job_id"`
			Generation   uint64 `json:"approval_generation"`
			RestoreReady *bool  `json:"restore_ready"`
		}
		_ = json.Unmarshal([]byte(raw), &meta)
		switch action {
		case notificationpkg.EventApprovalExpired:
			if status != "success" || meta.JobID == "" || meta.Generation == 0 {
				continue
			}
			generation := strconv.FormatUint(meta.Generation, 10)
			safeMeta, _ := json.Marshal(map[string]any{"job_id": meta.JobID, "approval_generation": meta.Generation})
			facts = append(facts, notificationpkg.ConditionFact{
				ID: "approval/" + meta.JobID + "/expired/" + generation, EventType: action, TargetType: "server", TargetName: name,
				Generation: generation, Active: true, OneShot: true, CreatedAt: created, MetaJSON: string(safeMeta),
				Message: fmt.Sprintf("Approval expired for job %s, plan generation %s.", meta.JobID, generation),
			})
		case "backup.export", "backup.verify", "backup.automatic.failed", "backup.automatic.succeeded":
			verify := action == "backup.verify"
			event, key, label := notificationpkg.EventBackupExportFailed, "backup/export", "Backup export failed. Review backup recovery health in the authenticated Administration page."
			if verify {
				event, key, label = notificationpkg.EventBackupVerifyFailed, "backup/verify", "Backup verification did not produce restore-ready evidence. Review backup recovery health in the authenticated Administration page."
			}
			failed := status != "success" || (verify && meta.RestoreReady != nil && !*meta.RestoreReady)
			facts = append(facts, notificationpkg.ConditionFact{ID: key, EventType: event, TargetType: "backup", TargetName: "Application backup", Active: failed, CreatedAt: created, Message: label})
		}
	}
	return facts, after, rows.Err()
}

func notificationHealthFacts(ctx context.Context, db notificationFactQuerier, after int64, identities map[string]string, restored bool) ([]notificationpkg.ConditionFact, int64, error) {
	// Keep historical snapshots for reports, but only current endpoint evidence
	// may establish or resolve a notification condition, including after restore.
	currentHealth := `WITH current_health AS (SELECT h.* FROM server_health_snapshots h WHERE NOT EXISTS (
		SELECT 1 FROM server_health_endpoints e WHERE e.server_name=h.server_name AND
		(h.endpoint<>e.endpoint OR julianday(h.captured_at)<julianday(e.changed_at)))) `
	query := currentHealth + `SELECT h.id,h.server_name,h.captured_at,h.reboot_required FROM current_health h JOIN servers s ON s.name=h.server_name WHERE h.id>? AND h.reboot_required IS NOT NULL ORDER BY h.id`
	if restored {
		query = currentHealth + `SELECT h.id,h.server_name,h.captured_at,h.reboot_required FROM current_health h JOIN servers s ON s.name=h.server_name WHERE h.reboot_required IS NOT NULL AND h.id=(SELECT h2.id FROM current_health h2 WHERE h2.server_name=h.server_name AND h2.reboot_required IS NOT NULL ORDER BY h2.captured_at DESC,h2.id DESC LIMIT 1) AND h.id>? ORDER BY h.id`
	}
	rows, err := db.QueryContext(ctx, query, after)
	if err != nil {
		return nil, after, err
	}
	defer rows.Close()
	facts := []notificationpkg.ConditionFact{}
	for rows.Next() {
		var id int64
		var name, created string
		var reboot bool
		if err := rows.Scan(&id, &name, &created, &reboot); err != nil {
			return nil, after, err
		}
		if id > after {
			after = id
		}
		key := identities[name]
		if key == "" {
			key = "reboot/" + name
			identities[name] = key
		}
		raw, _ := json.Marshal(map[string]any{"snapshot_id": id})
		facts = append(facts, notificationpkg.ConditionFact{ID: key, EventType: notificationpkg.EventRebootRequired, TargetType: "server", TargetName: name, Active: reboot, CreatedAt: created, MetaJSON: string(raw), Message: "A persisted host health observation confirms that a reboot is required."})
	}
	return facts, after, rows.Err()
}
