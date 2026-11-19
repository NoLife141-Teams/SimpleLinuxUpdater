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

	"debian-updater/internal/jobs"
	"debian-updater/internal/maintenance"
	"debian-updater/internal/notifications"

	"golang.org/x/crypto/ssh/knownhosts"
)

const restoreRollbackPrefix = "slu-restore-rollback-"

type OfflineRecoverySet struct {
	ID         string `json:"id"`
	Ready      bool   `json:"ready"`
	Blocker    string `json:"blocker,omitempty"`
	Servers    int    `json:"servers,omitempty"`
	APTReports int    `json:"apt_reports,omitempty"`
}

type OfflineRecoveryReport struct {
	RecoveryRequired bool                 `json:"recovery_required"`
	Sets             []OfflineRecoverySet `json:"sets"`
	Applied          string               `json:"applied,omitempty"`
	Diagnosis        string               `json:"diagnosis,omitempty"`
}

// OfflineRestoreRecovery inspects recovery sets or applies the explicitly
// selected set. The caller must own the exclusive installation process lock.
// mergeJobs uses the same policy as a normal in-place restore.
func (s *Service) OfflineRestoreRecovery(ctx context.Context, selection string, mergeJobs func(context.Context, *sql.DB, *sql.DB) error) (OfflineRecoveryReport, error) {
	report := OfflineRecoveryReport{Sets: []OfflineRecoverySet{}}
	database := s.deps.DBPath()
	dir, err := filepath.EvalSymlinks(restoreEntryDirectory(database))
	if err != nil {
		return report, errors.New("could not resolve installation directory")
	}
	marker := RestoreRecoveryMarkerPath(database)
	fs := restoreFiles{sync: s.syncRestoreFile}
	marked, err := RestoreRecoveryPending(database)
	if err != nil {
		return report, errors.New("could not inspect restore recovery marker")
	}
	report.RecoveryRequired = marked
	var ids []string
	if marked {
		data, err := readRecoveryFile(marker, 512)
		id := strings.TrimSpace(string(data))
		if err != nil || !validRecoverySetID(id) {
			return report, errors.New("restore recovery marker is invalid")
		}
		ids = []string{id}
	}
	work, err := os.MkdirTemp(s.deps.TempDir(), "slu-offline-recovery-*")
	if err != nil {
		return report, errors.New("could not prepare recovery workspace")
	}
	defer os.RemoveAll(work)
	livePath := filepath.Join(work, "current.sqlite")
	live, liveErr := cloneRecoveryDatabase(ctx, fs, livePath, database)
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
	if liveErr != nil {
		for _, id := range ids {
			report.Sets = append(report.Sets, OfflineRecoverySet{ID: id, Blocker: "current_database_unreadable"})
		}
		if selection != "" {
			return report, errors.New("current APT and delivery evidence could not be read; recovery remains locked")
		}
		return report, nil
	}
	if err := s.deps.EnsureSchema(live); err != nil {
		return report, errors.New("could not read current recovery evidence")
	}
	config, err := readRecoveryFile(s.deps.ConfigPath(), MaxExtractedBytes)
	if err != nil {
		return report, errors.New("could not read installation configuration")
	}
	key, err := s.ValidateConfigData(config)
	if err != nil {
		return report, errors.New("installation encryption key is invalid")
	}
	hosts := filepath.Join(dir, "known_hosts")
	if path, err := s.deps.KnownHostsWritePath(); err == nil && strings.TrimSpace(path) != "" {
		hosts = path
	}
	targets := append([]string{database}, SQLiteSidecarPaths(database)...)
	for _, reserved := range append(append([]string{}, targets...), marker, s.deps.ConfigPath(), database+".lock") {
		if same, err := sameRestoreEntry(reserved, hosts); err != nil || same {
			return report, errors.New("installation host trust path conflicts with recovery files")
		}
	}

	for _, id := range ids {
		set := OfflineRecoverySet{ID: id}
		staged, stagedHosts, setErr := s.prepareOfflineRecoverySet(ctx, filepath.Join(dir, id), work, targets, hosts, key, live, mergeJobs, &set)
		report.Sets = append(report.Sets, set)
		if selection == "" {
			continue
		}
		if setErr != nil {
			return report, errors.New("selected recovery set did not pass validation; recovery remains locked")
		}
		if err := ctx.Err(); err != nil {
			return report, err
		}
		diagnosisTargets := append(append([]string{}, targets...), hosts, s.deps.ConfigPath(), marker)
		diagnosis, entries, err := fs.snapshotFilesToDirectory(dir, diagnosisTargets, "slu-recovery-diagnosis-")
		if err != nil {
			return report, errors.New("could not preserve current installation for diagnosis")
		}
		report.Diagnosis = filepath.Base(diagnosis)
		raw, err := json.MarshalIndent(entries, "", "  ")
		if err != nil || fs.writeSnapshot(filepath.Join(diagnosis, "recovery.json"), bytes.NewReader(raw)) != nil ||
			fs.syncPath(diagnosis) != nil || fs.syncPath(dir) != nil {
			return report, errors.New("could not persist the diagnosis copy; recovery remains locked")
		}
		if !marked {
			if _, err := fs.publishRecoveryMarker(marker, filepath.Join(dir, id)); err != nil {
				return report, errors.New("could not persist restore recovery marker")
			}
		}
		if err := s.applyOfflineRecovery(ctx, fs, staged, stagedHosts, database, hosts, marker, filepath.Join(dir, id)); err != nil {
			return report, err
		}
		report.Applied = id
		report.RecoveryRequired = false
	}
	return report, nil
}

func (s *Service) prepareOfflineRecoverySet(ctx context.Context, rootPath, work string, targets []string, hosts string, key []byte, live *sql.DB, mergeJobs func(context.Context, *sql.DB, *sql.DB) error, result *OfflineRecoverySet) (string, *fileRestoreSnapshot, error) {
	fail := func(code string) (string, *fileRestoreSnapshot, error) {
		result.Blocker = code
		return "", nil, errors.New(code)
	}
	id := result.ID
	info, err := os.Lstat(rootPath)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fail("invalid_manifest")
	}
	manifest, err := readRecoveryFile(filepath.Join(rootPath, "recovery.json"), 128*1024)
	var snapshots []fileRestoreSnapshot
	if err != nil || json.Unmarshal(manifest, &snapshots) != nil {
		return fail("invalid_manifest")
	}
	paths := append(append([]string{}, targets...), hosts)
	indexed, err := validateRecoverySnapshots(rootPath, paths, snapshots)
	if err != nil {
		return fail("invalid_manifest")
	}
	reconstructed := filepath.Join(work, id+".sqlite")
	fs := restoreFiles{sync: s.syncRestoreFile}
	for i, suffix := range []string{"", "-wal"} {
		if indexed[i] != nil && indexed[i].Exists {
			if err := fs.copySnapshot(reconstructed+suffix, indexed[i].BackupPath); err != nil {
				return fail("invalid_database")
			}
		}
	}
	if err := s.ValidateDatabaseFile(ctx, reconstructed, key); err != nil {
		return fail("invalid_database")
	}
	// A restore that didn't replace host trust retains the current file.
	checkHosts := hosts
	if indexed[3] != nil {
		checkHosts = indexed[3].BackupPath
	}
	if checkHosts != "" {
		if _, err := knownhosts.New(checkHosts); err != nil && (indexed[3] != nil || !errors.Is(err, os.ErrNotExist)) {
			return fail("invalid_host_trust")
		}
	}
	db, err := openRecoveryCopy(ctx, reconstructed)
	if err != nil {
		return fail("invalid_database")
	}
	defer db.Close()
	if mergeJobs == nil || mergeJobs(ctx, live, db) != nil ||
		notifications.MergeOfflineAPTDeliveryEvidence(ctx, live, db, s.deps.Now()) != nil {
		return fail("apt_evidence_conflict")
	}
	if err := db.QueryRowContext(ctx, "SELECT count(*) FROM servers").Scan(&result.Servers); err != nil {
		return fail("invalid_database")
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fail("invalid_database")
	}
	incidents, readErr := jobs.ListAPTReconciliationIncidents(ctx, tx)
	_ = tx.Rollback()
	if readErr != nil {
		return fail("invalid_database")
	}
	result.APTReports = len(incidents)
	if err := clearDatabaseSessions(ctx, db); err != nil {
		return fail("invalid_database")
	}
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return db }}
	if err := store.Save(ctx, maintenance.State{
		Active: true, RecoveryRequired: true, Kind: string(maintenance.OperationBackupRestore),
	}); err != nil {
		return fail("invalid_database")
	}
	// Install a self-contained SQLite file with all WAL changes folded in.
	staged := filepath.Join(work, id+".complete")
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", staged); err != nil {
		return fail("invalid_database")
	}
	result.Ready = true
	return staged, indexed[3], nil
}

func validateRecoverySnapshots(root string, paths []string, snapshots []fileRestoreSnapshot) ([]*fileRestoreSnapshot, error) {
	if len(snapshots) != len(paths) && len(snapshots) != len(paths)-1 {
		return nil, errors.New("incomplete recovery manifest")
	}
	indexed := make([]*fileRestoreSnapshot, len(paths))
	for i, entry := range snapshots {
		same, err := sameRestoreEntry(paths[i], entry.Target)
		if err != nil || !same {
			return nil, errors.New("recovery target does not match installation")
		}
		if entry.Exists {
			path := filepath.Join(root, "snapshot-"+strconv.Itoa(i))
			same, err := sameRestoreEntry(path, entry.BackupPath)
			info, statErr := os.Lstat(path)
			if err != nil || !same || statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
				return nil, errors.New("invalid rollback snapshot file")
			}
			entry.BackupPath = path
		} else if entry.BackupPath != "" || i == 0 {
			return nil, errors.New("invalid missing rollback snapshot")
		}
		indexed[i] = &entry
	}
	return indexed, nil
}

func (s *Service) applyOfflineRecovery(ctx context.Context, fs restoreFiles, staged string, stagedHosts *fileRestoreSnapshot, database, hosts, marker, snapshotDir string) error {
	fail := errors.New("could not apply verified recovery files; recovery remains locked")
	if err := fs.removeSQLiteSidecars(database); err != nil {
		return fail
	}
	if err := fs.writeAtomicFileFromPath(database, staged, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
		return fail
	}
	if stagedHosts != nil {
		if stagedHosts.Exists {
			if err := fs.writeAtomicFileFromPath(hosts, stagedHosts.BackupPath, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
				return fail
			}
		} else if err := fs.removeFile(hosts); err != nil {
			return fail
		}
	}
	// Keep the SQLite latch while removing the filesystem marker. Either gate
	// must survive if verification or the marker directory sync fails.
	db, err := openRecoveryCopy(ctx, database)
	if err != nil {
		return fail
	}
	defer db.Close()
	if _, err := knownhosts.New(hosts); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail
	}
	if _, err := db.ExecContext(ctx, "PRAGMA synchronous=FULL"); err != nil {
		return fail
	}
	if err := fs.removeFile(marker); err != nil {
		return fail
	}
	store := maintenance.SQLiteStore{DB: func() *sql.DB { return db }}
	if err := errors.Join(store.Save(ctx, maintenance.State{}), db.Close()); err != nil {
		_, _ = fs.publishRecoveryMarker(marker, snapshotDir)
		return fail
	}
	return nil
}

func cloneRecoveryDatabase(ctx context.Context, fs restoreFiles, dest, source string) (*sql.DB, error) {
	if err := fs.copySnapshot(dest, source); err != nil {
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
	if info, err := os.Lstat(path); err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("SQLite recovery file is unavailable")
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(absolute), RawQuery: "mode=rw"}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	rows, err := db.QueryContext(ctx, "PRAGMA quick_check")
	if err == nil {
		var check string
		passed := rows.Next() && rows.Scan(&check) == nil && check == "ok" && !rows.Next() && rows.Err() == nil
		if err := rows.Close(); passed && err == nil {
			return db, nil
		}
	}
	_ = db.Close()
	return nil, errors.New("SQLite integrity check failed")
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
