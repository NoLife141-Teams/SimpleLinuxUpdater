package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"debian-updater/internal/maintenance"
)

func interruptedRecoveryFixture(t *testing.T) (durabilityFixture, string, string) {
	t.Helper()
	root := t.TempDir()
	calls := 0
	f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
		calls++
		return errors.New("fixture reload failure")
	}})
	f.service.deps.ConfigPath = func() string { return f.files["config.json"] }
	f.service.syncRestoreFile = func(file *os.File) error {
		if calls == 1 && filepath.Dir(file.Name()) == root && strings.HasPrefix(filepath.Base(file.Name()), ".restore-") {
			return syscall.EIO
		}
		return file.Sync()
	}
	var incomplete *IncompleteRecoveryError
	if err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{}); !errors.As(err, &incomplete) {
		t.Fatalf("expected incomplete recovery: %v", err)
	}
	f.service.syncRestoreFile = nil
	id := filepath.Base(incomplete.SnapshotDir)
	return f, root, id
}

func offlineRecoveryMergeFixtureJobs(context.Context, *sql.DB, *sql.DB) error { return nil }

func writeLegacyRecoveryFixture(t *testing.T, f durabilityFixture, root, id string, mutate func([]fileRestoreSnapshot) []fileRestoreSnapshot) {
	t.Helper()
	markerPath := RestoreRecoveryMarkerPath(f.db)
	marker, err := readRestoreRecoveryMarker(markerPath)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, id)
	entries, _, err := loadRecoverySnapshots(directory, marker)
	if err != nil {
		t.Fatal(err)
	}
	for i := range entries {
		entries[i].SHA256 = ""
	}
	raw, err := json.Marshal(mutate(entries))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "recovery.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(markerPath, []byte(id+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOfflineRecoveryInspectsAndAppliesWithDurableDiagnosis(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	before, err := os.ReadFile(f.db)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
	if err != nil || !report.RecoveryRequired || len(report.Sets) != 1 || report.Sets[0].ID != id || !report.Sets[0].Ready {
		t.Fatalf("inspect = %+v, %v", report, err)
	}
	current, err := os.ReadFile(f.db)
	if err != nil || !bytes.Equal(before, current) {
		t.Fatal("inspection changed the live database")
	}
	if _, err := f.service.OfflineRestoreRecovery(t.Context(), "slu-restore-rollback-0", offlineRecoveryMergeFixtureJobs); err == nil {
		t.Fatal("recovery accepted an unselected set")
	}
	report, err = f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || report.Applied != id || report.RecoveryRequired || report.Diagnosis == "" {
		t.Fatalf("apply = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
	retained, err := os.ReadFile(filepath.Join(root, report.Diagnosis, "snapshot-0"))
	if err != nil || !bytes.Equal(before, retained) {
		t.Fatal("diagnosis did not preserve pre-recovery bytes")
	}
}

func TestOfflineRecoveryRejectsInvalidManifestAndKeepsLatch(t *testing.T) {
	for _, tt := range []struct {
		name   string
		mutate func(durabilityFixture, []fileRestoreSnapshot) []fileRestoreSnapshot
	}{
		{"unrelated database snapshot", func(f durabilityFixture, entries []fileRestoreSnapshot) []fileRestoreSnapshot {
			entries[0].BackupPath = f.files["servers.db"]
			return entries
		}},
		{"omitted host trust snapshot", func(_ durabilityFixture, entries []fileRestoreSnapshot) []fileRestoreSnapshot {
			return entries[:3]
		}},
		{"host trust incorrectly marked absent", func(_ durabilityFixture, entries []fileRestoreSnapshot) []fileRestoreSnapshot {
			entries[3].Exists = false
			entries[3].BackupPath = ""
			return entries
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f, root, id := interruptedRecoveryFixture(t)
			writeLegacyRecoveryFixture(t, f, root, id, func(entries []fileRestoreSnapshot) []fileRestoreSnapshot {
				return tt.mutate(f, entries)
			})
			if err := os.WriteFile(f.hosts, []byte(archivedRestoreTrust), 0600); err != nil {
				t.Fatal(err)
			}
			report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
			if err == nil || len(report.Sets) != 1 || report.Sets[0].Blocker != "invalid_manifest" || !report.RecoveryRequired {
				t.Fatalf("apply = %+v, %v", report, err)
			}
			requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, archivedRestoreTrust, true)
		})
	}
}

func TestOfflineRecoveryUsesSealedRollbackWithUnreadableSQLite(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	corrupt := []byte("fixture with an invalid SQLite header")
	// This restore stopped before replacing trust. A read-only deployment
	// mount containing the original bytes must not need to be replaced.
	f.service.syncRestoreFile = func(file *os.File) error {
		if filepath.Dir(file.Name()) == filepath.Dir(f.hosts) && strings.HasPrefix(filepath.Base(file.Name()), ".restore-") {
			return syscall.EROFS
		}
		return file.Sync()
	}
	for _, path := range []string{f.db, f.db + "-wal"} {
		if err := os.WriteFile(path, corrupt, 0600); err != nil {
			t.Fatal(err)
		}
	}
	inspect, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
	if err != nil || !inspect.RecoveryRequired || len(inspect.Sets) != 1 || !inspect.Sets[0].Ready ||
		inspect.Sets[0].EvidenceSource != "sealed_rollback" {
		t.Fatalf("inspect = %+v, %v", inspect, err)
	}
	active, err := os.ReadFile(f.db)
	if err != nil || !bytes.Equal(active, corrupt) {
		t.Fatal("inspection changed the unreadable database")
	}
	applied, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || applied.Applied != id || applied.RecoveryRequired || applied.Diagnosis == "" {
		t.Fatalf("apply = %+v, %v", applied, err)
	}
	for _, name := range []string{"snapshot-0", "snapshot-1"} {
		body, err := os.ReadFile(filepath.Join(root, applied.Diagnosis, name))
		if err != nil || !bytes.Equal(body, corrupt) {
			t.Fatalf("diagnosis lost %s: %v", name, err)
		}
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestOfflineRecoveryRequiresIntactSealForUnreadableSQLite(t *testing.T) {
	for _, scenario := range []struct {
		name    string
		blocker string
	}{
		{"legacy", "current_database_unreadable"},
		{"different manifest", "invalid_manifest"},
		{"changed host trust", "invalid_snapshot"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f, root, id := interruptedRecoveryFixture(t)
			if scenario.name == "legacy" {
				writeLegacyRecoveryFixture(t, f, root, id, func(entries []fileRestoreSnapshot) []fileRestoreSnapshot {
					return entries
				})
				readable, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
				if err != nil || len(readable.Sets) != 1 || !readable.Sets[0].Ready || readable.Sets[0].EvidenceSource != "live_database" {
					t.Fatalf("legacy with readable SQLite: %+v, %v", readable, err)
				}
			} else {
				name := "recovery.json"
				content := []byte("{}")
				if scenario.name == "changed host trust" {
					name = "snapshot-3"
					content = []byte(archivedRestoreTrust)
				}
				if err := os.WriteFile(filepath.Join(root, id, name), content, 0600); err != nil {
					t.Fatal(err)
				}
			}
			corrupt := []byte("unreadable active database")
			if err := os.WriteFile(f.db, corrupt, 0600); err != nil {
				t.Fatal(err)
			}
			report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
			if err == nil || !report.RecoveryRequired || len(report.Sets) != 1 || report.Sets[0].Blocker != scenario.blocker || report.Diagnosis != "" {
				t.Fatalf("apply = %+v, %v", report, err)
			}
			present, err := RestoreRecoveryPending(f.db)
			if err != nil || !present {
				t.Fatal("unsafe recovery removed the marker")
			}
		})
	}
}

func TestOfflineRecoveryReleasesOnlyThePhysicalInstallation(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	child := filepath.Join(root, "child")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	otherDir := t.TempDir()
	aliasDir := filepath.Join(otherDir, "child-alias")
	if err := os.Symlink(child, aliasDir); err != nil {
		t.Skip("directory symlinks unavailable", err)
	}
	otherDB := filepath.Join(otherDir, "servers.db")
	if err := (restoreFiles{}).copySnapshot(otherDB, f.db); err != nil {
		t.Fatal(err)
	}
	other, err := openRecoveryCopy(t.Context(), otherDB)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return other }}
	if err := store.Save(t.Context(), maintenance.State{Active: true, RecoveryRequired: true, Kind: string(maintenance.OperationBackupRestore)}); err != nil {
		t.Fatal(err)
	}
	f.service.deps.DBPath = func() string {
		return aliasDir + string(filepath.Separator) + ".." + string(filepath.Separator) + "servers.db"
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || report.RecoveryRequired || report.Applied != id {
		t.Fatalf("apply = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
	if state, err := store.Load(t.Context()); err != nil || !state.RecoveryRequired {
		t.Fatalf("unrelated installation lost its recovery state: %+v, %v", state, err)
	}
}

func TestOfflineRecoveryMarkerRemovalSyncFailureKeepsSQLiteLatch(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	f.service.syncRestoreFile = func(file *os.File) error {
		if file.Name() == root {
			if _, err := os.Stat(RestoreRecoveryMarkerPath(f.db)); errors.Is(err, os.ErrNotExist) {
				return syscall.EIO
			}
		}
		return file.Sync()
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || !report.RecoveryRequired || report.Diagnosis == "" || report.Applied != "" {
		t.Fatalf("failed marker sync = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, true)
	f.service.syncRestoreFile = nil
	if err := os.WriteFile(f.db, []byte("damaged SQLite after interrupted recovery"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err = f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
	if err != nil || !report.RecoveryRequired || len(report.Sets) != 1 || !report.Sets[0].Ready ||
		report.Sets[0].EvidenceSource != "sealed_rollback" {
		t.Fatalf("retry inspection lost the sealed relationship: %+v, %v", report, err)
	}
}

func TestOfflineRecoveryCanRetryAfterMarkerWriteFailure(t *testing.T) {
	f, _, id := interruptedRecoveryFixture(t)
	marker := RestoreRecoveryMarkerPath(f.db)
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	live, err := openRecoveryCopy(t.Context(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return live }}
	state := maintenance.State{Active: true, RecoveryRequired: true, Kind: string(maintenance.OperationBackupRestore)}
	if err := errors.Join(store.Save(t.Context(), state), live.Close()); err != nil {
		t.Fatal(err)
	}
	f.service.syncRestoreFile = func(file *os.File) error {
		if file.Name() == marker || strings.HasPrefix(filepath.Base(file.Name()), ".slu-restore-recovery-") {
			// Simulate a cut before the marker contents reach durable storage.
			if err := file.Truncate(0); err != nil {
				t.Fatal(err)
			}
			return syscall.EIO
		}
		return file.Sync()
	}
	if report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs); err == nil || !report.RecoveryRequired || report.Diagnosis == "" {
		t.Fatalf("failed publication = %+v, %v", report, err)
	}
	f.service.syncRestoreFile = nil
	if report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs); err != nil || report.Applied != id || report.RecoveryRequired {
		t.Fatalf("retry = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestOfflineRecoveryWriteFailureKeepsCommittedWALState(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	// Copy a crash image while its writer is still open so the committed
	// change is in the WAL and no connection to the installation stays open.
	crashDB := filepath.Join(t.TempDir(), "crash.db")
	fs := restoreFiles{}
	if err := fs.copySnapshot(crashDB, f.db); err != nil {
		t.Fatal(err)
	}
	db, err := openRecoveryCopy(t.Context(), crashDB)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, query := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA wal_autocheckpoint=0",
		"CREATE TABLE committed_before_recovery (value INTEGER NOT NULL)",
		"INSERT INTO committed_before_recovery VALUES (42)",
	} {
		if _, err := db.Exec(query); err != nil {
			t.Fatal(err)
		}
	}
	if err := fs.writeAtomicFileFromPath(f.db, crashDB, 0600, nil); err != nil {
		t.Fatal(err)
	}
	if err := fs.copySnapshot(f.db+"-wal", crashDB+"-wal"); err != nil {
		t.Fatal(err)
	}
	f.service.syncRestoreFile = func(file *os.File) error {
		if filepath.Dir(file.Name()) == root && strings.HasPrefix(filepath.Base(file.Name()), ".restore-") {
			return syscall.EIO
		}
		return file.Sync()
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || !report.RecoveryRequired || report.Diagnosis == "" || report.Applied != "" {
		t.Fatalf("failed replacement = %+v, %v", report, err)
	}
	live, err := openRecoveryCopy(t.Context(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	defer live.Close()
	var value int
	if err := live.QueryRow("SELECT value FROM committed_before_recovery").Scan(&value); err != nil || value != 42 {
		t.Fatalf("failed replacement lost committed WAL data: value = %d, err = %v", value, err)
	}
}
