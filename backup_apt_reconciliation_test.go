package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/maintenance"
)

func TestAPTRestoreComparisonReportsCurrentTargetsAndContradictoryJobs(t *testing.T) {
	newDB := func(name string) *sql.DB {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), name))
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
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
	legacy := Server{Name: "legacy", Identity: "legacy-id", Host: "legacy.internal", Port: 22}
	insertServer(live, legacy)
	insertServer(staged, legacy)
	if _, err := live.Exec(`INSERT INTO jobs(id,kind,server_name,actor,status,error_class,created_at,updated_at)
		VALUES('legacy-job','update','legacy','test','failed','reconciliation_required',?,?)`, created, created); err != nil {
		t.Fatal(err)
	}
	if err := migrateLegacyAPTIncidents(live); err != nil {
		t.Fatal(err)
	}
	insertIncident(staged, "legacy-job", legacy)
	deps := AppDeps{
		DB:                     func() *sql.DB { return live },
		MaintenanceCoordinator: maintenance.NewCoordinator(maintenance.Deps{}),
	}
	t.Run("busy archive leaves live database available and honors cancellation", func(t *testing.T) {
		conn, err := staged.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		baseline := staged.Stats().WaitCount
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		review := make(chan error, 1)
		go func() { review <- previewAPTRestore(ctx, deps, staged, created) }()
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for staged.Stats().WaitCount == baseline {
			select {
			case err := <-review:
				t.Fatalf("preview finished before waiting for the archived database: %v", err)
			case <-ctx.Done():
				t.Fatal("preview did not reach the archived database")
			case <-ticker.C:
			}
		}
		if inUse := live.Stats().InUse; inUse != 0 {
			t.Errorf("preview holds %d live connections while waiting for the archived database", inUse)
		}
		lease, admission := deps.MaintenanceCoordinator.TryExclusive(maintenance.OperationBackupRestore)
		if !admission.Allowed {
			t.Error("preview probe still prevents exclusive admission while waiting for the archived database")
		} else if err := lease.Close(); err != nil {
			t.Fatal(err)
		}
		cancel()
		select {
		case err := <-review:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("preview cancellation error = %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("cancelling preview did not release its wait for the archived database")
		}
	})
	err := previewAPTRestore(t.Context(), deps, staged, created)
	var conflicts aptRestoreConflicts
	if !errors.As(err, &conflicts) || !errors.Is(err, jobs.ErrAPTRecoveryEvidenceConflict) || len(conflicts) != 4 {
		t.Fatalf("review conflicts = %+v, %v", conflicts, err)
	}
	for i, code := range []string{"apt_target_changed", "apt_evidence_conflict", "apt_evidence_conflict", "apt_target_missing"} {
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
