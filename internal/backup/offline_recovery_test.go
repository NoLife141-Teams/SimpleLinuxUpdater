package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"debian-updater/internal/installationhealth"
	"debian-updater/internal/maintenance"
)

func interruptedRecoveryFixture(t *testing.T, includeKnownHosts bool) (durabilityFixture, string, string) {
	t.Helper()
	root := t.TempDir()
	calls := 0
	f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
		calls++
		return errors.New("fixture reload failure")
	}})
	f.service.deps.ConfigPath = func() string { return f.files["config.json"] }
	if !includeKnownHosts {
		delete(f.files, "known_hosts")
	}
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
	f, root, id := interruptedRecoveryFixture(t, true)
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
			f, root, id := interruptedRecoveryFixture(t, true)
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
	f, root, id := interruptedRecoveryFixture(t, true)
	corrupt := []byte("fixture with an invalid SQLite header")
	// This restore stopped before replacing trust. A read-only deployment
	// mount containing the original bytes must not require free space.
	free, full := uint64(128<<20), uint64(0)
	f.service.deps.ProbeRestoreStorage = func(dir string) installationhealth.VolumeCapacity {
		if dir == filepath.Dir(f.hosts) {
			return installationhealth.VolumeCapacity{Device: 2, Identified: true, AvailableBytes: &full}
		}
		return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &free}
	}
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

func TestOfflineRecoveryOfDatabaseOnlyRestorePreservesUpdatedKnownHosts(t *testing.T) {
	f, _, id := interruptedRecoveryFixture(t, false)
	updatedTrust := "# known_hosts refreshed after the interrupted DB-only restore\n"
	if err := os.WriteFile(f.hosts, []byte(updatedTrust), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.db, []byte("unreadable active SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || report.Applied != id || report.RecoveryRequired || len(report.Sets) != 1 || report.Sets[0].EvidenceSource != "sealed_rollback" {
		t.Fatalf("apply = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, updatedTrust, false)
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
			f, root, id := interruptedRecoveryFixture(t, true)
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

func TestOfflineRecoveryRejectsSealedSetWithWrongInstallationKey(t *testing.T) {
	f, _, id := interruptedRecoveryFixture(t, true)
	// Both databases in this fixture lack encrypted credentials. They share
	// the deployed paths a copied set could have on a different volume.
	damaged := []byte("unreadable SQLite from a different installation")
	if err := os.WriteFile(f.db, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	f.service.deps.DecodeEncryptionKey = func(string) ([]byte, error) { return []byte("other installation key"), nil }
	for _, selection := range []string{"", id} {
		report, err := f.service.OfflineRestoreRecovery(t.Context(), selection, offlineRecoveryMergeFixtureJobs)
		if (err != nil) != (selection != "") || !report.RecoveryRequired || report.Diagnosis != "" ||
			len(report.Sets) != 1 || report.Sets[0].Ready || report.Sets[0].Blocker != "installation_mismatch" {
			t.Fatalf("recovery with foreign seal (%q) = %+v, %v", selection, report, err)
		}
	}
	present, markerErr := RestoreRecoveryPending(f.db)
	retained, readErr := os.ReadFile(f.db)
	if markerErr != nil || !present || readErr != nil || !bytes.Equal(retained, damaged) {
		t.Fatalf("foreign set changed recovery state: marker=%v, error=%v, read=%v", present, markerErr, readErr)
	}
}

func TestOfflineRecoveryReleasesOnlyThePhysicalInstallation(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t, true)
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
	f, root, id := interruptedRecoveryFixture(t, true)
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

func TestOfflineRecoveryRetriesTrustDirectorySyncBeforeRelease(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t, true)
	if err := os.WriteFile(f.hosts, []byte(archivedRestoreTrust), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.db, []byte("damaged active SQLite"), 0600); err != nil {
		t.Fatal(err)
	}
	free := uint64(128 << 20)
	f.service.deps.ProbeRestoreStorage = func(dir string) installationhealth.VolumeCapacity {
		if dir == filepath.Dir(f.hosts) {
			return installationhealth.VolumeCapacity{Device: 2, Identified: true, AvailableBytes: &free}
		}
		return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &free}
	}
	f.service.syncRestoreFile = func(file *os.File) error {
		if file.Name() == filepath.Dir(f.hosts) {
			return syscall.ENOSPC
		}
		return file.Sync()
	}
	for attempt := range 2 {
		report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
		if !errors.Is(err, errOfflineRecoveryStorage) || report.Applied != "" || !report.RecoveryRequired {
			t.Fatalf("recovery cleared the latch before synchronizing host trust: %+v, %v", report, err)
		}
		if len(report.Sets) != 1 || report.Sets[0].StorageRole != restoreStorageHostKeys ||
			!slices.ContainsFunc(report.Sets[0].Storage, func(volume RestoreStorageVolume) bool {
				return slices.Contains(volume.Roles, restoreStorageDatabase) && volume.State == restoreStorageReady
			}) {
			t.Fatalf("a host trust error affected SQLite capacity: %+v", report.Sets)
		}
		if (report.Diagnosis == "") != (attempt == 1) {
			t.Fatalf("unexpected retained diagnosis for attempt %d: %+v", attempt, report)
		}
		// The first attempt installs the correct contents but its directory
		// sync fails. The second attempt sees those same bytes.
		requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, true)
	}
	diagnoses, err := filepath.Glob(filepath.Join(root, "slu-recovery-diagnosis-*"))
	if err != nil || len(diagnoses) != 1 {
		t.Fatalf("retry retained another diagnosis before replacing files: %v, %v", diagnoses, err)
	}
	f.service.syncRestoreFile = nil
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || report.Applied != id || report.RecoveryRequired {
		t.Fatalf("retry after restoring sync = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestOfflineRecoveryCanRetryAfterMarkerWriteFailure(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t, true)
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
			return syscall.ENOSPC
		}
		return file.Sync()
	}
	if report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs); !errors.Is(err, errOfflineRecoveryStorage) || !report.RecoveryRequired || report.Diagnosis != "" {
		t.Fatalf("failed publication = %+v, %v", report, err)
	}
	diagnoses, err := filepath.Glob(filepath.Join(root, "slu-recovery-diagnosis-*"))
	if err != nil || len(diagnoses) != 0 {
		t.Fatalf("marker failure retained a diagnosis before replacing files: %v, %v", diagnoses, err)
	}
	f.service.syncRestoreFile = nil
	if report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs); err != nil || report.Applied != id || report.RecoveryRequired {
		t.Fatalf("retry = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestOfflineRecoveryWriteFailureKeepsCommittedWALState(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t, true)
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
			return syscall.ENOSPC
		}
		return file.Sync()
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || !errors.Is(err, errOfflineRecoveryStorage) || !report.RecoveryRequired || report.Diagnosis == "" || report.Applied != "" || len(report.Sets) != 1 || report.Sets[0].Ready || report.Sets[0].Blocker != offlineRecoveryStorageBlocker {
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

func TestOfflineRecoveryDoesNotSubstituteSealedEvidenceWhenCopyFails(t *testing.T) {
	f, _, id := interruptedRecoveryFixture(t, true)
	checkFiles := func() map[string]string {
		t.Helper()
		files := make(map[string]string)
		for _, path := range []string{f.db, f.hosts, RestoreRecoveryMarkerPath(f.db)} {
			body, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			files[path] = string(body)
		}
		return files
	}
	live, err := openRecoveryCopy(t.Context(), f.db)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := live.Exec("CREATE TABLE live_only_evidence (value TEXT); INSERT INTO live_only_evidence VALUES ('verified')")
	if err := errors.Join(writeErr, live.Close()); err != nil {
		t.Fatal(err)
	}
	before := checkFiles()
	for _, failure := range []error{syscall.ENOSPC, syscall.EACCES} {
		f.service.syncRestoreFile = func(file *os.File) error {
			if filepath.Base(file.Name()) == "current.sqlite" {
				return failure
			}
			return file.Sync()
		}
		for _, selection := range []string{"", id} {
			report, err := f.service.OfflineRestoreRecovery(t.Context(), selection, offlineRecoveryMergeFixtureJobs)
			wantBlocker := offlineRecoveryStorageBlocker
			if errors.Is(failure, syscall.EACCES) {
				wantBlocker = "current_database_unavailable"
			}
			if !report.RecoveryRequired || report.Applied != "" || report.Blocker != wantBlocker || len(report.Sets) != 0 ||
				(err == nil) != (selection == "" && wantBlocker == offlineRecoveryStorageBlocker) {
				t.Fatalf("workspace failure %v with selection %q: %+v, %v", failure, selection, report, err)
			}
			if wantBlocker == offlineRecoveryStorageBlocker && (len(report.Storage) != 1 || report.Storage[0].AvailableBytes != nil) {
				t.Fatalf("a failed write must invalidate known workspace capacity: %+v", report.Storage)
			}
			for path, current := range checkFiles() {
				if current != before[path] {
					t.Fatal("a failed workspace copy changed an active file")
				}
			}
		}
	}
	f.service.syncRestoreFile = nil
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, func(ctx context.Context, live, staged *sql.DB) error {
		var evidence string
		if err := live.QueryRowContext(ctx, "SELECT value FROM live_only_evidence").Scan(&evidence); err != nil || evidence != "verified" {
			t.Fatalf("retry did not read active SQLite evidence: %q, %v", evidence, err)
		}
		return nil
	})
	if err != nil || report.Applied != id || report.RecoveryRequired || report.Sets[0].EvidenceSource != "live_database" {
		t.Fatalf("retry = %+v, %v", report, err)
	}
}

func TestOfflineRecoveryRechecksCapacityAfterCopyingDiagnosis(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t, true)
	if err := os.WriteFile(f.hosts, []byte(archivedRestoreTrust), 0600); err != nil {
		t.Fatal(err)
	}
	dbFree := uint64(128 << 20)
	prepFree := dbFree
	tempDir := f.service.deps.TempDir()
	f.service.deps.ProbeRestoreStorage = func(dir string) installationhealth.VolumeCapacity {
		if dir == tempDir || strings.HasPrefix(dir, tempDir+string(filepath.Separator)) {
			return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &prepFree}
		}
		return installationhealth.VolumeCapacity{Device: 2, Identified: true, AvailableBytes: &dbFree}
	}
	preview, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
	if err != nil || len(preview.Sets) != 1 || len(preview.Sets[0].Storage) != 2 || !preview.Sets[0].Ready {
		t.Fatalf("volume preview = %+v, %v", preview, err)
	}
	if !slices.ContainsFunc(preview.Sets[0].Storage, func(volume RestoreStorageVolume) bool {
		return slices.Contains(volume.Roles, restoreStorageDatabase) && slices.Contains(volume.Roles, restoreStorageHostKeys) &&
			slices.Contains(volume.Roles, offlineRecoveryDiagnosis)
	}) {
		t.Fatalf("replacement and diagnosis must share one volume: %+v", preview.Sets[0].Storage)
	}
	f.service.syncRestoreFile = func(file *os.File) error {
		if filepath.Base(file.Name()) == "recovery.json" && strings.HasPrefix(filepath.Base(filepath.Dir(file.Name())), "slu-recovery-diagnosis-") {
			dbFree = 1 << 20
		}
		return file.Sync()
	}
	result, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || result.Applied != "" || !result.RecoveryRequired || len(result.Sets) != 1 ||
		result.Sets[0].Ready || result.Sets[0].Blocker != offlineRecoveryStorageBlocker || result.Diagnosis != "" {
		t.Fatalf("late capacity recheck = %+v, %v", result, err)
	}
	if result.Sets[0].Storage[0].State != restoreStorageInsufficient {
		t.Fatalf("expected a measured shortage: %+v", result.Sets[0].Storage)
	}
	diagnoses, err := filepath.Glob(filepath.Join(root, "slu-recovery-diagnosis-*"))
	if err != nil || len(diagnoses) != 0 {
		t.Fatalf("blocked attempt retained provisional diagnosis: %v, %v", diagnoses, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, archivedRestoreTrust, true)
}
