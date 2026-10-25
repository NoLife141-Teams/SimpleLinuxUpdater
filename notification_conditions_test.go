package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	notificationpkg "debian-updater/internal/notifications"
)

func conditionSourceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "facts.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	return db
}
func appendConditionAudit(t *testing.T, db *sql.DB, at, action, status, meta string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO audit_events(created_at,actor,action,target_type,target_name,status,message,meta_json,request_id,client_ip) VALUES(?,'test',?,'backup','test',?,'',?,'','')`, at, action, status, meta)
	if err != nil {
		t.Fatal(err)
	}
}
func lastConditionFact(facts []notificationpkg.ConditionFact, id string) notificationpkg.ConditionFact {
	var found notificationpkg.ConditionFact
	for _, fact := range facts {
		if fact.ID == id {
			found = fact
		}
	}
	return found
}
func conditionPrevious(facts []notificationpkg.ConditionFact) []notificationpkg.Condition {
	byID := map[string]notificationpkg.Condition{}
	for _, fact := range facts {
		byID[fact.ID] = notificationpkg.Condition{Fact: fact, Episode: 1}
	}
	out := []notificationpkg.Condition{}
	for _, value := range byID {
		out = append(out, value)
	}
	return out
}

func TestNotificationBackupFactsManualAutomaticCoverageAndUnavailable(t *testing.T) {
	db := conditionSourceDB(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now })
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !lastConditionFact(batch.Facts, "backup/coverage/missing").Active {
		t.Fatal("missing backup not detected")
	}
	appendConditionAudit(t, db, now.Format(time.RFC3339), "backup.export", "failure", `{"password":"do-not-forward"}`)
	appendConditionAudit(t, db, now.Format(time.RFC3339), "backup.automatic.failed", "failure", `{"occurrence_at":"now"}`)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if !lastConditionFact(batch.Facts, "backup/export").Active {
		t.Fatal("automatic and manual exports not consumed")
	}
	raw, _ := json.Marshal(batch.Facts)
	if strings.Contains(string(raw), "do-not-forward") {
		t.Fatal("secret escaped audit allowlist")
	}
	appendConditionAudit(t, db, now.Format(time.RFC3339), "backup.export", "success", `{}`)
	appendConditionAudit(t, db, now.Format(time.RFC3339), "backup.verify", "success", `{"restore_ready":false}`)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if !lastConditionFact(batch.Facts, "backup/verify").Active || !lastConditionFact(batch.Facts, "backup/coverage/missing").Active {
		t.Fatal("non-ready verification treated as recovery")
	}
	appendConditionAudit(t, db, now.Format(time.RFC3339), "backup.verify", "success", `{"restore_ready":true}`)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if lastConditionFact(batch.Facts, "backup/coverage/missing").Active || lastConditionFact(batch.Facts, "backup/verify").Active {
		t.Fatal("healthy recovery did not resolve")
	}
	now = now.Add(8 * 24 * time.Hour)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if !lastConditionFact(batch.Facts, "backup/coverage/stale").Active {
		t.Fatal("stale evidence not detected")
	}
	appendConditionAudit(t, db, "invalid-time", "backup.export", "success", `{}`)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range batch.Facts {
		if strings.HasPrefix(fact.ID, "backup/coverage") {
			t.Fatal("unavailable recovery projected a resolution")
		}
	}
}

func TestNotificationApprovalGenerationExpirationCancelAndRestart(t *testing.T) {
	db := conditionSourceDB(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	at := now.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('test','unused.invalid','root',''); INSERT INTO jobs(id,kind,server_name,actor,status,meta_json,created_at,updated_at) VALUES('job','update','test','tester','waiting_approval','{"approval_generation":2}','2026-10-25T12:00:00Z','2026-10-25T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now })
	// Persisted job recovers publication even when its best-effort audit failed.
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	fact := lastConditionFact(batch.Facts, "approval/job")
	if !fact.Active || fact.Generation != "2" || fact.TargetName != "test" {
		t.Fatalf("%+v", fact)
	}
	appendConditionAudit(t, db, at, "approval.expired", "success", `{"job_id":"job","approval_generation":2}`)
	if _, err := db.Exec(`UPDATE jobs SET status='cancelled' WHERE id='job'`); err != nil {
		t.Fatal(err)
	}
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if lastConditionFact(batch.Facts, "approval/job").Active || !lastConditionFact(batch.Facts, "approval/job/expired/2").Active {
		t.Fatal("expiration lost identity or retained approval")
	}
	restart := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now })
	batch, err = restart(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	for _, fact := range batch.Facts {
		if fact.EventType == notificationpkg.EventApprovalExpired {
			t.Fatal("restart replayed expiry")
		}
	}
}

func TestNotificationRebootUnknownRenameDeleteAndRestoreIDCollision(t *testing.T) {
	db := conditionSourceDB(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if _, err := db.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('first','unused.invalid','root',''); INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required) VALUES('first','2026-10-25T12:00:00Z',1)`); err != nil {
		t.Fatal(err)
	}
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now })
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var rebootID string
	for _, fact := range batch.Facts {
		if fact.EventType == notificationpkg.EventRebootRequired {
			rebootID = fact.ID
		}
	}
	if !lastConditionFact(batch.Facts, rebootID).Active {
		t.Fatal("confirmed reboot missing")
	}
	if _, err := db.Exec(`INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required) VALUES('first','2026-10-25T13:00:00Z',NULL); UPDATE servers SET name='renamed' WHERE name='first'; UPDATE notification_server_identities SET server_name='renamed' WHERE server_name='first'; UPDATE server_health_snapshots SET server_name='renamed' WHERE server_name='first'`); err != nil {
		t.Fatal(err)
	}
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	renamed := lastConditionFact(batch.Facts, rebootID)
	if !renamed.Active || renamed.TargetName != "renamed" {
		t.Fatalf("rename/unknown lost condition: %+v", renamed)
	}
	// A restored database may reuse a snapshot ID for a completely different Server.
	if _, err := db.Exec(`UPDATE server_health_snapshots SET server_name='unrelated'; INSERT INTO servers(name,host,user,pass_enc) VALUES('unrelated','unused.invalid','root','')`); err != nil {
		t.Fatal(err)
	}
	restored, err := source(context.Background(), "restore", conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	second, err := source(context.Background(), restored.Cursor, conditionPrevious(restored.Facts))
	if err != nil {
		t.Fatal(err)
	}
	original := lastConditionFact(second.Facts, rebootID)
	if original.TargetName != "renamed" {
		t.Fatalf("restore ID collision migrated condition: %+v", original)
	}
	if _, err := db.Exec(`DELETE FROM servers WHERE name='renamed'`); err != nil {
		t.Fatal(err)
	}
	batch, err = source(context.Background(), batch.Cursor, []notificationpkg.Condition{{Fact: renamed, Episode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	// A non-restore collision is deliberately not synthesized here: real IDs cannot
	// be reused during normal inventory operations. Delete accepted snapshots too.
	_ = batch
	if _, err := db.Exec(`DELETE FROM server_health_snapshots`); err != nil {
		t.Fatal(err)
	}
	batch, err = source(context.Background(), batch.Cursor, []notificationpkg.Condition{{Fact: renamed, Episode: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if lastConditionFact(batch.Facts, rebootID).Active {
		t.Fatal("deleted Server kept reminders")
	}
}

func TestNotificationRestoreReconcilesBackupFailuresWithoutHistoricalExpiry(t *testing.T) {
	db := conditionSourceDB(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	at := now.Format(time.RFC3339)
	appendConditionAudit(t, db, at, "backup.export", "success", `{}`)
	appendConditionAudit(t, db, at, "backup.verify", "success", `{"restore_ready":true}`)
	appendConditionAudit(t, db, at, "approval.expired", "success", `{"job_id":"old-job","approval_generation":1}`)
	previous := []notificationpkg.Condition{{Fact: notificationpkg.ConditionFact{ID: "backup/export", EventType: notificationpkg.EventBackupExportFailed, Active: true}, Episode: 1}, {Fact: notificationpkg.ConditionFact{ID: "backup/verify", EventType: notificationpkg.EventBackupVerifyFailed, Active: true}, Episode: 1}}
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now })
	batch, err := source(context.Background(), "restore", previous)
	if err != nil {
		t.Fatal(err)
	}
	if lastConditionFact(batch.Facts, "backup/export").Active || lastConditionFact(batch.Facts, "backup/verify").Active {
		t.Fatal("restored backup success retained live failure")
	}
	for _, fact := range batch.Facts {
		if fact.EventType == notificationpkg.EventApprovalExpired {
			t.Fatal("restore replayed historical expiration")
		}
	}
}
