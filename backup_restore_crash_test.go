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

func TestRestoreCrashBoundariesPreserveMaintenanceEvidence(t *testing.T) {
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
	for _, point := range []string{"before_marker", "before_known_hosts", "after_handoff", "after_durable_finalization", "after_marker_removal",
		"after_rollback_marker_removal", "complete", "cancel_after_reload", "marker_write_failed"} {
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
			cmd := exec.Command(executable, "-test.run=^TestRestoreCrashBoundariesPreserveMaintenanceEvidence$")
			cmd.Env = append(os.Environ(), "SLU_RESTORE_CRASH_TEST_DIR="+dir, "SLU_RESTORE_CRASH_TEST_POINT="+point)
			output, err := cmd.CombinedOutput()
			if point == "complete" || point == "cancel_after_reload" || point == "marker_write_failed" {
				if err != nil {
					t.Fatalf("restore child failed: %v\n%s", err, output)
				}
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) || exitErr.ExitCode() != restoreCrashTestExit {
					t.Fatalf("child did not stop at replacement boundary: %v\n%s", err, output)
				}
			}
			dbPath := filepath.Join(dir, "servers.db")
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var server string
			wantServer := "replacement"
			if point == "marker_write_failed" || point == "before_marker" || point == "after_rollback_marker_removal" {
				wantServer = "original"
			}
			if err := db.QueryRow("SELECT name FROM servers LIMIT 1").Scan(&server); err != nil || server != wantServer {
				t.Fatalf("database after %s: server=%q, want %q: %v", point, server, wantServer, err)
			}
			hosts, err := os.ReadFile(filepath.Join(dir, "known_hosts"))
			wantHosts := "replacement trust"
			if point == "before_known_hosts" || wantServer == "original" {
				wantHosts = "original trust"
			}
			if err != nil || string(hosts) != wantHosts {
				t.Fatalf("unexpected trust file at crash boundary: %q %v", hosts, err)
			}
			markerPending := func() (bool, error) { return internalbackup.RestoreRecoveryPending(dbPath) }
			wantBlocked := point == "before_known_hosts" || point == "after_handoff"
			if marked, err := markerPending(); err != nil || marked != (wantBlocked || point == "after_durable_finalization") {
				t.Fatalf("durable recovery marker after %s = %t, %v", point, marked, err)
			}
			wantResume := ""
			switch point {
			case "before_marker":
				wantResume = maintenancepkg.RestorePreparing
			case "after_durable_finalization", "after_marker_removal", "after_rollback_marker_removal":
				wantResume = maintenancepkg.RestoreFinalized
			}
			if wantResume != "" {
				service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{
					DBPath: func() string { return dbPath }, TempDir: func() string { return dir },
				})
				report, err := service.OfflineRestoreRecovery(t.Context(), "", nil)
				if err != nil || report.RecoveryRequired || report.ResumeOnStartup != wantResume || len(report.Sets) != 0 {
					t.Fatalf("offline inspection after %s = %+v, %v", point, report, err)
				}
				store := maintenancepkg.SQLiteStore{DB: func() *sql.DB { return db }}
				if persisted, err := store.Load(t.Context()); err != nil || !persisted.Active {
					t.Fatalf("inspection changed the active maintenance row: %+v, %v", persisted, err)
				}
			}
			coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{
				Store: maintenancepkg.SQLiteStore{DB: func() *sql.DB { return db }}, RecoveryPending: markerPending,
				ClearFinalizedRestoreMarker: func(digest string) (bool, error) {
					return internalbackup.ClearFinalizedRestoreMarker(dbPath, digest)
				},
			})
			if err := coordinator.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
			if snapshot := coordinator.Snapshot(); snapshot.Active != wantBlocked || snapshot.RecoveryRequired != wantBlocked {
				t.Errorf("startup maintenance after %s: %+v, want blocked=%t", point, snapshot, wantBlocked)
			}
			if marked, err := markerPending(); err != nil || marked != wantBlocked {
				t.Errorf("marker after startup at %s = %t, %v", point, marked, err)
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

type restoreCrashStore struct {
	maintenancepkg.SQLiteStore
	point string
}

func (s restoreCrashStore) SaveDurable(ctx context.Context, state maintenancepkg.State) error {
	err := s.SQLiteStore.SaveDurable(ctx, state)
	if err == nil && state.RestorePhase == maintenancepkg.RestoreFinalized && s.point == "after_durable_finalization" {
		os.Exit(restoreCrashTestExit)
	}
	return err
}

func runRestoreCrashChild(t *testing.T, dir, point string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	key := []byte("01234567890123456789012345678901")
	target := filepath.Join(dir, "servers.db")
	knownHosts := filepath.Join(dir, "known_hosts")
	var db *sql.DB
	reloaded := false
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
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{
		Store: restoreCrashStore{SQLiteStore: maintenancepkg.SQLiteStore{DB: getDB}, point: point},
	})
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
				if point == "before_marker" {
					os.Exit(restoreCrashTestExit)
				}
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
				if point == "after_rollback_marker_removal" && !reloaded {
					reloaded = true
					return errors.New("injected forward reload failure")
				}
				jm = newJobManagerWithRuntime(getDB(), nil, newServerState(), func() bool { return false })
				if point == "cancel_after_reload" {
					cancel()
				}
				return nil
			},
		},
	})
	lifecycle := newBackupOperationLifecycleWithDeps(backupOperationLifecycleDeps{
		Archive: service, CurrentJobManager: func() *JobManager { return jm }, ActiveServerActionNames: func() []string { return nil },
		EnsureEncryptionKey: func() []byte { return key }, RecordAudit: func(record backupOperationAuditRecord) {
			if (point == "after_marker_removal" && record.Status == "success") ||
				(point == "after_rollback_marker_removal" && record.Status == "failure") {
				os.Exit(restoreCrashTestExit)
			}
		},
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
