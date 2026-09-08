package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

const restoreCrashTestExit = 86

func TestRestoreCrashBeforeHandoffRetainsMaintenance(t *testing.T) {
	if dir := os.Getenv("SLU_RESTORE_CRASH_TEST_DIR"); dir != "" {
		runRestoreCrashChild(t, dir, os.Getenv("SLU_RESTORE_CRASH_TEST_POINT"))
		return
	}
	key := []byte("01234567890123456789012345678901")
	original := buildBackupDatabaseDataWithKey(t, key, Server{Name: "original", Host: "old.example", Port: 22, User: "root"}, "")
	replacement := buildBackupDatabaseDataWithKey(t, key, Server{Name: "replacement", Host: "new.example", Port: 22, User: "root"}, "")
	config, err := json.Marshal(map[string]string{"encryption_key": base64.StdEncoding.EncodeToString(key)})
	if err != nil {
		t.Fatal(err)
	}
	tarData, err := buildBackupTarGz(map[string][]byte{"servers.db": replacement, "config.json": config, "known_hosts": []byte("replacement trust")})
	if err != nil {
		t.Fatal(err)
	}
	archive, err := encryptBackupPayload(tarData, "very-strong-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, point := range []string{"before_known_hosts", "after_handoff", "complete", "marker_write_failed"} {
		t.Run(point, func(t *testing.T) {
			dir := t.TempDir()
			caseArchive := archive
			if point == "marker_write_failed" {
				source := filepath.Join(dir, "archive-source.db")
				if err := os.WriteFile(source, replacement, 0600); err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", source)
				if err != nil {
					t.Fatal(err)
				}
				_, writeErr := db.Exec(`CREATE TRIGGER reject_restore_marker BEFORE INSERT ON settings
					WHEN NEW.key = 'maintenance_state' BEGIN SELECT RAISE(ABORT, 'injected marker write failure'); END`)
				if err := errors.Join(writeErr, db.Close()); err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				tarData, err := buildBackupTarGz(map[string][]byte{"servers.db": data, "config.json": config, "known_hosts": []byte("replacement trust")})
				if err != nil {
					t.Fatal(err)
				}
				caseArchive, err = encryptBackupPayload(tarData, "very-strong-passphrase")
				if err != nil {
					t.Fatal(err)
				}
			}
			for name, data := range map[string][]byte{"servers.db": original, "known_hosts": []byte("original trust"), "archive.slubkp": caseArchive} {
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command(executable, "-test.run=^TestRestoreCrashBeforeHandoffRetainsMaintenance$")
			cmd.Env = append(os.Environ(), "SLU_RESTORE_CRASH_TEST_DIR="+dir, "SLU_RESTORE_CRASH_TEST_POINT="+point)
			output, err := cmd.CombinedOutput()
			if point == "complete" || point == "marker_write_failed" {
				if err != nil {
					t.Fatalf("restore child failed: %v\n%s", err, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != restoreCrashTestExit {
					t.Fatalf("child did not stop at replacement boundary: %v\n%s", err, output)
				}
			}
			db, err := sql.Open("sqlite", filepath.Join(dir, "servers.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var server string
			wantServer := "replacement"
			if point == "marker_write_failed" {
				wantServer = "original"
			}
			if err := db.QueryRow("SELECT name FROM servers LIMIT 1").Scan(&server); err != nil || server != wantServer {
				t.Fatalf("database after %s: server=%q, want %q: %v", point, server, wantServer, err)
			}
			hosts, err := os.ReadFile(filepath.Join(dir, "known_hosts"))
			wantHosts := "replacement trust"
			if point == "before_known_hosts" || point == "marker_write_failed" {
				wantHosts = "original trust"
			}
			if err != nil || string(hosts) != wantHosts {
				t.Fatalf("unexpected trust file at crash boundary: %q %v", hosts, err)
			}
			coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.SQLiteStore{DB: func() *sql.DB { return db }}})
			if err := coordinator.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantBlocked := point == "before_known_hosts" || point == "after_handoff"
			if snapshot := coordinator.Snapshot(); snapshot.Active != wantBlocked || snapshot.RecoveryRequired != wantBlocked {
				t.Errorf("startup maintenance after %s: %+v, want blocked=%t", point, snapshot, wantBlocked)
			}
			for _, work := range []maintenancepkg.WorkClass{maintenancepkg.WorkInteractive, maintenancepkg.WorkScheduled} {
				lease, decision := coordinator.TryShared(work)
				if lease != nil {
					lease.Close()
				}
				if decision.Allowed == wantBlocked {
					t.Errorf("startup admitted=%t for %s after %s", decision.Allowed, work, point)
				}
			}
		})
	}
}

func runRestoreCrashChild(t *testing.T, dir, point string) {
	t.Helper()
	ctx := context.Background()
	key := []byte("01234567890123456789012345678901")
	target := filepath.Join(dir, "servers.db")
	knownHosts := filepath.Join(dir, "known_hosts")
	var db *sql.DB
	getDB := func() *sql.DB {
		if db == nil {
			var err error
			db, err = sql.Open("sqlite", target)
			if err != nil {
				t.Fatal(err)
			}
		}
		return db
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	jm := newJobManagerWithRuntime(getDB(), nil, newServerState(), func() bool { return false })
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.SQLiteStore{DB: getDB}})
	if err := coordinator.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	lease, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("restore was not admitted")
	}
	service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		DBPath: func() string { return target }, CurrentEncryptionKey: func() []byte { return key }, TempDir: func() string { return dir },
		KnownHostsWritePath: func() (string, error) { return knownHosts, nil },
		EnsurePrivateDirForFile: func(path string) error {
			if path == knownHosts && point == "before_known_hosts" {
				os.Exit(restoreCrashTestExit)
			}
			return nil
		},
		RestoredRuntime: restoredRuntimeAdapter{
			prepare: func() {
				if db != nil {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					db = nil
				}
			},
			reload: func(context.Context) error {
				if point == "after_handoff" {
					os.Exit(restoreCrashTestExit)
				}
				jm = newJobManagerWithRuntime(getDB(), nil, newServerState(), func() bool { return false })
				return nil
			},
		},
	})
	lifecycle := newBackupOperationLifecycleWithDeps(backupOperationLifecycleDeps{
		Archive: service, CurrentJobManager: func() *JobManager { return jm }, ActiveServerActionNames: func() []string { return nil },
		EnsureEncryptionKey: func() []byte { return key }, RecordAudit: func(backupOperationAuditRecord) {},
	})
	outcome := lifecycle.Restore(ctx, backupRestoreCommand{Actor: "admin", Passphrase: "very-strong-passphrase", File: internalbackup.TemporaryFile{Path: filepath.Join(dir, "archive.slubkp")}, Lease: lease})
	if point == "marker_write_failed" {
		if outcome.Kind != backupOperationRestoreApplyFailed || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "injected marker write failure") {
			t.Fatalf("expected staged marker persistence failure: %+v", outcome)
		}
		return
	}
	if outcome.Kind != backupOperationSucceeded {
		t.Fatalf("restore failed: %+v", outcome)
	}
}
