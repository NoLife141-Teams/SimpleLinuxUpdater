package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	backuppkg "debian-updater/internal/backup"
	notificationpkg "debian-updater/internal/notifications"
)

type notificationFactCursor struct {
	Audit  int64
	Health int64
}

// The adapter reads accepted server facts, never browser projections. Audit and
// health cursors belong to Notification Delivery Lifecycle and commit with admission.
func newNotificationConditionSource(dbProvider func() *sql.DB, now func() time.Time) notificationpkg.ConditionSource {
	recovery := backuppkg.NewService(backuppkg.ServiceDeps{DB: dbProvider, Now: now})
	return func(ctx context.Context, cursorText string, previous []notificationpkg.Condition) (notificationpkg.ConditionBatch, error) {
		batch := notificationpkg.ConditionBatch{}
		db := dbProvider()
		if db == nil {
			return batch, fmt.Errorf("notification fact database unavailable")
		}
		cursor := notificationFactCursor{}
		restored := cursorText == "restore"
		batch.Rebased = restored
		if cursorText != "" && !restored {
			if err := json.Unmarshal([]byte(cursorText), &cursor); err != nil {
				return batch, err
			}
		}
		if restored {
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id),0) FROM audit_events`).Scan(&cursor.Audit); err != nil {
				return batch, err
			}
		}
		facts, auditID, err := notificationAuditFacts(ctx, db, cursor.Audit)
		if err != nil {
			return batch, err
		}
		batch.Facts = append(batch.Facts, facts...)
		if restored {
			restoredFacts, _, err := notificationAuditFacts(ctx, db, 0)
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
				if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notification_server_identities(server_name,identity) SELECT ?,? WHERE EXISTS(SELECT 1 FROM servers WHERE name=?)`, fact.TargetName, strings.TrimPrefix(fact.ID, "reboot/"), fact.TargetName); err != nil {
					return batch, err
				}
			}
		}
		if _, err := db.ExecContext(ctx, `INSERT OR IGNORE INTO notification_server_identities(server_name,identity) SELECT name,lower(hex(randomblob(16))) FROM servers WHERE name NOT IN (SELECT server_name FROM notification_server_identities)`); err != nil {
			return batch, err
		}
		identities, err := db.QueryContext(ctx, `SELECT n.server_name,n.identity FROM notification_server_identities n JOIN servers s ON s.name=n.server_name`)
		if err != nil {
			return batch, err
		}
		rebootIDs := map[string]string{}
		names := map[string]string{}
		for identities.Next() {
			var name, id string
			if err := identities.Scan(&name, &id); err != nil {
				_ = identities.Close()
				return batch, err
			}
			rebootIDs[name] = "reboot/" + id
			names["reboot/"+id] = name
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
			}
			batch.Facts = append(batch.Facts, fact)
		}

		healthFacts, healthID, err := notificationHealthFacts(ctx, db, cursor.Health, rebootIDs, restored)
		if err != nil {
			return batch, err
		}
		batch.Facts = append(batch.Facts, healthFacts...)
		cursor.Health = healthID

		// Resolved, cancelled and interrupted jobs cannot retain an actionable alert.
		approvals := map[string]notificationpkg.ConditionFact{}
		for _, condition := range previous {
			if condition.Fact.EventType == notificationpkg.EventApprovalPublished {
				approvals[condition.Fact.ID] = condition.Fact
			}
		}
		for _, fact := range batch.Facts {
			if fact.EventType == notificationpkg.EventApprovalPublished {
				approvals[fact.ID] = fact
			}
		}
		waiting, err := db.QueryContext(ctx, `SELECT id,server_name,meta_json,updated_at FROM jobs WHERE status='waiting_approval'`)
		if err != nil {
			return batch, err
		}
		for waiting.Next() {
			var id, name, raw, created string
			if err := waiting.Scan(&id, &name, &raw, &created); err != nil {
				_ = waiting.Close()
				return batch, err
			}
			var meta struct {
				Generation uint64 `json:"approval_generation"`
			}
			_ = json.Unmarshal([]byte(raw), &meta)
			if meta.Generation == 0 {
				continue
			}
			generation := strconv.FormatUint(meta.Generation, 10)
			safeMeta, _ := json.Marshal(map[string]any{"job_id": id, "approval_generation": meta.Generation})
			approvals["approval/"+id] = notificationpkg.ConditionFact{ID: "approval/" + id, EventType: notificationpkg.EventApprovalPublished, TargetType: "server", TargetName: name, Generation: generation, Active: true, CreatedAt: created, MetaJSON: string(safeMeta), Message: fmt.Sprintf("Approval pending for job %s, plan generation %s. Review the current plan in the authenticated Status page.", id, generation)}
		}
		if err := waiting.Err(); err != nil {
			_ = waiting.Close()
			return batch, err
		}
		_ = waiting.Close()
		for _, fact := range approvals {
			if !fact.Active {
				continue
			}
			var meta struct {
				JobID string `json:"job_id"`
			}
			_ = json.Unmarshal([]byte(fact.MetaJSON), &meta)
			var status, name string
			err := db.QueryRowContext(ctx, `SELECT status,server_name FROM jobs WHERE id=?`, meta.JobID).Scan(&status, &name)
			if err != nil && err != sql.ErrNoRows {
				return batch, err
			}
			var exists int
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM servers WHERE name=?`, name).Scan(&exists); err != nil {
				return batch, err
			}
			if status != "waiting_approval" || exists == 0 {
				fact.Active = false
			} else {
				fact.TargetName = name
			}
			batch.Facts = append(batch.Facts, fact)
		}
		health, err := recovery.RecoveryHealth(ctx)
		if err == nil && health.State != backuppkg.RecoveryStateUnavailable {
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

func notificationAuditFacts(ctx context.Context, db *sql.DB, after int64) ([]notificationpkg.ConditionFact, int64, error) {
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
		case notificationpkg.EventApprovalPublished, notificationpkg.EventApprovalExpired:
			if status != "success" || meta.JobID == "" || meta.Generation == 0 {
				continue
			}
			generation := strconv.FormatUint(meta.Generation, 10)
			safeMeta, _ := json.Marshal(map[string]any{"job_id": meta.JobID, "approval_generation": meta.Generation})
			fact := notificationpkg.ConditionFact{ID: "approval/" + meta.JobID, EventType: notificationpkg.EventApprovalPublished, TargetType: "server", TargetName: name, Generation: generation, Active: action == notificationpkg.EventApprovalPublished, CreatedAt: created, MetaJSON: string(safeMeta), Message: fmt.Sprintf("Approval pending for job %s, plan generation %s. Review the current plan in the authenticated Status page.", meta.JobID, generation)}
			facts = append(facts, fact)
			if action == notificationpkg.EventApprovalExpired {
				fact.ID += "/expired/" + generation
				fact.EventType = action
				fact.Active = true
				fact.OneShot = true
				fact.Message = fmt.Sprintf("Approval expired for job %s, plan generation %s.", meta.JobID, generation)
				facts = append(facts, fact)
			}
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

func notificationHealthFacts(ctx context.Context, db *sql.DB, after int64, identities map[string]string, restored bool) ([]notificationpkg.ConditionFact, int64, error) {
	query := `SELECT h.id,h.server_name,h.captured_at,h.reboot_required FROM server_health_snapshots h JOIN servers s ON s.name=h.server_name WHERE h.id>? AND h.reboot_required IS NOT NULL ORDER BY h.id`
	if restored {
		query = `SELECT h.id,h.server_name,h.captured_at,h.reboot_required FROM server_health_snapshots h JOIN servers s ON s.name=h.server_name WHERE h.reboot_required IS NOT NULL AND h.id=(SELECT h2.id FROM server_health_snapshots h2 WHERE h2.server_name=h.server_name AND h2.reboot_required IS NOT NULL ORDER BY h2.captured_at DESC,h2.id DESC LIMIT 1) AND h.id>? ORDER BY h.id`
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
