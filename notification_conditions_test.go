package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	healthpkg "debian-updater/internal/health"
	notificationpkg "debian-updater/internal/notifications"

	"modernc.org/sqlite"
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

// Interleave a second SQLite connection immediately after one source query has
// finished, without relying on scheduling or sleeps.
type conditionReadConnector struct {
	base      driver.Driver
	path      string
	afterRead func(string)
}

func (c *conditionReadConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.base.Open(c.path)
	if err != nil {
		return nil, err
	}
	return &conditionReadConn{Conn: conn, afterRead: c.afterRead}, nil
}

func (c *conditionReadConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type conditionReadConn struct {
	driver.Conn
	afterRead func(string)
}

func (c *conditionReadConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		return nil, err
	}
	return &conditionReadRows{Rows: rows, afterClose: func() { c.afterRead(query) }}, nil
}

func (c *conditionReadConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
}

type conditionReadRows struct {
	driver.Rows
	afterClose func()
}

func (r *conditionReadRows) Close() error {
	err := r.Rows.Close()
	if r.afterClose != nil {
		r.afterClose()
		r.afterClose = nil
	}
	return err
}

func interleavedConditionDB(t *testing.T, afterRead func(string)) (*sql.DB, *sql.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "interleaved.db")
	writer, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	if _, err := writer.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		t.Fatal(err)
	}
	if err := ensureSchema(writer); err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(&conditionReadConnector{base: &sqlite.Driver{}, path: path, afterRead: afterRead})
	t.Cleanup(func() { _ = db.Close() })
	return db, writer
}

func TestNotificationSourceKeepsApprovalGenerationAndAuditCursorConsistent(t *testing.T) {
	var writer *sql.DB
	interleaved := false
	db, writeDB := interleavedConditionDB(t, func(query string) {
		if interleaved || !strings.Contains(query, "FROM audit_events WHERE id>?") {
			return
		}
		interleaved = true
		appendConditionAudit(t, writer, "2026-10-25T12:00:00Z", "approval.published", "success", `{"job_id":"job","approval_generation":1}`)
		if _, err := writer.Exec(`UPDATE jobs SET meta_json='{"approval_generation":2}' WHERE id='job'`); err != nil {
			t.Fatal(err)
		}
		appendConditionAudit(t, writer, "2026-10-25T12:00:01Z", "approval.published", "success", `{"job_id":"job","approval_generation":2}`)
	})
	writer = writeDB
	if _, err := writer.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('test','unused.invalid','root',''); INSERT INTO jobs(id,kind,server_name,actor,status,meta_json,created_at,updated_at) VALUES('job','update','test','tester','waiting_approval','{"approval_generation":1}','2026-10-25T12:00:00Z','2026-10-25T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	source := newNotificationConditionSource(func() *sql.DB { return db }, time.Now, nil, backupAutomationDeployment{})
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		// A concurrent write can invalidate SQLite's read-to-write upgrade. The
		// entire observation must then be retried with the unchanged cursor.
		batch, err = source(context.Background(), "", nil)
	}
	if err != nil {
		t.Fatal(err)
	}
	var cursor notificationFactCursor
	if err := json.Unmarshal([]byte(batch.Cursor), &cursor); err != nil {
		t.Fatal(err)
	}
	if !interleaved || lastConditionFact(batch.Facts, "approval/job").Generation != "2" || cursor.Audit != 2 {
		t.Fatalf("job generation advanced beyond consumed audits: cursor=%+v facts=%+v", cursor, batch.Facts)
	}
	appendConditionAudit(t, writer, "2026-10-25T12:00:02Z", "approval.published", "success", `{"job_id":"job","approval_generation":1}`)
	next, err := source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	var observed int
	for _, fact := range next.Facts {
		if fact.ID == "approval/job" {
			observed++
			if !fact.Active || fact.Generation != "2" {
				t.Fatalf("replayed an obsolete generation: %+v", fact)
			}
		}
	}
	if observed != 1 {
		t.Fatalf("expected exactly one current fact, got %d", observed)
	}
}

func TestNotificationApprovalLinkFollowsExactPlanAndStableInventory(t *testing.T) {
	db := conditionSourceDB(t)
	write := func(query string) {
		t.Helper()
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	write(`INSERT INTO servers(name,host,user,pass_enc,identity) VALUES('original','demo.invalid','root','','stable-host');
		INSERT INTO jobs(id,kind,server_name,actor,status,meta_json,created_at,updated_at)
		VALUES('job','update','original','tester','waiting_approval','{"approval_generation":1,"server_identity":"stable-host","password":"test-secret"}','2026-10-25T12:00:00Z','2026-10-25T12:00:00Z')`)
	origin, err := parseApplicationOrigin("https://updates.example")
	if err != nil {
		t.Fatal(err)
	}
	read := func(base *url.URL, cursor string, previous []notificationpkg.Condition) (notificationpkg.ConditionBatch, notificationpkg.ConditionFact) {
		t.Helper()
		source := newNotificationConditionSource(func() *sql.DB { return db }, time.Now, base, backupAutomationDeployment{})
		batch, err := source(t.Context(), cursor, previous)
		if err != nil {
			t.Fatal(err)
		}
		return batch, lastConditionFact(batch.Facts, "approval/job")
	}
	assertLink := func(fact notificationpkg.ConditionFact, name, generation string) {
		t.Helper()
		var meta struct {
			URL string `json:"approval_url"`
		}
		if err := json.Unmarshal([]byte(fact.MetaJSON), &meta); err != nil {
			t.Fatal(err)
		}
		want := "https://updates.example/?approval_generation=" + generation + "&job_id=job&server_identity=stable-host"
		if !fact.Active || fact.TargetName != name || meta.URL != want || !strings.Contains(fact.Message, want) || strings.Contains(fact.MetaJSON, "test-secret") {
			t.Fatalf("unexpected approval alert: %+v", fact)
		}
	}
	first, fact := read(origin, "", nil)
	assertLink(fact, "original", "1")
	_, textual := read(nil, first.Cursor, conditionPrevious(first.Facts))
	if !textual.Active || strings.Contains(textual.MetaJSON, "approval_url") || !strings.Contains(textual.Message, "authenticated Status page") {
		t.Fatalf("unconfigured origin changed the textual notification: %+v", textual)
	}

	write(`UPDATE servers SET name='renamed' WHERE name='original';
		UPDATE jobs SET server_name='renamed', meta_json='{"approval_generation":2,"server_identity":"stable-host"}' WHERE id='job'`)
	next, fact := read(origin, first.Cursor, conditionPrevious(first.Facts))
	assertLink(fact, "renamed", "2")
	write(`DELETE FROM servers WHERE name='renamed';
		INSERT INTO servers(name,host,user,pass_enc,identity) VALUES('renamed','replacement.invalid','root','','replacement-host')`)
	_, replaced := read(origin, next.Cursor, conditionPrevious(next.Facts))
	if replaced.Active || strings.Contains(replaced.MetaJSON, "approval_url") {
		t.Fatalf("same-name replacement received the old plan: %+v", replaced)
	}
}

func TestNotificationSourceKeepsServerIdentityConsistentDuringRename(t *testing.T) {
	var writer *sql.DB
	interleaved := false
	var renameErr error
	rename := func() error {
		_, err := writer.Exec(`BEGIN; UPDATE servers SET name='renamed' WHERE name='test'; UPDATE notification_server_identities SET server_name='renamed' WHERE server_name='test'; UPDATE server_health_snapshots SET server_name='renamed' WHERE server_name='test'; COMMIT`)
		if err != nil {
			_, _ = writer.Exec(`ROLLBACK`)
		}
		return err
	}
	db, writeDB := interleavedConditionDB(t, func(query string) {
		if interleaved || !strings.Contains(query, "SELECT n.server_name,n.identity") {
			return
		}
		interleaved = true
		renameErr = rename()
	})
	writer = writeDB
	if _, err := writer.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('test','unused.invalid','root',''); INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required) VALUES('test','2026-10-25T12:00:00Z',1)`); err != nil {
		t.Fatal(err)
	}
	source := newNotificationConditionSource(func() *sql.DB { return db }, time.Now, nil, backupAutomationDeployment{})
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var identity string
	if err := writer.QueryRow(`SELECT identity FROM notification_server_identities`).Scan(&identity); err != nil {
		t.Fatal(err)
	}
	if !interleaved || !lastConditionFact(batch.Facts, "reboot/"+identity).Active {
		t.Fatalf("rename detached confirmed health from the durable Server identity: %+v", batch.Facts)
	}
	if renameErr != nil {
		if err := rename(); err != nil {
			t.Fatal(err)
		}
	}
	next, err := source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	fact := lastConditionFact(next.Facts, "reboot/"+identity)
	if !fact.Active || fact.TargetName != "renamed" {
		t.Fatalf("rename lost the accepted reboot condition: %+v", fact)
	}
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
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
	batch, err := source(context.Background(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if batch.Incomplete || lastConditionFact(batch.Facts, "backup/coverage/missing").ID != "" {
		t.Fatal("unobserved local storage produced a coverage alert")
	}
	automation := backupAutomationState{Config: defaultBackupSchedule(),
		Observation: newBackupArchiveObservation(now, "available", "fixture", "fixture-directory")}
	saveAutomation := func(t *testing.T) {
		t.Helper()
		raw, err := json.Marshal(automation)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`,
			backupAutomationSettingsKey, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	saveAutomation(t)
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
	if !lastConditionFact(batch.Facts, "backup/coverage/missing").Active || lastConditionFact(batch.Facts, "backup/verify").Active {
		t.Fatal("unrelated export and verification evidence established local coverage")
	}
	automation.Archives = []ownedBackupArchive{{
		Name: "slu-auto-fixture-example.slubkp", Owner: "fixture", DirectoryIdentity: "fixture-directory",
		LastVerifiedAt: backupStamp(now), CapturedAt: backupStamp(now.Add(-8 * 24 * time.Hour)),
		Verification: "verified", Availability: "present", ObservedAt: backupStamp(now), ObservationProcess: backupArchiveObservationProcess,
	}}
	saveAutomation(t)
	batch, err = source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
	if err != nil {
		t.Fatal(err)
	}
	if !lastConditionFact(batch.Facts, "backup/coverage/stale").Active || lastConditionFact(batch.Facts, "backup/coverage/missing").Active {
		t.Fatal("stale capture with fresh verification did not replace the missing condition")
	}
	previous := conditionPrevious(batch.Facts)
	stale := lastConditionFact(batch.Facts, "backup/coverage/stale")
	stale.Suspended = true
	for _, tc := range []struct{ name, storage, storageProcess, archiveProcess string }{
		{"restart before the first scan", "available", "previous-process", "previous-process"},
		{"a partial scan has yet to observe the archive", "available", backupArchiveObservationProcess, "previous-process"},
		{"storage unavailable", "unavailable", backupArchiveObservationProcess, backupArchiveObservationProcess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			automation.Observation.Storage = tc.storage
			automation.Observation.Process = tc.storageProcess
			automation.Archives[0].ObservationProcess = tc.archiveProcess
			saveAutomation(t)
			observed, err := source(t.Context(), batch.Cursor, previous)
			if err != nil || observed.Incomplete || lastConditionFact(observed.Facts, "backup/coverage/stale") != stale {
				t.Fatalf("unknown local availability did not suspend the prior stale episode: %+v, %v", observed.Facts, err)
			}
		})
	}
	if _, err := db.Exec("UPDATE settings SET value='!' WHERE key=?", backupAutomationSettingsKey); err != nil {
		t.Fatal(err)
	}
	batch, err = source(context.Background(), batch.Cursor, previous)
	if err != nil {
		t.Fatal(err)
	}
	if !batch.Incomplete {
		t.Fatal("unreadable recovery evidence appeared to be a complete evaluation")
	}
	if unreadable := lastConditionFact(batch.Facts, "backup/coverage/stale"); unreadable != stale {
		t.Fatal("unreadable recovery evidence changed the suspended episode")
	}
}

func TestNotificationApprovalGenerationExpirationCancelAndRestart(t *testing.T) {
	db := conditionSourceDB(t)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	at := now.Format(time.RFC3339)
	if _, err := db.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('test','unused.invalid','root',''); INSERT INTO jobs(id,kind,server_name,actor,status,meta_json,created_at,updated_at) VALUES('job','update','test','tester','waiting_approval','{"approval_generation":2}','2026-10-25T12:00:00Z','2026-10-25T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
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
	restart := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
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
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
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
	source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
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

func TestNotificationRebootRetiresEvidenceFromPreviousEndpoint(t *testing.T) {
	for _, mode := range []string{"observed", "unobserved", "restore"} {
		t.Run(mode, func(t *testing.T) {
			db := conditionSourceDB(t)
			now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			if _, err := db.Exec(`INSERT INTO servers(name,host,user,pass_enc) VALUES('host','192.0.2.1','root',''); INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required) VALUES('host','2026-10-25T12:00:00Z',1)`); err != nil {
				t.Fatal(err)
			}
			source := newNotificationConditionSource(func() *sql.DB { return db }, func() time.Time { return now }, nil, backupAutomationDeployment{})
			cursor := ""
			var previous []notificationpkg.Condition
			if mode != "unobserved" {
				batch, err := source(context.Background(), cursor, previous)
				if err != nil {
					t.Fatal(err)
				}
				cursor, previous = batch.Cursor, conditionPrevious(batch.Facts)
			}
			now = now.Add(time.Hour)
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			observation := healthpkg.SQLiteObservation{Now: func() time.Time { return now }}
			if err := observation.InvalidateEndpointTx(tx, "host", "192.0.2.1:22", "192.0.2.2:22"); err != nil {
				t.Fatal(err)
			}
			if _, err := tx.Exec(`UPDATE servers SET host='192.0.2.2' WHERE name='host'`); err != nil {
				t.Fatal(err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if mode == "restore" {
				cursor = "restore"
			}
			batch, err := source(context.Background(), cursor, previous)
			if err != nil {
				t.Fatal(err)
			}
			for _, fact := range batch.Facts {
				if fact.EventType == notificationpkg.EventRebootRequired && fact.Active {
					t.Fatalf("replacement endpoint inherited old reboot evidence: %+v", fact)
				}
			}
			if _, err := db.Exec(`INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required,endpoint) VALUES('host','2026-10-25T13:01:00Z',1,'192.0.2.2:22')`); err != nil {
				t.Fatal(err)
			}
			next, err := source(context.Background(), batch.Cursor, conditionPrevious(batch.Facts))
			if err != nil {
				t.Fatal(err)
			}
			active := false
			for _, fact := range next.Facts {
				active = active || (fact.EventType == notificationpkg.EventRebootRequired && fact.Active)
			}
			if !active {
				t.Fatal("current endpoint's confirmed reboot evidence was suppressed")
			}
			if _, err := db.Exec(`INSERT INTO server_health_snapshots(server_name,captured_at,reboot_required,endpoint) VALUES('host','2026-10-25T13:02:00Z',NULL,'192.0.2.2:22')`); err != nil {
				t.Fatal(err)
			}
			unknown, err := source(context.Background(), next.Cursor, conditionPrevious(next.Facts))
			if err != nil {
				t.Fatal(err)
			}
			active = false
			for _, fact := range unknown.Facts {
				active = active || (fact.EventType == notificationpkg.EventRebootRequired && fact.Active)
			}
			if !active {
				t.Fatal("unknown observation cleared the current endpoint's confirmed condition")
			}
		})
	}
}

func TestAPTNotificationKeepsTheOriginalJobForTheExactTarget(t *testing.T) {
	db := conditionSourceDB(t)
	server := Server{Name: "node", Identity: "durable-identity", Host: "ssh.invalid", Port: 22}
	if _, err := db.Exec(`INSERT INTO servers(name,identity,host,port,user,pass_enc) VALUES(?,?,?,?,'root','')`,
		server.Name, server.Identity, server.Host, server.Port); err != nil {
		t.Fatal(err)
	}
	insert := func(id, kind, status, class, at, target string) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO jobs(id,kind,server_name,actor,status,error_class,created_at,updated_at,apt_target,logs_text)
			VALUES(?,?,'node','tester',?,?,?,?,?,'secret raw SSH output')`, id, kind, status, class, at, at, target); err != nil {
			t.Fatal(err)
		}
	}
	target := server.APTTarget()
	insert("original-job", "update", "failed", "reconciliation_required", "2026-10-25T12:00:00Z", target)
	insert("failed-repair", "apt_repair", "failed", "reconciliation_required", "2026-10-25T12:01:00Z", target)
	insert("unverified-legacy", "update", "failed", "reconciliation_required", "2026-10-25T11:00:00Z", "")
	source := newNotificationConditionSource(func() *sql.DB { return db }, time.Now, nil, backupAutomationDeployment{})
	ctx := t.Context()
	read := func(cursor string, previous []notificationpkg.Condition) notificationpkg.ConditionBatch {
		t.Helper()
		batch, err := source(ctx, cursor, previous)
		if err != nil {
			t.Fatal(err)
		}
		return batch
	}
	first := read("", nil)
	incidentID := notificationpkg.APTReconciliationConditionID(target, "original-job")
	original := lastConditionFact(first.Facts, incidentID)
	if !original.Active || original.TargetName != server.Name || original.ServerIdentity != server.Identity ||
		original.MetaJSON != `{"job_id":"original-job"}` || strings.Contains(original.Message, "secret") {
		t.Fatalf("unexpected original incident: %+v", original)
	}
	for _, fact := range first.Facts {
		if fact.EventType == notificationpkg.EventAPTReconciliationRequired && fact.ID != incidentID {
			t.Fatalf("retry or unverified job became its own alert: %+v", fact)
		}
	}
	if _, err := db.Exec(`UPDATE servers SET name='renamed' WHERE identity=?`, server.Identity); err != nil {
		t.Fatal(err)
	}
	renamed := read(first.Cursor, conditionPrevious(first.Facts))
	if fact := lastConditionFact(renamed.Facts, incidentID); !fact.Active || fact.TargetName != "renamed" {
		t.Fatalf("rename lost the exact incident: %+v", fact)
	}
	if _, err := db.Exec(`UPDATE servers SET port=2202 WHERE identity=?`, server.Identity); err != nil {
		t.Fatal(err)
	}
	replaced := read("restore", conditionPrevious(renamed.Facts))
	if fact := lastConditionFact(replaced.Facts, incidentID); fact.ID == "" || fact.Active {
		t.Fatalf("replacement did not retire original condition: %+v", fact)
	}
	if _, err := db.Exec(`UPDATE servers SET port=22 WHERE identity=?`, server.Identity); err != nil {
		t.Fatal(err)
	}
	insert("successful-repair", "apt_repair", "succeeded", "", "2026-10-25T12:02:00Z", target)
	insert("new-incident", "autoremove", "failed", "reconciliation_required", "2026-10-25T12:03:00Z", target)
	later := read(replaced.Cursor, conditionPrevious(renamed.Facts))
	if fact := lastConditionFact(later.Facts, incidentID); fact.ID == "" || fact.Active {
		t.Fatalf("repair left the old alert actionable: %+v", fact)
	}
	if fact := lastConditionFact(later.Facts, notificationpkg.APTReconciliationConditionID(target, "new-incident")); !fact.Active {
		t.Fatalf("distinct later incident was not admitted: %+v", fact)
	}
}
