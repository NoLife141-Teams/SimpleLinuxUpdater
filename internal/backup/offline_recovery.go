package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"debian-updater/internal/installationlock"
	"debian-updater/internal/jobs"
	"debian-updater/internal/maintenance"
	"debian-updater/internal/notifications"

	"golang.org/x/crypto/ssh/knownhosts"
	"modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

const restoreRollbackPrefix = "slu-restore-rollback-"

var (
	errSQLiteRecoveryCorrupt = errors.New("SQLite integrity check failed")
	errSQLiteRecoveryMissing = errors.New("current SQLite file is missing")
)

type OfflineRecoverySet struct {
	ID             string                 `json:"id"`
	Ready          bool                   `json:"ready"`
	Blocker        string                 `json:"blocker,omitempty"`
	EvidenceSource string                 `json:"evidence_source,omitempty"`
	Servers        int                    `json:"servers,omitempty"`
	StorageRole    string                 `json:"storage_role,omitempty"`
	Storage        []RestoreStorageVolume `json:"storage,omitempty"`
	APTReports     int                    `json:"apt_reports,omitempty"`
}

type OfflineRecoveryReport struct {
	RecoveryRequired bool                   `json:"recovery_required"`
	Sets             []OfflineRecoverySet   `json:"sets"`
	Applied          string                 `json:"applied,omitempty"`
	Blocker          string                 `json:"blocker,omitempty"`
	Storage          []RestoreStorageVolume `json:"storage,omitempty"`
	Diagnosis        string                 `json:"diagnosis,omitempty"`
}

// OfflineRestoreRecovery inspects recovery sets or applies the explicitly
// selected set. The caller must own the exclusive installation process lock.
// mergeJobs uses the same policy as a normal in-place restore.
func (s *Service) OfflineRestoreRecovery(ctx context.Context, selection string, mergeJobs func(context.Context, *sql.DB, *sql.DB) error) (OfflineRecoveryReport, error) {
	report := OfflineRecoveryReport{Sets: []OfflineRecoverySet{}}
	database, err := installationlock.ResolveDatabasePath(s.deps.DBPath())
	if err != nil {
		return report, errors.New("could not resolve installation directory")
	}
	dir := filepath.Dir(database)
	marker := RestoreRecoveryMarkerPath(database)
	fs := restoreFiles{sync: s.syncRestoreFile}
	marked, err := RestoreRecoveryPending(database)
	if err != nil {
		return report, errors.New("could not inspect restore recovery marker")
	}
	report.RecoveryRequired = marked
	var ids []string
	var recorded restoreRecoveryMarker
	if marked {
		recorded, err = readRestoreRecoveryMarker(marker)
		if err != nil {
			return report, errors.New("restore recovery marker is invalid")
		}
		ids = []string{recorded.ID}
	}
	if marked && selection != "" && selection != recorded.ID {
		return report, errors.New("selected recovery set does not belong to this interrupted restore")
	}
	tempDir := s.deps.TempDir()
	if tempDir == "" {
		tempDir = os.TempDir()
	}
	preparation := recoveryFileNeed(tempDir, restoreStoragePreparation, database, database+"-wal", database+"-shm")
	volumes := s.estimateRestoreStorage(preparation)
	storageFailure := func(cause error) (OfflineRecoveryReport, error) {
		report.Blocker = offlineRecoveryStorageBlocker
		report.Storage = recoveryStorageFailure(volumes, restoreStoragePreparation, cause)
		if selection == "" && marked {
			return report, nil
		}
		return report, offlineRecoveryStorageError(cause)
	}
	if err := storageBlocker(volumes); err != nil {
		return storageFailure(err)
	}
	work, err := os.MkdirTemp(tempDir, "slu-offline-recovery-*")
	if err != nil {
		if IsStorageFull(err) {
			return storageFailure(err)
		}
		return report, errors.New("could not prepare recovery workspace")
	}
	defer os.RemoveAll(work)
	livePath := filepath.Join(work, "current.sqlite")
	live, liveErr := cloneRecoveryDatabase(ctx, fs, livePath, database)
	if IsStorageFull(liveErr) {
		return storageFailure(liveErr)
	}
	if liveErr != nil && !canUseSealedRecoveryEvidence(liveErr) {
		report.Blocker = "current_database_unavailable"
		return report, errors.New("could not prepare a copy of current SQLite evidence")
	}
	if live != nil {
		defer live.Close()
	}
	if !marked && liveErr == nil {
		store := maintenance.SQLiteStore{DB: func() *sql.DB { return live }}
		state, loadErr := store.Load(ctx)
		if loadErr != nil {
			return report, errors.New("could not read the installation maintenance state")
		}
		report.RecoveryRequired = state.Active && (state.RecoveryRequired || state.Kind == string(maintenance.OperationBackupRestore))
	}
	if !report.RecoveryRequired {
		if liveErr != nil {
			return report, errors.New("could not inspect current SQLite state")
		}
		if selection != "" {
			return report, errors.New("installation does not require restore recovery")
		}
		return report, nil
	}
	if !marked {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return report, errors.New("could not list recovery sets")
		}
		for _, entry := range entries {
			if entry.IsDir() && validRecoverySetID(entry.Name()) {
				ids = append(ids, entry.Name())
			}
		}
	}
	if selection != "" {
		if !validRecoverySetID(selection) || !containsRecoveryID(ids, selection) {
			return report, errors.New("selected recovery set does not belong to this interrupted restore")
		}
		ids = []string{selection}
	}
	if liveErr != nil && recorded.ManifestSHA256 == "" {
		for _, id := range ids {
			report.Sets = append(report.Sets, OfflineRecoverySet{ID: id, Blocker: "current_database_unreadable"})
		}
		if selection != "" {
			return report, errors.New("current APT and delivery evidence could not be read; recovery remains locked")
		}
		return report, nil
	}
	if live != nil {
		if err := s.deps.EnsureSchema(live); err != nil {
			if IsStorageFull(err) {
				return storageFailure(err)
			}
			return report, errors.New("could not read current recovery evidence")
		}
	}
	config, err := readRecoveryFile(s.deps.ConfigPath(), MaxExtractedBytes)
	if err != nil {
		return report, errors.New("could not read installation configuration")
	}
	key, err := s.ValidateConfigData(config)
	if err != nil {
		return report, errors.New("installation encryption key is invalid")
	}
	hosts := s.restoreKnownHostsTarget(database)
	targets := append([]string{database}, SQLiteSidecarPaths(database)...)
	for _, reserved := range append(append([]string{}, targets...), marker, s.deps.ConfigPath(), database+".lock", RestoreIsolationPath(database), restoreIsolationPendingPath(database)) {
		if same, err := sameRestoreEntry(reserved, hosts); err != nil || same {
			return report, errors.New("installation host trust path conflicts with recovery files")
		}
	}

	diagnosisTargets := append(append([]string{}, targets...), hosts, s.deps.ConfigPath(), marker)
	for _, id := range ids {
		set := OfflineRecoverySet{ID: id}
		setWork, err := os.MkdirTemp(work, "set-*")
		if err != nil {
			if IsStorageFull(err) {
				return storageFailure(err)
			}
			return report, errors.New("could not prepare recovery workspace")
		}
		chosen := recorded
		if !marked {
			chosen.ID = id
		}
		staged, stagedHosts, setErr := s.prepareOfflineRecoverySet(ctx, filepath.Join(dir, id), setWork, targets, hosts, key, live, mergeJobs, chosen, &set, diagnosisTargets)
		if selection == "" {
			if setErr == nil {
				// Evidence can grow the staged database. Check its actual
				// size after the staging connections have closed.
				stagedBytes, _ := sizeOnDisk(staged)
				hostsPath := recoveryHostReplacementPath(hosts, stagedHosts, true)
				remaining := s.estimateRestoreStorage(s.recoveryReplacementNeeds(database, hosts, hostsPath, stagedBytes, diagnosisTargets)...)
				if err := storageBlocker(remaining); err != nil {
					set.storageFailure(err, "")
				}
			}
			report.Sets = append(report.Sets, set)
			if err := os.RemoveAll(setWork); err != nil {
				return report, errors.New("could not remove temporary recovery files")
			}
			continue
		}
		report.Sets = append(report.Sets, set)
		if setErr != nil {
			if set.Blocker == offlineRecoveryStorageBlocker {
				return report, setErr
			}
			return report, errors.New("selected recovery set did not pass validation; recovery remains locked")
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		databaseBytes, _ := sizeOnDisk(staged)
		hostsPath := recoveryHostReplacementPath(hosts, stagedHosts, true)
		selected := &report.Sets[len(report.Sets)-1]
		failStorage := func(cause error, role string) (OfflineRecoveryReport, error) {
			err := selected.storageFailure(cause, role)
			return report, err
		}
		selected.Storage = s.estimateRestoreStorage(s.recoveryReplacementNeeds(database, hosts, hostsPath, databaseBytes, diagnosisTargets)...)
		if err := storageBlocker(selected.Storage); err != nil {
			return failStorage(err, "")
		}
		diagnosis, entries, err := fs.snapshotFilesToDirectory(dir, diagnosisTargets, "slu-recovery-diagnosis-")
		if err != nil {
			if IsStorageFull(err) {
				return failStorage(err, offlineRecoveryDiagnosis)
			}
			return report, errors.New("could not preserve current installation for diagnosis")
		}
		report.Diagnosis = filepath.Base(diagnosis)
		discardProvisionalDiagnosis := func() {
			if os.RemoveAll(diagnosis) == nil {
				report.Diagnosis = ""
			}
		}
		raw, err := json.MarshalIndent(entries, "", "  ")
		if err == nil {
			err = fs.writeSnapshot(filepath.Join(diagnosis, "recovery.json"), bytes.NewReader(raw))
		}
		if err == nil {
			err = fs.syncPath(diagnosis)
		}
		if err == nil {
			err = fs.syncPath(dir)
		}
		if err != nil {
			if IsStorageFull(err) {
				discardProvisionalDiagnosis()
				return failStorage(err, offlineRecoveryDiagnosis)
			}
			return report, errors.New("could not persist the diagnosis copy; recovery remains locked")
		}
		currentHosts := entries[len(targets)]
		if stagedHosts != nil && stagedHosts.Exists == currentHosts.Exists && stagedHosts.SHA256 == currentHosts.SHA256 {
			// Confirm the directory entry is durable even when a prior
			// attempt installed or removed known_hosts before its sync failed.
			if err := fs.syncPath(restoreEntryDirectory(hosts)); err != nil &&
				(currentHosts.Exists || !errors.Is(err, os.ErrNotExist)) {
				if IsStorageFull(err) {
					discardProvisionalDiagnosis()
					return failStorage(err, restoreStorageHostKeys)
				}
				return report, errors.New("could not synchronize host trust; recovery remains locked")
			}
			stagedHosts = nil
		}
		hostsPath = ""
		if stagedHosts != nil && stagedHosts.Exists {
			hostsPath = stagedHosts.BackupPath
		}
		selected.Storage = s.estimateRestoreStorage(s.recoveryReplacementNeeds(database, hosts, hostsPath, databaseBytes, nil)...)
		if err := storageBlocker(selected.Storage); err != nil {
			discardProvisionalDiagnosis()
			return failStorage(err, "")
		}
		if !marked {
			if _, err := fs.publishRecoveryMarker(marker, chosen); err != nil {
				if IsStorageFull(err) {
					discardProvisionalDiagnosis()
					return failStorage(err, restoreStorageDatabase)
				}
				return report, errors.New("could not persist restore recovery marker")
			}
		}
		if err := s.applyOfflineRecovery(ctx, fs, staged, stagedHosts, database, hosts, marker, chosen, live != nil); err != nil {
			var failure offlineRecoveryWriteFailure
			if errors.As(err, &failure) {
				return failStorage(err, string(failure))
			}
			return report, err
		}
		report.Applied = id
		report.RecoveryRequired = false
	}
	return report, nil
}

func (s *Service) prepareOfflineRecoverySet(ctx context.Context, rootPath, work string, targets []string, hosts string, key []byte, live *sql.DB, mergeJobs func(context.Context, *sql.DB, *sql.DB) error, marker restoreRecoveryMarker, result *OfflineRecoverySet, diagnosis []string) (string, *fileRestoreSnapshot, error) {
	fail := func(code string, cause error) (string, *fileRestoreSnapshot, error) {
		if IsStorageFull(cause) {
			return "", nil, result.storageFailure(cause, restoreStoragePreparation)
		}
		result.Blocker = code
		return "", nil, errors.New(code)
	}
	id := result.ID
	if marker.ManifestSHA256 != "" && !marker.matchesInstallation(key) {
		return fail("installation_mismatch", nil)
	}
	info, err := os.Lstat(rootPath)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fail("invalid_manifest", nil)
	}
	snapshots, sealed, err := loadRecoverySnapshots(rootPath, marker)
	if err != nil {
		return fail("invalid_manifest", nil)
	}
	paths := append(append([]string{}, targets...), hosts)
	indexed, err := validateRecoverySnapshots(rootPath, paths, snapshots)
	if err != nil {
		return fail("invalid_manifest", nil)
	}
	var inputs, databaseInputs []string
	for i, entry := range indexed {
		if entry == nil || !entry.Exists || (i == 2 && !sealed) {
			continue
		}
		inputs = append(inputs, entry.BackupPath)
		if i < 2 {
			databaseInputs = append(databaseInputs, entry.BackupPath)
		}
	}
	hostsPath := recoveryHostReplacementPath(hosts, indexed[len(targets)], sealed)
	preparation := recoveryFileNeed(work, restoreStoragePreparation, inputs...)
	// The reconstructed database, SQLite's working files and VACUUM INTO
	// output can coexist. The live clone already consumes space, and only
	// its APT and delivery evidence can grow the reconstructed database.
	database := recoveryFileNeed(work, restoreStoragePreparation, databaseInputs...)
	databaseBytes := database.bytes
	preparation.bytes = storageSum(preparation.bytes, storageSum(databaseBytes, databaseBytes))
	preparation.incomplete = preparation.incomplete || database.incomplete
	needs := s.recoveryReplacementNeeds(targets[0], hosts, hostsPath, databaseBytes, diagnosis)
	result.Storage = s.estimateRestoreStorage(append([]restoreStorageNeed{preparation}, needs...)...)
	if err := storageBlocker(result.Storage); err != nil {
		return "", nil, result.storageFailure(err, "")
	}
	reconstructed := filepath.Join(work, id+".sqlite")
	fs := restoreFiles{sync: s.syncRestoreFile}
	for i, suffix := range []string{"", "-wal", ".snapshot-shm", ".known_hosts"} {
		entry := indexed[i]
		if entry == nil || !entry.Exists {
			continue
		}
		if i == 2 && !sealed {
			continue
		}
		destination := reconstructed + suffix
		digest, err := fs.copyAndHashSnapshot(destination, entry.BackupPath)
		if err != nil || (sealed && digest != entry.SHA256) {
			return fail("invalid_snapshot", err)
		}
		entry.SHA256 = digest
		entry.BackupPath = destination
	}
	if err := s.ValidateDatabaseFile(ctx, reconstructed, key); err != nil {
		return fail("invalid_database", err)
	}
	// A restore that didn't replace host trust retains the current file.
	checkHosts := hosts
	if indexed[3] != nil {
		checkHosts = indexed[3].BackupPath
	}
	if checkHosts != "" {
		if _, err := knownhosts.New(checkHosts); err != nil && (indexed[3] != nil || !errors.Is(err, os.ErrNotExist)) {
			return fail("invalid_host_trust", nil)
		}
	}
	db, err := openRecoveryCopy(ctx, reconstructed)
	if err != nil {
		return fail("invalid_database", err)
	}
	defer db.Close()
	source := "live_database"
	if live == nil {
		// The sealed rollback database is the complete pre-replacement state.
		// Use a separate connection and copy for the same evidence validation
		// used when merging from a readable live database.
		live, err = cloneRecoveryDatabase(ctx, fs, reconstructed+".evidence", reconstructed)
		if err != nil {
			return fail("invalid_database", err)
		}
		defer live.Close()
		source = "sealed_rollback"
	}
	if mergeJobs == nil {
		return fail("apt_evidence_conflict", nil)
	}
	if err := mergeJobs(ctx, live, db); err != nil {
		return fail("apt_evidence_conflict", err)
	}
	if err := notifications.MergeOfflineDeliveryEvidence(ctx, live, db, s.deps.Now()); err != nil {
		return fail("delivery_evidence_conflict", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM servers").Scan(&result.Servers); err != nil {
		return fail("invalid_database", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail("invalid_database", err)
	}
	incidents, readErr := jobs.ListAPTReconciliationIncidents(ctx, tx)
	_ = tx.Rollback()
	if readErr != nil {
		return fail("invalid_database", readErr)
	}
	result.APTReports = len(incidents)
	if err := clearDatabaseSessions(ctx, db); err != nil {
		return fail("invalid_database", err)
	}
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return db }}
	if err := store.Save(ctx, maintenance.State{
		Active: true, RecoveryRequired: true, Kind: string(maintenance.OperationBackupRestore),
	}); err != nil {
		return fail("invalid_database", err)
	}
	// Install a self-contained SQLite file with all WAL changes folded in.
	staged := filepath.Join(work, id+".complete")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", staged); err != nil {
		return fail("invalid_database", err)
	}
	result.Ready = true
	result.EvidenceSource = source
	return staged, indexed[3], nil
}

func loadRecoverySnapshots(root string, marker restoreRecoveryMarker) ([]fileRestoreSnapshot, bool, error) {
	raw, err := readRecoveryFile(filepath.Join(root, "recovery.json"), 128*1024)
	if err != nil {
		return nil, false, err
	}
	if marker.ManifestSHA256 != "" && recoveryDigest(raw) != marker.ManifestSHA256 {
		return nil, false, errors.New("rollback manifest does not match recovery marker")
	}
	var manifest sealedRecoveryManifest
	if err := json.Unmarshal(raw, &manifest); err == nil {
		// The database, WAL and SHM entries are always declared. Trust is
		// included only when the restore could replace the active trust file.
		expectedSnapshots := 3
		if manifest.ReplacesKnownHosts {
			expectedSnapshots++
		}
		if manifest.Version != restoreSealVersion || manifest.SetID != marker.ID || len(manifest.Snapshots) != expectedSnapshots {
			return nil, false, errors.New("invalid sealed recovery manifest")
		}
		for _, entry := range manifest.Snapshots {
			if entry.Exists && !validRecoveryDigest(entry.SHA256) || !entry.Exists && entry.SHA256 != "" {
				return nil, false, errors.New("invalid snapshot digest")
			}
		}
		return manifest.Snapshots, true, nil
	}
	var legacy []fileRestoreSnapshot
	if marker.ManifestSHA256 != "" || json.Unmarshal(raw, &legacy) != nil {
		return nil, false, errors.New("invalid legacy recovery manifest")
	}
	return legacy, false, nil
}

func validateRecoverySnapshots(root string, paths []string, snapshots []fileRestoreSnapshot) ([]*fileRestoreSnapshot, error) {
	if len(snapshots) != len(paths) && len(snapshots) != len(paths)-1 {
		return nil, errors.New("incomplete recovery manifest")
	}
	indexed := make([]*fileRestoreSnapshot, len(paths))
	for i, target := range paths {
		path := filepath.Join(root, "snapshot-"+strconv.Itoa(i))
		info, statErr := os.Lstat(path)
		if i >= len(snapshots) {
			if !errors.Is(statErr, os.ErrNotExist) {
				return nil, errors.New("unlisted rollback snapshot file")
			}
			continue
		}
		entry := snapshots[i]
		same, err := sameRestoreEntry(target, entry.Target)
		if err != nil || !same {
			return nil, errors.New("recovery target does not match installation")
		}
		if entry.Exists {
			same, err := sameRestoreEntry(path, entry.BackupPath)
			if err != nil || !same || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return nil, errors.New("invalid rollback snapshot file")
			}
			entry.BackupPath = path
		} else if entry.BackupPath != "" || i == 0 || !errors.Is(statErr, os.ErrNotExist) {
			return nil, errors.New("invalid missing rollback snapshot")
		}
		indexed[i] = &entry
	}
	return indexed, nil
}

func (s *Service) applyOfflineRecovery(ctx context.Context, fs restoreFiles, staged string, stagedHosts *fileRestoreSnapshot, database, hosts, marker string, recorded restoreRecoveryMarker, readable bool) error {
	fail := errors.New("could not apply verified recovery files; recovery remains locked")
	writeFailure := func(err error, role string) error {
		if IsStorageFull(err) {
			return offlineRecoveryWriteFailure(role)
		}
		return fail
	}
	// For sealed recovery, the unreadable files have already been saved to
	// the diagnosis directory. SQLite cannot checkpoint their WAL safely.
	if readable {
		if err := fs.checkpointRecoveryWAL(ctx, database); err != nil {
			return writeFailure(err, restoreStorageDatabase)
		}
	}
	if err := fs.removeSQLiteSidecars(database); err != nil {
		return writeFailure(err, restoreStorageDatabase)
	}
	if err := fs.writeAtomicFileFromPath(database, staged, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
		return writeFailure(err, restoreStorageDatabase)
	}
	if stagedHosts != nil {
		if stagedHosts.Exists {
			if err := fs.writeAtomicFileFromPath(hosts, stagedHosts.BackupPath, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
				return writeFailure(err, restoreStorageHostKeys)
			}
		} else if err := fs.removeFile(hosts); err != nil {
			return writeFailure(err, restoreStorageHostKeys)
		}
	}
	// Keep the SQLite latch while removing the filesystem marker. Either gate
	// must survive if verification or the marker directory sync fails.
	db, err := openRecoveryCopy(ctx, database)
	if err != nil {
		return writeFailure(err, restoreStorageDatabase)
	}
	defer db.Close()
	if _, err := knownhosts.New(hosts); err != nil && !errors.Is(err, os.ErrNotExist) {
		return writeFailure(err, restoreStorageHostKeys)
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return writeFailure(err, restoreStorageDatabase)
	}
	if err := fs.removeFile(marker); err != nil {
		_, _ = fs.publishRecoveryMarker(marker, recorded)
		return writeFailure(err, restoreStorageDatabase)
	}
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return db }}
	if err := errors.Join(store.Save(ctx, maintenance.State{}), db.Close()); err != nil {
		_, _ = fs.publishRecoveryMarker(marker, recorded)
		return writeFailure(err, restoreStorageDatabase)
	}
	return nil
}

func (fs restoreFiles) checkpointRecoveryWAL(ctx context.Context, database string) error {
	if _, err := os.Lstat(database + "-wal"); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	// The installation is offline. Rebuild the transient WAL index and
	// preserve any committed frames in the live database until replacement.
	if err := fs.removeFile(database + "-shm"); err != nil {
		return err
	}
	db, err := openRecoveryCopy(ctx, database)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return err
	}
	var busy, frames, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &frames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 || frames != checkpointed {
		return errors.New("could not checkpoint the live SQLite WAL")
	}
	return db.Close()
}

func cloneRecoveryDatabase(ctx context.Context, fs restoreFiles, dest, source string) (*sql.DB, error) {
	if err := fs.copySnapshot(dest, source); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) && pathErr.Path == source && errors.Is(err, os.ErrNotExist) {
			return nil, errSQLiteRecoveryMissing
		}
		return nil, err
	}
	wal := source + "-wal"
	if info, err := os.Lstat(wal); err == nil && info.Mode().IsRegular() {
		if err := fs.copySnapshot(dest+"-wal", wal); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, errors.New("invalid SQLite WAL")
	}
	return openRecoveryCopy(ctx, dest)
}

func openRecoveryCopy(ctx context.Context, path string) (*sql.DB, error) {
	absolute, err := installationlock.ResolveDatabasePath(path)
	if err != nil {
		return nil, err
	}
	if info, err := os.Lstat(absolute); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("SQLite recovery file is unavailable")
	}
	uriPath := filepath.ToSlash(absolute)
	if !strings.HasPrefix(uriPath, "/") {
		uriPath = "/" + uriPath
	}
	dsn := url.URL{Scheme: "file", Path: uriPath, RawQuery: "mode=rw"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	count, passed := 0, true
	for rows.Next() {
		var check string
		if err = rows.Scan(&check); err != nil {
			break
		}
		count++
		passed = passed && check == "ok"
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err == nil && passed && count == 1 {
		return db, nil
	}
	_ = db.Close()
	if err != nil {
		return nil, err
	}
	return nil, errSQLiteRecoveryCorrupt
}

func canUseSealedRecoveryEvidence(err error) bool {
	if errors.Is(err, errSQLiteRecoveryCorrupt) || errors.Is(err, errSQLiteRecoveryMissing) {
		return true
	}
	var sqliteErr *sqlite.Error
	if !errors.As(err, &sqliteErr) {
		return false
	}
	code := sqliteErr.Code() & 0xff
	return code == sqlitelib.SQLITE_CORRUPT || code == sqlitelib.SQLITE_NOTADB
}

func readRecoveryFile(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("unreadable recovery file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if int64(len(data)) > limit {
		return nil, errors.New("recovery file exceeded its size limit")
	}
	return data, err
}

func validRecoverySetID(id string) bool {
	suffix, ok := strings.CutPrefix(id, restoreRollbackPrefix)
	n, err := strconv.ParseUint(suffix, 10, 64)
	return ok && err == nil && strconv.FormatUint(n, 10) == suffix
}

func containsRecoveryID(ids []string, wanted string) bool {
	for _, id := range ids {
		if id == wanted {
			return true
		}
	}
	return false
}
