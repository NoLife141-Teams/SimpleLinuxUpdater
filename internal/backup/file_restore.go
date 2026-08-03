package backup

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"

	notificationpkg "debian-updater/internal/notifications"
	serverpkg "debian-updater/internal/servers"
)

func (s *Service) ValidateDatabaseFile(ctx context.Context, path string, encryptionKey []byte) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open restored database: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("set restored database busy_timeout: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "servers"); err != nil {
		return err
	}
	if err := s.deps.EnsureSchema(db); err != nil {
		return fmt.Errorf("validate restored database schema: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "jobs", "job_log_chunks"); err != nil {
		return err
	}

	rows, err := db.QueryContext(ctx, "SELECT name, pass_enc, key_enc FROM servers ORDER BY name")
	if err != nil {
		return fmt.Errorf("validate restored servers: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, passEnc, keyEnc string
		if err := rows.Scan(&name, &passEnc, &keyEnc); err != nil {
			return fmt.Errorf("scan restored server: %w", err)
		}
		if _, err := s.deps.DecryptSecretWithKey(passEnc, encryptionKey); err != nil {
			return fmt.Errorf("decrypt restored password for %s: %w", name, err)
		}
		if _, err := s.deps.DecryptSecretWithKey(keyEnc, encryptionKey); err != nil {
			return fmt.Errorf("decrypt restored SSH key for %s: %w", name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read restored servers: %w", err)
	}

	credential := serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
		Store: serverpkg.SQLiteGlobalSSHCredentialStore{DB: func() *sql.DB { return db }},
		Decrypt: func(encrypted string) (string, error) {
			return s.deps.DecryptSecretWithKey(encrypted, encryptionKey)
		},
	})
	if _, err := credential.Resolve(ctx, ""); err != nil {
		return fmt.Errorf("validate restored Global SSH Credential: %w", err)
	}
	return nil
}

func InspectDatabaseSafeCountsFile(ctx context.Context, path string) (SafeCounts, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return SafeCounts{}, fmt.Errorf("open restored database for safe counts: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return SafeCounts{}, fmt.Errorf("set restored database query-only mode: %w", err)
	}
	servers, err := countExistingRows(ctx, db, "servers")
	if err != nil {
		return SafeCounts{}, err
	}
	policies, err := countExistingRows(ctx, db, "update_policies")
	if err != nil {
		return SafeCounts{}, err
	}
	jobs, err := countExistingRows(ctx, db, "jobs")
	if err != nil {
		return SafeCounts{}, err
	}
	sessions, err := countExistingRows(ctx, db, "sessions")
	if err != nil {
		return SafeCounts{}, err
	}
	return SafeCounts{Servers: servers, Policies: policies, Jobs: jobs, Sessions: sessions}, nil
}

func (s *Service) ReencryptDatabaseFile(ctx context.Context, sourcePath string, fromKey, toKey []byte) (TemporaryFile, error) {
	tmp, err := createTemporaryFile(s.deps.TempDir(), "slu-restore-rewrap-*.sqlite")
	if err != nil {
		return TemporaryFile{}, err
	}
	path := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, err
	}
	if err := copyPathBounded(sourcePath, path, MaxExtractedBytes); err != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, err
	}
	if !bytes.Equal(fromKey, toKey) {
		if err := s.reencryptDatabaseFileInPlace(ctx, path, fromKey, toKey); err != nil {
			_ = os.Remove(path)
			return TemporaryFile{}, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		_ = os.Remove(path)
		return TemporaryFile{}, err
	}
	return TemporaryFile{Path: path, Size: info.Size()}, nil
}

func (s *Service) reencryptDatabaseFileInPlace(ctx context.Context, path string, fromKey, toKey []byte) error {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("open restored database for rewrap: %w", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return fmt.Errorf("set restored database rewrap busy_timeout: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "servers"); err != nil {
		return err
	}
	if err := s.deps.EnsureSchema(db); err != nil {
		return fmt.Errorf("prepare restored database rewrap schema: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "jobs", "job_log_chunks"); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restored database rewrap: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	type encryptedServerSecrets struct {
		name    string
		passEnc string
		keyEnc  string
	}
	rows, err := tx.QueryContext(ctx, "SELECT name, pass_enc, key_enc FROM servers ORDER BY name")
	if err != nil {
		return fmt.Errorf("read restored server secrets for rewrap: %w", err)
	}
	var secretRows []encryptedServerSecrets
	for rows.Next() {
		var row encryptedServerSecrets
		if err := rows.Scan(&row.name, &row.passEnc, &row.keyEnc); err != nil {
			_ = rows.Close()
			return fmt.Errorf("scan restored server secret for rewrap: %w", err)
		}
		secretRows = append(secretRows, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return fmt.Errorf("read restored server secret rows for rewrap: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close restored server secret rows for rewrap: %w", err)
	}

	updateServerStmt, err := tx.PrepareContext(ctx, "UPDATE servers SET pass_enc = ?, key_enc = ? WHERE name = ?")
	if err != nil {
		return fmt.Errorf("prepare restored server secret rewrap: %w", err)
	}
	for _, row := range secretRows {
		pass, err := s.deps.DecryptSecretWithKey(row.passEnc, fromKey)
		if err != nil {
			_ = updateServerStmt.Close()
			return fmt.Errorf("decrypt restored password for %s during rewrap: %w", row.name, err)
		}
		passEnc, err := s.deps.EncryptSecretWithKey(pass, toKey)
		if err != nil {
			_ = updateServerStmt.Close()
			return fmt.Errorf("encrypt restored password for %s during rewrap: %w", row.name, err)
		}
		key, err := s.deps.DecryptSecretWithKey(row.keyEnc, fromKey)
		if err != nil {
			_ = updateServerStmt.Close()
			return fmt.Errorf("decrypt restored SSH key for %s during rewrap: %w", row.name, err)
		}
		keyEnc, err := s.deps.EncryptSecretWithKey(key, toKey)
		if err != nil {
			_ = updateServerStmt.Close()
			return fmt.Errorf("encrypt restored SSH key for %s during rewrap: %w", row.name, err)
		}
		if _, err := updateServerStmt.ExecContext(ctx, passEnc, keyEnc, row.name); err != nil {
			_ = updateServerStmt.Close()
			return fmt.Errorf("update restored server secret for %s during rewrap: %w", row.name, err)
		}
	}
	if err := updateServerStmt.Close(); err != nil {
		return fmt.Errorf("close restored server secret rewrap statement: %w", err)
	}

	credential := serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
		Store: serverpkg.SQLiteGlobalSSHCredentialStore{Tx: tx},
		Decrypt: func(encrypted string) (string, error) {
			return s.deps.DecryptSecretWithKey(encrypted, fromKey)
		},
		Encrypt: func(key string) (string, error) {
			return s.deps.EncryptSecretWithKey(key, toKey)
		},
	})
	if err := credential.ReencryptStored(ctx); err != nil {
		return fmt.Errorf("rewrap restored Global SSH Credential: %w", err)
	}
	if err := notificationpkg.ReencryptStoredWebhookURL(
		ctx,
		tx,
		func(encrypted string) (string, error) { return s.deps.DecryptSecretWithKey(encrypted, fromKey) },
		func(webhookURL string) (string, error) { return s.deps.EncryptSecretWithKey(webhookURL, toKey) },
	); err != nil {
		return fmt.Errorf("rewrap restored notification webhook: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restored database rewrap: %w", err)
	}
	committed = true
	return nil
}

type fileRestoreSnapshot struct {
	Target     string
	BackupPath string
	Exists     bool
}

func snapshotFilesToDirectory(tempRoot string, targets []string) (string, []fileRestoreSnapshot, error) {
	dir, err := os.MkdirTemp(tempRoot, "slu-restore-rollback-*")
	if err != nil {
		return "", nil, err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	snapshots := make([]fileRestoreSnapshot, 0, len(targets))
	for index, target := range targets {
		snapshot := fileRestoreSnapshot{Target: target}
		info, err := os.Stat(target)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, snapshot)
			continue
		}
		if err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
		if info.IsDir() {
			_ = os.RemoveAll(dir)
			return "", nil, fmt.Errorf("restore target %q is a directory", target)
		}
		snapshot.Exists = true
		snapshot.BackupPath = filepath.Join(dir, fmt.Sprintf("snapshot-%d", index))
		if err := copyPathBounded(target, snapshot.BackupPath, MaxExtractedBytes); err != nil {
			_ = os.RemoveAll(dir)
			return "", nil, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return dir, snapshots, nil
}

func writeAtomicFileFromPath(target, source string, mode os.FileMode, ensurePrivateDirForFile func(string) error) error {
	if ensurePrivateDirForFile != nil {
		if err := ensurePrivateDirForFile(target); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(target), ".restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	sourceFile, err := os.Open(source)
	if err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	_, copyErr := copyFileBounded(tmp, sourceFile, MaxExtractedBytes)
	closeSourceErr := sourceFile.Close()
	if copyErr != nil || closeSourceErr != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return errors.Join(copyErr, closeSourceErr)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, target); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return nil
}

func restoreFileSnapshots(snapshots []fileRestoreSnapshot, ensurePrivateDirForFile func(string) error) error {
	var errs []error
	for _, snapshot := range snapshots {
		if snapshot.Exists {
			if err := writeAtomicFileFromPath(snapshot.Target, snapshot.BackupPath, 0600, ensurePrivateDirForFile); err != nil {
				errs = append(errs, err)
			}
			continue
		}
		if err := os.Remove(snapshot.Target); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (s *Service) applyArchiveFiles(ctx context.Context, files map[string]string, restoreHandoff func(context.Context) error) error {
	configData, err := readPathBounded(files["config.json"], MaxUploadBytes)
	if err != nil {
		return fmt.Errorf("read restored config: %w", err)
	}
	backupKey, err := s.ValidateConfigData(configData)
	if err != nil {
		return err
	}
	if err := s.ValidateDatabaseFile(ctx, files["servers.db"], backupKey); err != nil {
		return err
	}
	preparedDB, err := s.ReencryptDatabaseFile(ctx, files["servers.db"], backupKey, s.deps.CurrentEncryptionKey())
	if err != nil {
		return err
	}
	defer preparedDB.Remove()

	dbTarget := s.deps.DBPath()
	knownHostsTarget := filepath.Join(filepath.Dir(dbTarget), "known_hosts")
	if path, err := s.deps.KnownHostsWritePath(); err == nil && strings.TrimSpace(path) != "" {
		knownHostsTarget = path
	}
	targets := []string{dbTarget}
	targets = append(targets, SQLiteSidecarPaths(dbTarget)...)
	if _, ok := files["known_hosts"]; ok {
		targets = append(targets, knownHostsTarget)
	}
	snapshotDir, snapshots, err := snapshotFilesToDirectory(s.deps.TempDir(), targets)
	if err != nil {
		return err
	}
	defer os.RemoveAll(snapshotDir)

	if err := s.deps.RestoredRuntime.PreparePersistenceReplacement(ctx); err != nil {
		return fmt.Errorf("prepare restored persistence replacement: %w", err)
	}
	rollback := func(cause error) error {
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancelRollback()
		errs := []error{cause}
		if restoreErr := restoreFileSnapshots(snapshots, s.deps.EnsurePrivateDirForFile); restoreErr != nil {
			errs = append(errs, fmt.Errorf("rollback restore snapshots: %w", restoreErr))
		}
		if prepareErr := s.deps.RestoredRuntime.PreparePersistenceReplacement(rollbackCtx); prepareErr != nil {
			errs = append(errs, fmt.Errorf("rollback prepare restored persistence replacement: %w", prepareErr))
		}
		if restoreHandoff != nil {
			if handoffErr := restoreHandoff(rollbackCtx); handoffErr != nil {
				errs = append(errs, fmt.Errorf("rollback maintenance handoff: %w", handoffErr))
			}
		}
		if reloadErr := s.deps.RestoredRuntime.ReloadRestoredState(rollbackCtx); reloadErr != nil {
			errs = append(errs, fmt.Errorf("rollback reload runtime state after reset: %w", reloadErr))
		}
		return errors.Join(errs...)
	}
	if err := RemoveSQLiteSidecars(dbTarget); err != nil {
		return rollback(err)
	}
	if err := writeAtomicFileFromPath(dbTarget, preparedDB.Path, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
		return rollback(err)
	}
	if err := RemoveSQLiteSidecars(dbTarget); err != nil {
		return rollback(err)
	}
	if knownHosts, ok := files["known_hosts"]; ok {
		if err := writeAtomicFileFromPath(knownHostsTarget, knownHosts, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
			return rollback(err)
		}
	}
	if restoreHandoff != nil {
		if err := restoreHandoff(ctx); err != nil {
			return rollback(fmt.Errorf("maintenance restore handoff: %w", err))
		}
	}
	if err := s.deps.RestoredRuntime.ReloadRestoredState(ctx); err != nil {
		return rollback(err)
	}
	if err := s.ClearPersistedSessions(); err != nil {
		return rollback(err)
	}
	return nil
}

func restoreResourceReviewsFromPaths(manifest Manifest, files map[string]string) []ResourceReview {
	resources := make([]ResourceReview, 0, 3)
	for _, name := range []string{"servers.db", "config.json", "known_hosts"} {
		meta, declared := manifest.Files[name]
		_, included := files[name]
		resources = append(resources, ResourceReview{
			Name: name, SizeBytes: meta.Size, Required: name != "known_hosts", Included: declared && included,
		})
	}
	return resources
}

func (s *Service) VerifyArchiveFile(ctx context.Context, encrypted TemporaryFile, passphrase string) (VerifyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	passphrase = strings.TrimSpace(passphrase)
	if err := ValidatePassphrase(passphrase); err != nil {
		return VerifyResult{}, err
	}
	envelope, err := InspectEnvelopeFile(encrypted.Path)
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	result := VerifyResult{
		ArchiveFormat: envelope.Format, ArchiveVersion: envelope.Version, ArchiveCreatedAt: envelope.CreatedAt,
		ArchiveSizeBytes: encrypted.Size, Impact: defaultRestoreImpact(), Blockers: []ReadinessIssue{},
		Warnings: defaultRestoreWarnings(), Resources: []ResourceReview{}, MissingResources: []string{},
	}
	if envelope.Format != FormatName {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "unsupported_format", Message: fmt.Sprintf("Backup format %q is not supported by this application.", envelope.Format)})
		return result, nil
	}
	if envelope.Version != FormatVersion {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "unsupported_version", Message: fmt.Sprintf("Backup version %d is not supported; this application accepts version %d.", envelope.Version, FormatVersion)})
		return result, nil
	}
	plain, err := s.decryptFile(encrypted.Path, passphrase)
	if err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			result.Blockers = append(result.Blockers, ReadinessIssue{Code: "unsupported_encryption", Message: "The backup uses encryption settings that this application does not support."})
			return result, nil
		}
		return VerifyResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	defer plain.Remove()
	inspection, err := InspectTarGzFileWithLimits(plain.Path, s.deps.TempDir(), MaxUploadBytes, MaxExtractedBytes)
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	defer inspection.Remove()
	manifest := inspection.Manifest
	result.Manifest = manifest
	result.ArchiveFormat = manifest.Format
	result.ArchiveVersion = manifest.Version
	result.ArchiveCreatedAt = manifest.CreatedAt
	result.Compatible = inspection.Compatible
	result.MissingResources = append([]string(nil), inspection.MissingResources...)
	result.Resources = restoreResourceReviewsFromPaths(manifest, inspection.Files)
	result.FileNames, result.ManifestFileCount, result.TotalBytes = manifestResourceFacts(manifest)
	_, result.KnownHostsIncluded = manifest.Files["known_hosts"]
	if !inspection.Compatible {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "unsupported_manifest", Message: fmt.Sprintf("Backup manifest %q version %d is not supported.", manifest.Format, manifest.Version)})
		return result, nil
	}
	if len(inspection.MissingResources) > 0 {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "missing_required_resource", Message: "The backup is missing required resources: " + strings.Join(inspection.MissingResources, ", ") + "."})
		return result, nil
	}
	configData, err := readPathBounded(inspection.Files["config.json"], MaxUploadBytes)
	if err != nil {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "invalid_configuration", Message: "The archived configuration cannot be restored safely."})
		return result, nil
	}
	backupKey, err := s.ValidateConfigData(configData)
	if err != nil {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "invalid_configuration", Message: "The archived configuration cannot be restored safely."})
		return result, nil
	}
	result.ConfigValid = true
	if err := s.ValidateDatabaseFile(ctx, inspection.Files["servers.db"], backupKey); err != nil {
		result.Blockers = append(result.Blockers, ReadinessIssue{Code: "invalid_database", Message: "The archived database is not compatible with this application."})
		return result, nil
	}
	result.DatabaseValid = true
	counts, err := InspectDatabaseSafeCountsFile(ctx, inspection.Files["servers.db"])
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	result.SafeCounts = counts
	if !result.KnownHostsIncluded {
		result.Warnings = append(result.Warnings, ReadinessIssue{Code: "known_hosts_not_included", Message: "known_hosts is not included; the current host-key trust file will remain unchanged."})
	}
	result.RestoreReady = true
	return result, nil
}

func (s *Service) RestoreArchiveFileWithOptions(ctx context.Context, encrypted TemporaryFile, passphrase string, opts RestoreOptions) (RestoreResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	passphrase = strings.TrimSpace(passphrase)
	if err := ValidatePassphrase(passphrase); err != nil {
		return RestoreResult{}, err
	}
	plain, err := s.decryptFile(encrypted.Path, passphrase)
	if err != nil {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	defer plain.Remove()
	inspection, err := InspectTarGzFileWithLimits(plain.Path, s.deps.TempDir(), MaxUploadBytes, MaxExtractedBytes)
	if err != nil {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	defer inspection.Remove()
	if !inspection.Compatible {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageArchive, Err: ErrUnsupportedFormat}
	}
	if len(inspection.MissingResources) > 0 {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageArchive, Err: fmt.Errorf("%w: %s", ErrMissingFile, inspection.MissingResources[0])}
	}
	if opts.BeforeApply != nil {
		opts.BeforeApply()
	}
	if err := s.applyArchiveFiles(ctx, inspection.Files, opts.RestoreHandoff); err != nil {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageApply, Err: err}
	}
	globalKeyPresent := false
	if s.deps.GlobalSSHCredential == nil {
		s.deps.Logf("backup restore: Global SSH Credential status is not configured")
	} else if status, statusErr := s.deps.GlobalSSHCredential.Status(ctx); statusErr != nil {
		s.deps.Logf("backup restore: failed to read Global SSH Credential presence after restore: %v", statusErr)
	} else {
		globalKeyPresent = status.Configured
	}
	_, knownHostsRestored := inspection.Files["known_hosts"]
	return RestoreResult{
		Manifest: inspection.Manifest, GlobalKeyPresent: globalKeyPresent,
		KnownHostsRestored: knownHostsRestored, SessionsInvalidated: true,
	}, nil
}

func ReadUploadedFileToTemp(fileHeader *multipart.FileHeader, tempDir string) (TemporaryFile, error) {
	if fileHeader == nil {
		return TemporaryFile{}, errors.New("missing backup file")
	}
	if fileHeader.Size > MaxUploadBytes {
		return TemporaryFile{}, fmt.Errorf("backup file too large (max %d bytes)", MaxUploadBytes)
	}
	source, err := fileHeader.Open()
	if err != nil {
		return TemporaryFile{}, err
	}
	defer source.Close()
	target, err := createTemporaryFile(tempDir, "slu-backup-upload-*")
	if err != nil {
		return TemporaryFile{}, err
	}
	targetPath := target.Name()
	written, err := copyFileBounded(target, source, MaxUploadBytes)
	if err != nil {
		_ = target.Close()
		_ = os.Remove(targetPath)
		return TemporaryFile{}, err
	}
	if written == 0 {
		_ = target.Close()
		_ = os.Remove(targetPath)
		return TemporaryFile{}, errors.New("empty backup file")
	}
	return closeTemporaryFile(target)
}
