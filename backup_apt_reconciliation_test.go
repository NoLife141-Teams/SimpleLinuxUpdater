package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"debian-updater/internal/jobs"
)

func TestAPTRestoreComparisonReportsCurrentTargetsAndContradictoryJobs(t *testing.T) {
	newDB := func(name string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if err := ensureSchema(db); err != nil {
			t.Fatal(err)
		}
		return db
	}
	insertServer := func(db *sql.DB, server Server) {
		t.Helper()
		if _, err := db.Exec("INSERT INTO servers(name, identity, host, port, user, pass_enc) VALUES(?, ?, ?, ?, 'root', '')",
			server.Name, server.Identity, server.Host, server.Port); err != nil {
			t.Fatal(err)
		}
	}
	const created = "2026-11-19T01:00:00.000000000Z"
	insertIncident := func(db *sql.DB, id string, server Server) {
		t.Helper()
		if _, err := db.Exec(`INSERT INTO jobs(id, kind, server_name, actor, status, error_class, created_at, updated_at, apt_target)
			VALUES(?, 'update', ?, 'test', 'failed', 'reconciliation_required', ?, ?, ?)`,
			id, server.Name, created, created, server.APTTarget()); err != nil {
			t.Fatal(err)
		}
	}
	live, staged := newDB("live.db"), newDB("staged.db")
	for _, name := range []string{"missing", "changed", "contradicted", "renamed"} {
		server := Server{Name: name, Identity: name + "-id", Host: name + ".internal", Port: 22}
		insertServer(live, server)
		insertIncident(live, name+"-job", server)
		switch name {
		case "changed":
			server.Port = 2202
			insertServer(staged, server)
		case "contradicted":
			insertServer(staged, server)
			server.Identity = "different-job-target"
			insertIncident(staged, name+"-job", server)
		case "renamed":
			server.Name = "new-name"
			insertServer(staged, server)
		}
	}
	err := reconcileAPTRestore(t.Context(), live, staged, created, false)
	var conflicts aptRestoreConflicts
	if !errors.As(err, &conflicts) || !errors.Is(err, jobs.ErrAPTRecoveryEvidenceConflict) || len(conflicts) != 3 {
		t.Fatalf("review conflicts = %+v, %v", conflicts, err)
	}
	for i, code := range []string{"apt_target_changed", "apt_evidence_conflict", "apt_target_missing"} {
		if conflict := conflicts[i]; conflict.Code != code || conflict.jobID != conflict.Server+"-job" || conflict.ReportURL != "" {
			t.Fatalf("unexpected conflict %d: %+v", i, conflict)
		}
	}
	stagedRenamed, err := jobs.NewSQLiteRepository(staged).Get("renamed-job")
	if err != nil || stagedRenamed.ServerName != "new-name" {
		t.Fatalf("renamed target did not match: %+v, %v", stagedRenamed, err)
	}
	liveUnchanged, err := jobs.NewSQLiteRepository(live).Get("renamed-job")
	if err != nil || liveUnchanged.ServerName != "renamed" {
		t.Fatalf("review changed live evidence: %+v, %v", liveUnchanged, err)
	}
}
