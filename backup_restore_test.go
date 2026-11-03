package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"database/sql"
	"debian-updater/internal/installationlock"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	internalbackup "debian-updater/internal/backup"
	observabilitypkg "debian-updater/internal/observability"
	"debian-updater/internal/vulnerabilities"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
)

func TestBackupStatusAPIExposesRecoveryHealthAndUnavailableEvidence(t *testing.T) {
	now := time.Date(2026, 7, 28, 5, 0, 0, 0, time.UTC)
	newService := func(t *testing.T, withEvidenceSchema bool) *BackupService {
		t.Helper()
		db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "backup-status.db"))
		if err != nil {
			t.Fatalf("open backup status database: %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		if withEvidenceSchema {
			if _, err := db.Exec(`
				CREATE TABLE audit_events (
					id INTEGER PRIMARY KEY AUTOINCREMENT,
					created_at TEXT NOT NULL,
					action TEXT NOT NULL,
					target_type TEXT NOT NULL,
					status TEXT NOT NULL,
					message TEXT NOT NULL,
					meta_json TEXT NOT NULL
				);
				INSERT INTO audit_events(created_at, action, target_type, status, message, meta_json)
				VALUES
					('2026-07-27T05:00:00Z', 'backup.export', 'backup', 'success', 'Backup exported', '{"bytes":4096}'),
					('2026-07-27T17:00:00Z', 'backup.verify', 'backup', 'success', 'Backup restore readiness reviewed', '{"restore_ready":true,"archive_size_bytes":4096}')
			`); err != nil {
				t.Fatalf("seed backup recovery evidence: %v", err)
			}
		}
		dataDir := t.TempDir()
		return NewBackupServiceWithDeps(internalbackup.ServiceDeps{
			DB:                            func() *sql.DB { return db },
			DBPath:                        func() string { return filepath.Join(dataDir, "servers.db") },
			ConfigPath:                    func() string { return filepath.Join(dataDir, "config.json") },
			KnownHostsWritePath:           func() (string, error) { return filepath.Join(dataDir, "known_hosts"), nil },
			Now:                           func() time.Time { return now },
			RecoveryStaleAfter:            7 * 24 * time.Hour,
			RecoveryEvidenceRetentionDays: 90,
		})
	}

	t.Run("healthy", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
		handleBackupStatusWithService(c, newService(t, true))
		if rec.Code != http.StatusOK {
			t.Fatalf("backup status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
		}
		var response internalbackup.StatusResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal backup status: %v", err)
		}
		if response.RecoveryHealth.State != internalbackup.RecoveryStateHealthy || response.RecoveryHealth.Export.SizeBytes == nil || *response.RecoveryHealth.Export.SizeBytes != 4096 {
			t.Fatalf("recovery health = %+v, want healthy evidence with size", response.RecoveryHealth)
		}
		if response.RecoveryHealth.Schedule.Scheduled || response.RecoveryHealth.Retention.ArchiveRetained || response.RecoveryHealth.Retention.AutomaticDeletion {
			t.Fatalf("recovery schedule/retention = %+v/%+v, want no scheduler or archive retention", response.RecoveryHealth.Schedule, response.RecoveryHealth.Retention)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
		handleBackupStatusWithService(c, newService(t, false))
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("backup status = %d, want %d (body=%s)", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
		}
		var response struct {
			RecoveryHealth internalbackup.RecoveryHealth `json:"recovery_health"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
			t.Fatalf("unmarshal unavailable backup status: %v", err)
		}
		if response.RecoveryHealth.State != internalbackup.RecoveryStateUnavailable {
			t.Fatalf("unavailable recovery health = %+v", response.RecoveryHealth)
		}
	})
}

func TestBackupRestoreRollbackUsesSameRestoredRuntimeInterface(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "rollback-runtime.db"))

	if err := saveServers(); err != nil {
		t.Fatalf("save initial Servers: %v", err)
	}
	backupKey := append([]byte(nil), getEncryptionKey()...)
	restoredDB := buildBackupDatabaseDataWithKey(t, backupKey, Server{Name: "replacement", Host: "example.org", Port: 22, User: "root"}, "")
	config, err := json.Marshal(map[string]string{"encryption_key": base64.StdEncoding.EncodeToString(backupKey)})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	forwardErr := errors.New("forward runtime reload failed")
	rollbackErr := errors.New("rollback runtime reload failed")
	reloads := 0
	service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		TempDir: t.TempDir,
		RestoredRuntime: restoredRuntimeAdapter{
			prepare: resetRuntimeCaches,
			reload: func(context.Context) error {
				reloads++
				if reloads == 1 {
					return forwardErr
				}
				return rollbackErr
			},
		},
	})

	err = service.ApplyFiles(context.Background(), map[string][]byte{"servers.db": restoredDB, "config.json": config})
	if !errors.Is(err, forwardErr) || !errors.Is(err, rollbackErr) {
		t.Fatalf("ApplyFiles() error = %v, want joined forward and rollback reload failures", err)
	}
	if reloads != 2 {
		t.Fatalf("restored-runtime reload calls = %d, want forward and rollback", reloads)
	}
}

func TestBackupRestoreRollbackRecoversRuntimeAfterRequestCancellation(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "cancelled-restore.db"))

	if err := saveServers(); err != nil {
		t.Fatalf("save initial Servers: %v", err)
	}
	backupKey := append([]byte(nil), getEncryptionKey()...)
	restoredDB := buildBackupDatabaseDataWithKey(t, backupKey, Server{Name: "replacement", Host: "example.org", Port: 22, User: "root"}, "")
	config, err := json.Marshal(map[string]string{"encryption_key": base64.StdEncoding.EncodeToString(backupKey)})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	prepareCalls := 0
	reloadCalls := 0
	rollbackReloadHadLiveContext := false
	service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		RestoredRuntime: restoredRuntimeAdapter{
			prepare: func() { prepareCalls++ },
			reload: func(reloadCtx context.Context) error {
				reloadCalls++
				if reloadCalls == 1 {
					cancel()
					return ctx.Err()
				}
				rollbackReloadHadLiveContext = reloadCtx.Err() == nil
				return nil
			},
		},
	})

	err = service.ApplyFiles(ctx, map[string][]byte{"servers.db": restoredDB, "config.json": config})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyFiles() error = %v, want context canceled", err)
	}
	if prepareCalls != 2 {
		t.Fatalf("restored-runtime prepare calls = %d, want forward and rollback", prepareCalls)
	}
	if reloadCalls != 2 {
		t.Fatalf("restored-runtime reload calls = %d, want forward and rollback", reloadCalls)
	}
	if !rollbackReloadHadLiveContext {
		t.Fatal("rollback runtime reload inherited the cancelled request context")
	}
}

func buildBackupDatabaseDataWithKey(t *testing.T, key []byte, server Server, backupGlobalKey string) []byte {
	t.Helper()
	dbFile := filepath.Join(t.TempDir(), "backup-source.db")
	backupDB, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open backup source db: %v", err)
	}
	backupDB.SetMaxOpenConns(1)
	backupDB.SetMaxIdleConns(1)
	if _, err := backupDB.Exec(`
		CREATE TABLE IF NOT EXISTS servers (
			name TEXT PRIMARY KEY,
			host TEXT NOT NULL,
			port INTEGER NOT NULL DEFAULT 22,
			user TEXT NOT NULL,
			pass_enc TEXT NOT NULL,
			key_enc TEXT NOT NULL DEFAULT '',
			key_path TEXT NOT NULL DEFAULT '',
			tags TEXT NOT NULL DEFAULT ''
		)
	`); err != nil {
		_ = backupDB.Close()
		t.Fatalf("create backup servers table: %v", err)
	}
	if err := ensureSchema(backupDB); err != nil {
		_ = backupDB.Close()
		t.Fatalf("ensureSchema(backup source) unexpected error: %v", err)
	}
	passEnc, err := encryptSecretWithKey(server.Pass, key)
	if err != nil {
		_ = backupDB.Close()
		t.Fatalf("encrypt backup password: %v", err)
	}
	keyEnc, err := encryptSecretWithKey(server.Key, key)
	if err != nil {
		_ = backupDB.Close()
		t.Fatalf("encrypt backup SSH key: %v", err)
	}
	if _, err := backupDB.Exec(
		"INSERT INTO servers(name, host, port, user, pass_enc, key_enc, key_path, tags) VALUES(?, ?, ?, ?, ?, ?, '', ?)",
		server.Name,
		server.Host,
		normalizePort(server.Port),
		server.User,
		passEnc,
		keyEnc,
		joinTags(server.Tags),
	); err != nil {
		_ = backupDB.Close()
		t.Fatalf("insert backup server: %v", err)
	}
	if strings.TrimSpace(backupGlobalKey) != "" {
		globalKeyEnc, err := encryptSecretWithKey(backupGlobalKey, key)
		if err != nil {
			_ = backupDB.Close()
			t.Fatalf("encrypt backup global key: %v", err)
		}
		if _, err := backupDB.Exec(
			"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			"global_ssh_key",
			globalKeyEnc,
		); err != nil {
			_ = backupDB.Close()
			t.Fatalf("insert backup global key: %v", err)
		}
	}
	if err := backupDB.Close(); err != nil {
		t.Fatalf("close backup source db: %v", err)
	}
	data, err := os.ReadFile(dbFile)
	if err != nil {
		t.Fatalf("read backup source db: %v", err)
	}
	return data
}

func preserveEncryptionState(t *testing.T) {
	t.Helper()
	origKey := encryptionKey
	encryptionKey = nil
	keyOnce = sync.Once{}
	t.Cleanup(func() {
		encryptionKey = origKey
		keyOnce = sync.Once{}
		if origKey != nil {
			keyOnce.Do(func() {})
		}
	})
}

func TestGetEncryptionKeyConcurrentInitializationUsesPersistedKey(t *testing.T) {
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "encryption-race.db"))

	const callers = 16
	results := make(chan []byte, callers)
	for range callers {
		go func() {
			results <- append([]byte(nil), getEncryptionKey()...)
		}()
	}

	var first []byte
	for i := 0; i < callers; i++ {
		got := <-results
		if len(got) != 32 {
			t.Fatalf("getEncryptionKey() len = %d, want 32", len(got))
		}
		if i == 0 {
			first = got
			continue
		}
		if !bytes.Equal(got, first) {
			t.Fatalf("concurrent getEncryptionKey() returned different keys")
		}
	}

	raw, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("ReadFile(configPath()) unexpected error: %v", err)
	}
	var cfg map[string]string
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("config unmarshal unexpected error: %v", err)
	}
	persisted, err := base64.StdEncoding.DecodeString(cfg["encryption_key"])
	if err != nil {
		t.Fatalf("persisted key decode unexpected error: %v", err)
	}
	if !bytes.Equal(persisted, first) {
		t.Fatalf("persisted encryption key differs from in-memory key")
	}
}

func TestRuntimeDataFilesAreOwnerOnly(t *testing.T) {
	preserveDBState(t)
	preserveEncryptionState(t)
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(runtimeDir, "servers.db"))

	_ = getEncryptionKey()
	_ = getDB()

	for _, tc := range []struct {
		path string
		mode os.FileMode
	}{
		{path: runtimeDir, mode: 0700},
		{path: configPath(), mode: 0600},
		{path: dbPath(), mode: 0600},
	} {
		info, err := os.Stat(tc.path)
		if err != nil {
			t.Fatalf("stat %s: %v", tc.path, err)
		}
		if got := info.Mode().Perm(); got != tc.mode {
			t.Fatalf("%s mode = %o, want %o", tc.path, got, tc.mode)
		}
	}
}

func TestLoadServersLeavesEmptyInventoryEmpty(t *testing.T) {
	preserveDBState(t)
	preserveEncryptionState(t)
	preserveServerState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "empty-inventory.db"))

	if err := loadServers(); err != nil {
		t.Fatalf("load empty inventory: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(servers) != 0 {
		t.Fatalf("loadServers seeded default servers: %+v", servers)
	}
}

func TestBackupPayloadRoundTrip(t *testing.T) {
	files := map[string][]byte{
		"servers.db":  []byte("sqlite-snapshot"),
		"config.json": []byte(`{"encryption_key":"abc"}`),
		"known_hosts": []byte("host ssh-ed25519 AAAATEST"),
	}

	tarGz, err := buildBackupTarGz(map[string][]byte{
		"servers.db":  append([]byte(nil), files["servers.db"]...),
		"config.json": append([]byte(nil), files["config.json"]...),
		"known_hosts": append([]byte(nil), files["known_hosts"]...),
	})
	if err != nil {
		t.Fatalf("buildBackupTarGz() unexpected error: %v", err)
	}

	encrypted, err := encryptBackupPayload(tarGz, "very-strong-passphrase")
	if err != nil {
		t.Fatalf("encryptBackupPayload() unexpected error: %v", err)
	}

	plain, err := decryptBackupPayload(encrypted, "very-strong-passphrase")
	if err != nil {
		t.Fatalf("decryptBackupPayload(valid) unexpected error: %v", err)
	}
	if !bytes.Equal(plain, tarGz) {
		t.Fatalf("decryptBackupPayload(valid) payload mismatch")
	}

	restoredFiles, manifest, err := extractBackupTarGz(plain)
	if err != nil {
		t.Fatalf("extractBackupTarGz() unexpected error: %v", err)
	}
	for name, want := range files {
		got, ok := restoredFiles[name]
		if !ok {
			t.Fatalf("extractBackupTarGz() missing file %q", name)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("extractBackupTarGz() file %q mismatch", name)
		}
	}
	if manifest.Format != backupFormatName {
		t.Fatalf("manifest.Format = %q, want %q", manifest.Format, backupFormatName)
	}

	if _, err := decryptBackupPayload(encrypted, "wrong-passphrase"); err == nil {
		t.Fatalf("decryptBackupPayload(wrong passphrase) error = nil, want non-nil")
	}
}

func TestExtractBackupTarGzRejectsOversizedDecompressedPayload(t *testing.T) {
	tarGz, err := buildBackupTarGz(map[string][]byte{
		"servers.db":  []byte("sqlite-snapshot"),
		"config.json": []byte(`{"encryption_key":"abc"}`),
		"known_hosts": []byte("host ssh-ed25519 AAAATEST"),
	})
	if err != nil {
		t.Fatalf("buildBackupTarGz() unexpected error: %v", err)
	}

	_, _, err = extractBackupTarGzWithLimits(tarGz, 1024, 1)
	if err == nil {
		t.Fatalf("extractBackupTarGzWithLimits() error = nil, want decompressed size error")
	}
	if !strings.Contains(err.Error(), "backup payload is too large") {
		t.Fatalf("extractBackupTarGzWithLimits() error = %v, want payload size error", err)
	}
}

func TestExtractBackupTarGzRejectsUnknownRegularEntries(t *testing.T) {
	manifest := backupManifest{
		Format:  backupFormatName,
		Version: backupFormatVersion,
		Files:   map[string]backupManifestFile{},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) unexpected error: %v", err)
	}

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{name: "ignored.bin", data: bytes.Repeat([]byte("x"), 32)},
		{name: "manifest.json", data: manifestData},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			t.Fatalf("WriteHeader(%q) unexpected error: %v", entry.name, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatalf("Write(%q) unexpected error: %v", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Close() unexpected error: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close() unexpected error: %v", err)
	}

	_, _, err = extractBackupTarGzWithLimits(raw.Bytes(), 1024, 16)
	if err == nil {
		t.Fatalf("extractBackupTarGzWithLimits() error = nil, want unknown entry rejection")
	}
	if !strings.Contains(err.Error(), `unexpected backup entry "ignored.bin"`) {
		t.Fatalf("extractBackupTarGzWithLimits() error = %v, want unknown entry error", err)
	}
}

func TestExtractBackupTarGzRejectsUnmanifestedRestoredFiles(t *testing.T) {
	files := map[string][]byte{
		"servers.db":  []byte("sqlite-snapshot"),
		"config.json": []byte(`{"encryption_key":"abc"}`),
	}
	configSum := sha256.Sum256(files["config.json"])
	manifest := backupManifest{
		Format:  backupFormatName,
		Version: backupFormatVersion,
		Files: map[string]backupManifestFile{
			"config.json": {
				Size:   int64(len(files["config.json"])),
				SHA256: hex.EncodeToString(configSum[:]),
			},
		},
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) unexpected error: %v", err)
	}

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, entry := range []struct {
		name string
		data []byte
	}{
		{name: "manifest.json", data: manifestData},
		{name: "servers.db", data: files["servers.db"]},
		{name: "config.json", data: files["config.json"]},
	} {
		if err := tw.WriteHeader(&tar.Header{Name: entry.name, Mode: 0600, Size: int64(len(entry.data))}); err != nil {
			t.Fatalf("WriteHeader(%q) unexpected error: %v", entry.name, err)
		}
		if _, err := tw.Write(entry.data); err != nil {
			t.Fatalf("Write(%q) unexpected error: %v", entry.name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar.Close() unexpected error: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip.Close() unexpected error: %v", err)
	}

	_, _, err = extractBackupTarGzWithLimits(raw.Bytes(), 1024, 4096)
	if err == nil {
		t.Fatalf("extractBackupTarGzWithLimits() error = nil, want missing manifest entry error")
	}
	if !strings.Contains(err.Error(), "servers.db") {
		t.Fatalf("extractBackupTarGzWithLimits() error = %v, want servers.db missing from manifest", err)
	}
}

func TestApplyBackupFilesRemovesSQLiteSidecars(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveEncryptionState(t)
	dbFile := filepath.Join(t.TempDir(), "restore-sidecars.db")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", dbFile)

	servers = []Server{{Name: "srv-sidecar", Host: "example.org", Port: 22, User: "root", Pass: "pw"}}
	if err := saveServers(); err != nil {
		t.Fatalf("saveServers() unexpected error: %v", err)
	}
	dbSnapshot, err := createDBBackupSnapshot()
	if err != nil {
		t.Fatalf("createDBBackupSnapshot() unexpected error: %v", err)
	}
	configData, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("ReadFile(configPath()) unexpected error: %v", err)
	}

	resetRuntimeCaches()
	for _, sidecar := range sqliteSidecarPaths(dbFile) {
		if err := os.WriteFile(sidecar, []byte("stale sidecar"), 0600); err != nil {
			t.Fatalf("WriteFile(%s) unexpected error: %v", sidecar, err)
		}
	}

	if err := applyBackupFiles(context.Background(), map[string][]byte{
		"servers.db":  dbSnapshot,
		"config.json": configData,
	}); err != nil {
		t.Fatalf("applyBackupFiles() unexpected error: %v", err)
	}
	for _, sidecar := range sqliteSidecarPaths(dbFile) {
		data, err := os.ReadFile(sidecar)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			t.Fatalf("ReadFile(%s) unexpected error: %v", sidecar, err)
		}
		if bytes.Equal(data, []byte("stale sidecar")) {
			t.Fatalf("sidecar %s still contains stale pre-restore bytes", sidecar)
		}
	}
}

// A restore test needs a reopenable, app-owned provider: the package-global
// cache may legitimately still belong to another runtime in the same process.
func newMetricsRestoreTestRuntime(t *testing.T, dbFile string) AppDeps {
	t.Helper()
	t.Setenv("DEBIAN_UPDATER_DB_PATH", dbFile)
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", filepath.Join(filepath.Dir(dbFile), "known_hosts"))
	var ownerMu sync.Mutex
	var ownedDB *sql.DB
	database := func() *sql.DB {
		ownerMu.Lock()
		defer ownerMu.Unlock()
		if ownedDB == nil {
			var err error
			ownedDB, err = sql.Open("sqlite", dbFile)
			if err != nil {
				t.Fatalf("open restore fixture: %v", err)
			}
			ownedDB.SetMaxOpenConns(1)
			ownedDB.SetMaxIdleConns(1)
			if err := ensureSchema(ownedDB); err != nil {
				t.Fatalf("migrate restore fixture: %v", err)
			}
		}
		return ownedDB
	}
	closeDatabase := func() {
		ownerMu.Lock()
		defer ownerMu.Unlock()
		if ownedDB != nil {
			if err := ownedDB.Close(); err != nil {
				t.Errorf("close restore fixture: %v", err)
			}
			ownedDB = nil
		}
	}
	// Register before composition so its Notification Delivery Lifecycle drains
	// before the final connection closes. Restore closes/reopens only this DB.
	t.Cleanup(closeDatabase)
	composition := newRuntimeComposition(AppDeps{
		DB:                   database,
		DBPath:               func() string { return dbFile },
		ApplicationTime:      apptimepkg.New(apptimepkg.Deps{Store: apptimepkg.NewMemoryStore("UTC")}),
		SetCurrentJobManager: func(*JobManager) {},
		SetSessionManager: func(sm *scs.SessionManager) {
			if cleanup, ok := sm.Store.(interface{ StopCleanup() }); ok {
				t.Cleanup(cleanup.StopCleanup)
			}
		},
	})
	composition.resetCaches = closeDatabase
	return composeRuntimeForTest(t, composition)
}

func TestAppScopedBackupRestoreInvalidatesMetricsAccessCredential(t *testing.T) {
	for _, tc := range []struct {
		name        string
		preexisting bool
		used        bool
	}{
		{name: "new credential"},
		{name: "rotated credential", preexisting: true},
		{name: "used credential", preexisting: true, used: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			preserveDBState(t)
			preserveSessionState(t)
			preserveEncryptionState(t)
			// Keep a different runtime cached globally, with a sentinel that
			// must survive both fixture creation and persistence replacement.
			foreignFile := filepath.Join(t.TempDir(), "unrelated.db")
			t.Setenv("DEBIAN_UPDATER_DB_PATH", foreignFile)
			foreignDB := getDB()
			foreignStore := observabilitypkg.SQLiteMetricsCredentialStore{DB: func() *sql.DB { return foreignDB }}
			foreignRecord := observabilitypkg.MetricsCredentialRecord{
				Hash:      "synthetic-unrelated-hash",
				Lifecycle: observabilitypkg.MetricsCredentialLifecycle{CreatedAt: "2020-01-02T03:04:05Z"},
			}
			if err := foreignStore.Replace(context.Background(), foreignRecord); err != nil {
				t.Fatal(err)
			}
			if _, err := foreignDB.Exec("INSERT INTO settings(key,value) VALUES('restore-test-sentinel','unchanged')"); err != nil {
				t.Fatal(err)
			}
			foreignBefore, err := foreignStore.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}

			dbFile := filepath.Join(t.TempDir(), "restore-metrics-cache.db")
			deps := newMetricsRestoreTestRuntime(t, dbFile)
			deps.ServerState.Lock()
			deps.ServerState.SetServers([]Server{{Name: "srv-cache", Host: "example.org", Port: 22, User: "root", Pass: "pw"}})
			deps.ServerState.Unlock()
			if err := deps.ServerInventoryService.SaveWithTxHook(nil); err != nil {
				t.Fatalf("save fixture inventory: %v", err)
			}
			if deps.DB() == foreignDB || deps.BackupService.Status().DBPath != dbFile {
				t.Fatal("restore fixture does not own coherent persistence")
			}
			if status, err := deps.MetricsAccessCredential.Status(context.Background()); err != nil || status != observabilitypkg.MetricsAccessDisabled {
				t.Fatalf("fresh fixture credential status = %q, %v", status, err)
			}
			if tc.preexisting {
				if _, err := deps.MetricsAccessCredential.Rotate(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			restoredToken, err := deps.MetricsAccessCredential.Rotate(context.Background())
			if err != nil {
				t.Fatalf("rotate archived credential: %v", err)
			}
			if tc.used {
				if result, err := deps.MetricsAccessCredential.VerifyWithOrigin(context.Background(), restoredToken, "198.51.100.42"); err != nil || result != observabilitypkg.MetricsAccessAccepted {
					t.Fatalf("use archived credential = %q, %v", result, err)
				}
			}
			archivedDetails, err := deps.MetricsAccessCredential.Details(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if archivedDetails.CreatedAt == "" || (archivedDetails.RotatedAt != "") != tc.preexisting {
				t.Fatal("archive fixture lost creation/rotation distinction")
			}
			if tc.used {
				if archivedDetails.LastUsedAt == "" || archivedDetails.LastUsedOriginMasked != "198.51.100.x" || archivedDetails.LifecycleState != observabilitypkg.MetricsAccessLifecycleCurrent {
					t.Fatal("used archive fixture lost usage metadata")
				}
			} else if archivedDetails.LifecycleState != observabilitypkg.MetricsAccessLifecycleNeverUsed || archivedDetails.LastUsedAt != "" || archivedDetails.LastUsedOriginMasked != "" {
				t.Fatal("unused archive fixture gained usage metadata")
			}
			archivedRecord, err := (observabilitypkg.SQLiteMetricsCredentialStore{DB: deps.DB}).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			dbSnapshot, err := deps.BackupService.CreateDBSnapshot()
			if err != nil {
				t.Fatalf("snapshot fixture: %v", err)
			}
			configData, err := os.ReadFile(configPath())
			if err != nil {
				t.Fatal(err)
			}
			staleToken, err := deps.MetricsAccessCredential.Rotate(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if result, err := deps.MetricsAccessCredential.VerifyWithOrigin(context.Background(), staleToken, "203.0.113.42"); err != nil || result != observabilitypkg.MetricsAccessAccepted {
				t.Fatalf("stale credential before restore = %q, %v", result, err)
			}
			staleDetails, err := deps.MetricsAccessCredential.Details(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if staleDetails == archivedDetails {
				t.Fatal("fixture did not distinguish archive from newer credential lifecycle")
			}
			if err := deps.BackupService.ApplyFiles(context.Background(), map[string][]byte{"servers.db": dbSnapshot, "config.json": configData}); err != nil {
				t.Fatalf("restore fixture: %v", err)
			}
			if status, err := deps.MetricsAccessCredential.Status(context.Background()); err != nil || status != observabilitypkg.MetricsAccessEnabled {
				t.Fatalf("credential status after restore = %q, %v", status, err)
			}
			restoredDetails, err := deps.MetricsAccessCredential.Details(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if restoredDetails != archivedDetails {
				t.Fatalf("credential lifecycle after restore = %+v, want exact archived lifecycle %+v", restoredDetails, archivedDetails)
			}
			restoredRecord, err := (observabilitypkg.SQLiteMetricsCredentialStore{DB: deps.DB}).Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if restoredRecord != archivedRecord {
				t.Fatal("restored credential hash/lifecycle differs from archived persistence")
			}
			if result, err := deps.MetricsAccessCredential.Verify(context.Background(), staleToken); err != nil || result != observabilitypkg.MetricsAccessRejected {
				t.Fatalf("stale credential after restore = %q, %v", result, err)
			}
			if result, err := deps.MetricsAccessCredential.Verify(context.Background(), restoredToken); err != nil || result != observabilitypkg.MetricsAccessAccepted {
				t.Fatalf("archived credential after restore = %q, %v", result, err)
			}
			if db != foreignDB || foreignDB.Ping() != nil {
				t.Fatal("app restore replaced or closed unrelated global persistence")
			}
			foreignAfter, err := foreignStore.Load(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if foreignAfter != foreignBefore {
				t.Fatal("app restore changed unrelated credential persistence")
			}
			var sentinel string
			if err := foreignDB.QueryRow("SELECT value FROM settings WHERE key='restore-test-sentinel'").Scan(&sentinel); err != nil || sentinel != "unchanged" {
				t.Fatalf("unrelated DB sentinel changed: %v", err)
			}
		})
	}
}

func TestAppScopedBackupRestoreReloadsServerStateAndSessionManager(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveEncryptionState(t)
	dbFile := filepath.Join(t.TempDir(), "restore-app-runtime.db")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", dbFile)

	routeState := newServerState()
	deps := appDepsWithDefaultsForTest(t, AppDeps{ServerState: routeState})
	if _, err := setupRouterWithDeps(deps); err != nil {
		t.Fatalf("setupRouterWithDeps() unexpected error: %v", err)
	}
	installation, err := installationlock.Acquire(dbFile, false)
	if err != nil {
		t.Fatal(err)
	}
	defer installation.Close()
	lockBefore, err := os.Stat(dbFile + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	initialSessionManager := deps.CurrentSessionManager()
	if initialSessionManager == nil {
		t.Fatalf("app-scoped session manager was not initialized")
	}

	backupKey := append([]byte(nil), getEncryptionKey()...)
	restoredServer := Server{
		Name: "srv-restored-runtime",
		Host: "runtime-restored.example.org",
		Port: 2202,
		User: "root",
		Pass: "restored-password",
		Key:  "restored-private-key",
		Tags: []string{"restored"},
	}
	backupDB := buildBackupDatabaseDataWithKey(t, backupKey, restoredServer, "")
	backupConfig, err := json.Marshal(map[string]string{
		"encryption_key": base64.StdEncoding.EncodeToString(backupKey),
	})
	if err != nil {
		t.Fatalf("marshal backup config: %v", err)
	}

	if err := deps.BackupService.ApplyFiles(context.Background(), map[string][]byte{
		"servers.db":  backupDB,
		"config.json": backupConfig,
	}); err != nil {
		t.Fatalf("ApplyFiles() unexpected error: %v", err)
	}
	lockAfter, err := os.Stat(dbFile + ".lock")
	if err != nil || !os.SameFile(lockBefore, lockAfter) {
		t.Fatal("controlled restore replaced installation lock", err)
	}
	if other, err := installationlock.Acquire(dbFile, false); !errors.Is(err, installationlock.ErrBusy) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatal("controlled restore lost installation exclusivity", err)
	}
	loaded := deps.ServerState.CloneServers()
	if len(loaded) != 1 || loaded[0].Name != restoredServer.Name || loaded[0].Pass != restoredServer.Pass || loaded[0].Key != restoredServer.Key {
		t.Fatalf("app-scoped servers after restore = %+v, want restored server", loaded)
	}
	status := deps.ServerState.CurrentStatusSnapshot(restoredServer.Name)
	if status == nil || status.Status != "idle" || status.HasPassword != true || status.HasKey != true {
		t.Fatalf("app-scoped status after restore = %+v, want idle restored credentials", status)
	}
	if got := deps.CurrentSessionManager(); got == nil || got == initialSessionManager {
		t.Fatalf("app-scoped session manager after restore = %p, want fresh manager different from %p", got, initialSessionManager)
	}
}

func TestApplyBackupFilesReencryptsRestoredDatabaseWithLocalKey(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveEncryptionState(t)
	dbFile := filepath.Join(t.TempDir(), "restore-rewrap.db")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", dbFile)

	currentKey := append([]byte(nil), getEncryptionKey()...)
	currentConfigData, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("ReadFile(configPath()) unexpected error: %v", err)
	}
	backupKey := bytes.Repeat([]byte{7}, 32)
	if bytes.Equal(backupKey, currentKey) {
		t.Fatalf("backup key unexpectedly matches current key")
	}
	restoredServer := Server{
		Name: "srv-restored-rewrap",
		Host: "restored.example.org",
		Port: 2222,
		User: "root",
		Pass: "restored-password",
		Key:  "restored-private-key",
		Tags: []string{"prod"},
	}
	backupDB := buildBackupDatabaseDataWithKey(t, backupKey, restoredServer, "restored-global-key")
	factorDBPath := filepath.Join(t.TempDir(), "source-with-factor.db")
	if err := os.WriteFile(factorDBPath, backupDB, 0600); err != nil {
		t.Fatal(err)
	}
	factorDB, err := sql.Open("sqlite", factorDBPath)
	if err != nil {
		t.Fatal(err)
	}
	factorCipher, err := encryptSecretWithKey("SYNTHETICBACKUPFACTOR", backupKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = factorDB.Exec("INSERT INTO auth_users(id,username,password_hash,created_at,updated_at,totp_secret_enc) VALUES(1,'restored-admin','synthetic-hash','created','updated',?)", factorCipher); err != nil {
		t.Fatal(err)
	}
	if err = factorDB.Close(); err != nil {
		t.Fatal(err)
	}
	backupDB, err = os.ReadFile(factorDBPath)
	if err != nil {
		t.Fatal(err)
	}
	backupConfig, err := json.Marshal(map[string]string{
		"encryption_key": base64.StdEncoding.EncodeToString(backupKey),
	})
	if err != nil {
		t.Fatalf("marshal backup config: %v", err)
	}

	if err := applyBackupFiles(context.Background(), map[string][]byte{
		"servers.db":  backupDB,
		"config.json": backupConfig,
	}); err != nil {
		t.Fatalf("applyBackupFiles() unexpected error: %v", err)
	}
	afterConfigData, err := os.ReadFile(configPath())
	if err != nil {
		t.Fatalf("ReadFile(configPath()) after restore unexpected error: %v", err)
	}
	if !bytes.Equal(afterConfigData, currentConfigData) {
		t.Fatalf("restore changed local config.json; restored DB should be rewrapped to local key")
	}

	mu.Lock()
	if len(servers) != 1 || servers[0].Name != restoredServer.Name || servers[0].Pass != restoredServer.Pass || servers[0].Key != restoredServer.Key {
		t.Fatalf("loaded servers after rewrap = %+v, want restored server with decrypted secrets", servers)
	}
	mu.Unlock()
	resolved, err := appDepsWithDefaultsForTest(t, AppDeps{}).GlobalSSHCredential.Resolve(context.Background(), "")
	if err != nil || resolved.Key != "restored-global-key" {
		t.Fatalf("GlobalSSHCredential.Resolve() = %+v, %v, want restored global key", resolved, err)
	}

	var passEnc string
	if err := getDB().QueryRow("SELECT pass_enc FROM servers WHERE name = ?", restoredServer.Name).Scan(&passEnc); err != nil {
		t.Fatalf("query restored pass_enc: %v", err)
	}
	if got, err := decryptSecretWithKey(passEnc, currentKey); err != nil || got != restoredServer.Pass {
		t.Fatalf("decrypt restored password with current key = %q, %v; want %q", got, err, restoredServer.Pass)
	}
	if got, err := decryptSecretWithKey(passEnc, backupKey); err == nil && got == restoredServer.Pass {
		t.Fatalf("restored password still decrypts with backup key")
	}
	var restoredFactor string
	if err := getDB().QueryRow("SELECT totp_secret_enc FROM auth_users WHERE id=1").Scan(&restoredFactor); err != nil {
		t.Fatal(err)
	}
	if plain, err := decryptSecretWithKey(restoredFactor, currentKey); err != nil || plain != "SYNTHETICBACKUPFACTOR" {
		t.Fatal("MFA factor not rewrapped with destination key", err)
	}
	if _, err := decryptSecretWithKey(restoredFactor, backupKey); err == nil {
		t.Fatal("factor still decrypts with archived key")
	}

}

func TestBackupAPIExportRestoreLifecycle(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "backup-lifecycle.db"))

	r, err := setupRouter()
	if err != nil {
		t.Fatalf("setupRouter() unexpected error: %v", err)
	}
	handler := sessionHandler(r)

	setupBody := bytes.NewBufferString(`{"username":"admin","password":"` + testPasswordStrong + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", setupBody)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionCookie := testSessionCookieFromRecorder(t, rec)

	if _, err := getDB().Exec(
		"INSERT INTO servers(name, host, port, user, pass_enc, key_enc, key_path, tags) VALUES(?, ?, ?, ?, '', '', '', '')",
		"before-export", "before.example", 22, "root",
	); err != nil {
		t.Fatalf("insert before-export server unexpected error: %v", err)
	}
	if err := loadServers(); err != nil {
		t.Fatalf("load before-export inventory: %v", err)
	}

	assessment := vulnerabilities.Assessment{Ecosystem: "Debian:13", SourcePackage: "example", SourceVersion: "1.0-1", VendorStatus: "unimportant", VendorReason: "Synthetic backup fixture", Applicability: "source_match", Confidence: "vendor_qualified", Source: "https://example.invalid/advisory", ObservedAt: "2026-10-01T00:00:00Z"}
	repo := vulnerabilities.Repository{DB: getDB}
	if _, err := repo.Save(context.Background(), "before-export", vulnerabilities.Scan{State: "complete", Scope: "installed_packages", Inventoried: 1, Evaluated: 1, FinishedAt: assessment.ObservedAt, Findings: []vulnerabilities.Finding{{ID: "CVE-2026-1000", Package: "example", Assessment: assessment}}}); err != nil {
		t.Fatal(err)
	}
	proof := &vulnerabilities.EvaluationProof{ScanID: "historical-proof", ServerName: "before-export", Package: "example", InstalledVersion: "2", SourcePackage: "example", SourceVersion: "2", Ecosystem: "Debian:13", Source: "https://example.invalid/querybatch", ObservedAt: "2026-10-02T00:00:00Z", VerifiedAt: "2026-10-02T00:00:00Z", Complete: true, Stable: true, ProvenanceVerified: true, FindingIDs: []string{}}
	if _, err := repo.Save(context.Background(), "before-export", vulnerabilities.Scan{ID: proof.ScanID, ServerName: proof.ServerName, State: "partial", Scope: "installed_packages", StartedAt: "2026-10-01T23:59:00Z", FinishedAt: proof.VerifiedAt, Inventoried: 2, Evaluated: 1, Unknown: 1, Packages: []vulnerabilities.Package{{Name: "example", Version: "2", SourcePackage: "example", SourceVersion: "2", State: "evaluated", Proof: proof}, {Name: "unknown", Version: "1", State: "unknown"}}}); err != nil {
		t.Fatal(err)
	}
	// Preserve a later legacy failed attempt as well as its earlier accepted
	// proof. There is deliberately no running operation to recover on restore.
	if _, err := repo.Save(context.Background(), "before-export", vulnerabilities.Scan{State: "unavailable", FinishedAt: "2026-10-03T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/export", bytes.NewBufferString(`{"passphrase":"very-strong-passphrase","include_known_hosts":false}`))
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup export status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("backup export Content-Type = %q, want %q", got, "application/octet-stream")
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.Contains(got, backupFileExtension) {
		t.Fatalf("backup export Content-Disposition = %q, want extension %q", got, backupFileExtension)
	}
	exportJobID := strings.TrimSpace(rec.Header().Get("X-Job-ID"))
	if exportJobID == "" {
		t.Fatalf("backup export missing X-Job-ID header")
	}
	backupBlob := append([]byte(nil), rec.Body.Bytes()...)
	if len(backupBlob) == 0 {
		t.Fatalf("backup export payload is empty")
	}
	var exportJobStatus, exportJobKind string
	if err := getDB().QueryRow("SELECT status, kind FROM jobs WHERE id = ?", exportJobID).Scan(&exportJobStatus, &exportJobKind); err != nil {
		t.Fatalf("query export job: %v", err)
	}
	if exportJobKind != jobKindBackupExport || exportJobStatus != jobStatusSucceeded {
		t.Fatalf("export job kind/status = %q/%q, want %q/%q", exportJobKind, exportJobStatus, jobKindBackupExport, jobStatusSucceeded)
	}

	if _, err := getDB().Exec(
		"INSERT INTO servers(name, host, port, user, pass_enc, key_enc, key_path, tags) VALUES(?, ?, ?, ?, '', '', '', '')",
		"after-export", "after.example", 22, "root",
	); err != nil {
		t.Fatalf("insert after-export server unexpected error: %v", err)
	}
	if err := loadServers(); err != nil {
		t.Fatalf("load after-export inventory: %v", err)
	}

	var verifyBody bytes.Buffer
	verifyWriter := multipart.NewWriter(&verifyBody)
	verifyPart, err := verifyWriter.CreateFormFile("file", "test"+backupFileExtension)
	if err != nil {
		t.Fatalf("CreateFormFile(verify) unexpected error: %v", err)
	}
	if _, err := verifyPart.Write(backupBlob); err != nil {
		t.Fatalf("verify part.Write() unexpected error: %v", err)
	}
	if err := verifyWriter.WriteField("passphrase", "very-strong-passphrase"); err != nil {
		t.Fatalf("verify WriteField(passphrase) unexpected error: %v", err)
	}
	if err := verifyWriter.Close(); err != nil {
		t.Fatalf("verify writer.Close() unexpected error: %v", err)
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/verify", &verifyBody)
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", verifyWriter.FormDataContentType())
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup verify status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var verifyResp struct {
		Valid              bool     `json:"valid"`
		Compatible         bool     `json:"compatible"`
		RestoreReady       bool     `json:"restore_ready"`
		ManifestFiles      int      `json:"manifest_files"`
		FileNames          []string `json:"file_names"`
		KnownHostsIncluded bool     `json:"known_hosts_included"`
		DatabaseValid      bool     `json:"database_valid"`
		ConfigValid        bool     `json:"config_valid"`
		Archive            struct {
			Format    string `json:"format"`
			Version   int    `json:"version"`
			CreatedAt string `json:"created_at"`
			SizeBytes int64  `json:"size_bytes"`
		} `json:"archive"`
		SafeCounts internalbackup.SafeCounts       `json:"safe_counts"`
		Impact     internalbackup.RestoreImpact    `json:"impact"`
		Blockers   []internalbackup.ReadinessIssue `json:"blockers"`
		Warnings   []internalbackup.ReadinessIssue `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verifyResp); err != nil {
		t.Fatalf("unmarshal verify response: %v", err)
	}
	if !verifyResp.Valid || !verifyResp.Compatible || !verifyResp.RestoreReady || !verifyResp.DatabaseValid || !verifyResp.ConfigValid || verifyResp.ManifestFiles != 2 || verifyResp.KnownHostsIncluded {
		t.Fatalf("verify response = %+v, want valid archive with manifest files", verifyResp)
	}
	if verifyResp.Archive.Format != internalbackup.FormatName || verifyResp.Archive.Version != internalbackup.FormatVersion || verifyResp.Archive.CreatedAt == "" || verifyResp.Archive.SizeBytes != int64(len(backupBlob)) {
		t.Fatalf("verify archive facts = %+v, want current version/date/size", verifyResp.Archive)
	}
	if verifyResp.SafeCounts.Servers != 1 || len(verifyResp.Blockers) != 0 || len(verifyResp.Warnings) < 3 {
		t.Fatalf("verify readiness facts counts=%+v blockers=%+v warnings=%+v", verifyResp.SafeCounts, verifyResp.Blockers, verifyResp.Warnings)
	}
	if !verifyResp.Impact.SessionsInvalidated || !verifyResp.Impact.MetricsAccessReplaced || !verifyResp.Impact.MaintenanceRequired || !verifyResp.Impact.DowntimeExpected || verifyResp.Impact.RestartRequired {
		t.Fatalf("verify restore impact = %+v, want expected invalidation and no restart", verifyResp.Impact)
	}
	var afterVerifyCount int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM servers WHERE name = ?", "after-export").Scan(&afterVerifyCount); err != nil {
		t.Fatalf("query after-export count after verify unexpected error: %v", err)
	}
	if afterVerifyCount != 1 {
		t.Fatalf("after-export count after verify = %d, want 1", afterVerifyCount)
	}
	var jobsAfterVerify int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM jobs").Scan(&jobsAfterVerify); err != nil {
		t.Fatalf("query jobs count after verify unexpected error: %v", err)
	}
	if jobsAfterVerify != 1 {
		t.Fatalf("jobs count after verify = %d, want 1 export job only", jobsAfterVerify)
	}
	if state := currentMaintenanceState(); state.Active {
		t.Fatalf("maintenance state after verify = %+v, want inactive", state)
	}

	incompatibleBlob, err := json.Marshal(internalbackup.Envelope{
		Format:    internalbackup.FormatName,
		Version:   internalbackup.FormatVersion + 1,
		CreatedAt: "2026-05-17T06:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal incompatible backup envelope: %v", err)
	}
	var incompatibleBody bytes.Buffer
	incompatibleWriter := multipart.NewWriter(&incompatibleBody)
	incompatiblePart, err := incompatibleWriter.CreateFormFile("file", "future"+backupFileExtension)
	if err != nil {
		t.Fatalf("CreateFormFile(incompatible verify): %v", err)
	}
	if _, err := incompatiblePart.Write(incompatibleBlob); err != nil {
		t.Fatalf("write incompatible verify part: %v", err)
	}
	if err := incompatibleWriter.WriteField("passphrase", "very-strong-passphrase"); err != nil {
		t.Fatalf("write incompatible verify passphrase: %v", err)
	}
	if err := incompatibleWriter.Close(); err != nil {
		t.Fatalf("close incompatible verify writer: %v", err)
	}
	incompatibleRec := httptest.NewRecorder()
	incompatibleReq := httptest.NewRequest(http.MethodPost, "/api/backup/verify", &incompatibleBody)
	incompatibleReq.AddCookie(sessionCookie)
	markSameOriginAuthRequest(incompatibleReq)
	incompatibleReq.Header.Set("Content-Type", incompatibleWriter.FormDataContentType())
	handler.ServeHTTP(incompatibleRec, incompatibleReq)
	if incompatibleRec.Code != http.StatusOK {
		t.Fatalf("incompatible backup review status = %d, want %d (body=%s)", incompatibleRec.Code, http.StatusOK, incompatibleRec.Body.String())
	}
	var incompatibleReview struct {
		Valid        bool                            `json:"valid"`
		Compatible   bool                            `json:"compatible"`
		RestoreReady bool                            `json:"restore_ready"`
		Blockers     []internalbackup.ReadinessIssue `json:"blockers"`
	}
	if err := json.Unmarshal(incompatibleRec.Body.Bytes(), &incompatibleReview); err != nil {
		t.Fatalf("unmarshal incompatible review: %v", err)
	}
	if incompatibleReview.Valid || incompatibleReview.Compatible || incompatibleReview.RestoreReady || len(incompatibleReview.Blockers) != 1 || incompatibleReview.Blockers[0].Code != "unsupported_version" {
		t.Fatalf("incompatible review = %+v, want structured unsupported-version blocker", incompatibleReview)
	}

	var restoreBody bytes.Buffer
	writer := multipart.NewWriter(&restoreBody)
	part, err := writer.CreateFormFile("file", "test"+backupFileExtension)
	if err != nil {
		t.Fatalf("CreateFormFile() unexpected error: %v", err)
	}
	if _, err := part.Write(backupBlob); err != nil {
		t.Fatalf("part.Write() unexpected error: %v", err)
	}
	if err := writer.WriteField("passphrase", "very-strong-passphrase"); err != nil {
		t.Fatalf("WriteField(passphrase) unexpected error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() unexpected error: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/restore", &restoreBody)
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("backup restore status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	var restoreResp struct {
		JobID string `json:"job_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &restoreResp); err != nil {
		t.Fatalf("unmarshal restore response: %v", err)
	}
	if strings.TrimSpace(restoreResp.JobID) == "" {
		t.Fatalf("restore response missing job_id: %s", rec.Body.String())
	}

	var beforeCount int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM servers WHERE name = ?", "before-export").Scan(&beforeCount); err != nil {
		t.Fatalf("query before-export count unexpected error: %v", err)
	}
	if beforeCount != 1 {
		t.Fatalf("before-export count = %d, want 1", beforeCount)
	}

	var afterCount int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM servers WHERE name = ?", "after-export").Scan(&afterCount); err != nil {
		t.Fatalf("query after-export count unexpected error: %v", err)
	}
	if afterCount != 0 {
		t.Fatalf("after-export count = %d, want 0", afterCount)
	}

	restoredObservation, err := repo.Load(context.Background(), "before-export")
	if err != nil || len(restoredObservation.Findings) != 1 || restoredObservation.Findings[0].Assessment != assessment {
		t.Fatalf("distribution assessment lost during backup/restore: %+v err=%v", restoredObservation, err)
	}
	if restoredObservation.Findings[0].Resolution == nil || restoredObservation.Findings[0].Resolution.ScanID != proof.ScanID || restoredObservation.PackageProofs["example"].SourceVersion != "2" || !restoredObservation.SupersedesPackageEvidence("example", assessment.ObservedAt) {
		t.Fatalf("package proof/resolution lost during backup restore: %+v", restoredObservation)
	}

	var sessionCount int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM sessions").Scan(&sessionCount); err != nil {
		t.Fatalf("query session count unexpected error: %v", err)
	}
	if sessionCount != 0 {
		t.Fatalf("session count after restore = %d, want 0", sessionCount)
	}
	var restoreJobStatus, restoreJobKind string
	if err := getDB().QueryRow("SELECT status, kind FROM jobs WHERE id = ?", restoreResp.JobID).Scan(&restoreJobStatus, &restoreJobKind); err != nil {
		t.Fatalf("query restore job: %v", err)
	}
	if restoreJobKind != jobKindBackupRestore || restoreJobStatus != jobStatusSucceeded {
		t.Fatalf("restore job kind/status = %q/%q, want %q/%q", restoreJobKind, restoreJobStatus, jobKindBackupRestore, jobStatusSucceeded)
	}
	var restoredExportJobs int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM jobs WHERE kind = ?", jobKindBackupExport).Scan(&restoredExportJobs); err != nil {
		t.Fatalf("query restored export job count: %v", err)
	}
	if restoredExportJobs != 0 {
		t.Fatalf("restored export job count = %d, want 0", restoredExportJobs)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/backup/status", nil)
	req.AddCookie(sessionCookie)
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("backup status with pre-restore session status = %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestBackupRoutesRequireAuthentication(t *testing.T) {
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "backup-auth-gate.db"))

	gin.SetMode(gin.TestMode)
	r, err := setupRouter()
	if err != nil {
		t.Fatalf("setupRouter() unexpected error: %v", err)
	}
	handler := sessionHandler(r)

	for _, tc := range []struct {
		name   string
		method string
		path   string
		body   *bytes.Buffer
	}{
		{name: "status", method: http.MethodGet, path: "/api/backup/status", body: nil},
		{name: "export", method: http.MethodPost, path: "/api/backup/export", body: bytes.NewBufferString(`{"passphrase":"very-strong-passphrase"}`)},
		{name: "restore", method: http.MethodPost, path: "/api/backup/restore", body: bytes.NewBufferString("")},
		{name: "verify", method: http.MethodPost, path: "/api/backup/verify", body: bytes.NewBufferString("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var bodyReader io.Reader
			if tc.body != nil {
				bodyReader = bytes.NewBuffer(append([]byte(nil), tc.body.Bytes()...))
			}
			req := httptest.NewRequest(tc.method, tc.path, bodyReader)
			if tc.path == "/api/backup/export" {
				req.Header.Set("Content-Type", "application/json")
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("%s unauthenticated status = %d, want %d", tc.path, rec.Code, http.StatusUnauthorized)
			}
		})
	}
}

func TestBackupWriteRoutesRequireSameOrigin(t *testing.T) {
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "backup-same-origin.db"))

	r, err := setupRouter()
	if err != nil {
		t.Fatalf("setupRouter() unexpected error: %v", err)
	}
	handler := sessionHandler(r)

	setupBody := bytes.NewBufferString(`{"username":"admin","password":"` + testPasswordStrong + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", setupBody)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionCookie := testSessionCookieFromRecorder(t, rec)

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/export", bytes.NewBufferString(`{"passphrase":"very-strong-passphrase"}`))
	req.AddCookie(sessionCookie)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("backup export without same-origin status = %d, want %d (body=%s)", rec.Code, http.StatusForbidden, rec.Body.String())
	}
}

func TestBackupExportRejectsOversizedJSONBody(t *testing.T) {
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "backup-export-body-cap.db"))

	r, err := setupRouter()
	if err != nil {
		t.Fatalf("setupRouter() unexpected error: %v", err)
	}
	handler := sessionHandler(r)

	setupBody := bytes.NewBufferString(`{"username":"admin","password":"` + testPasswordStrong + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", setupBody)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionCookie := testSessionCookieFromRecorder(t, rec)

	body := `{"passphrase":"` + strings.Repeat("a", backupMaxExportRequestBytes) + `"}`
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/export", strings.NewReader(body))
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("backup export oversized body status = %d, want %d (body=%s)", rec.Code, http.StatusRequestEntityTooLarge, rec.Body.String())
	}
}

func TestBackupRoutesRejectWhileServerActionsAreActive(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)
	preserveSessionState(t)
	preserveRateLimiterState(t)
	preserveEncryptionState(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(t.TempDir(), "backup-busy-actions.db"))

	routeState := newServerState()
	r, err := setupRouterWithDeps(AppDeps{
		ServerState:            routeState,
		ServerInventoryService: newServerInventoryServiceWithState(routeState),
	})
	if err != nil {
		t.Fatalf("setupRouter() unexpected error: %v", err)
	}
	handler := sessionHandler(r)

	setupBody := bytes.NewBufferString(`{"username":"admin","password":"` + testPasswordStrong + `"}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/auth/setup", setupBody)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("setup status = %d, want %d (body=%s)", rec.Code, http.StatusOK, rec.Body.String())
	}
	sessionCookie := testSessionCookieFromRecorder(t, rec)

	routeState.Lock()
	routeState.SetServers([]Server{{Name: "srv-busy", Host: "example.org", Port: 22, User: "root", Pass: "pw"}})
	routeState.SetStatusMap(map[string]*ServerStatus{
		"srv-busy": {Name: "srv-busy", Status: "pending_approval", Upgradable: []string{"openssl"}},
	})
	routeState.Unlock()

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/export", bytes.NewBufferString(`{"passphrase":"very-strong-passphrase"}`))
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("backup export with active action status = %d, want %d (body=%s)", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var restoreBody bytes.Buffer
	writer := multipart.NewWriter(&restoreBody)
	part, err := writer.CreateFormFile("file", "test"+backupFileExtension)
	if err != nil {
		t.Fatalf("CreateFormFile() unexpected error: %v", err)
	}
	if _, err := part.Write([]byte("not-a-real-backup")); err != nil {
		t.Fatalf("part.Write() unexpected error: %v", err)
	}
	if err := writer.WriteField("passphrase", "very-strong-passphrase"); err != nil {
		t.Fatalf("WriteField(passphrase) unexpected error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() unexpected error: %v", err)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/backup/restore", &restoreBody)
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("backup restore with active action status = %d, want %d (body=%s)", rec.Code, http.StatusConflict, rec.Body.String())
	}

	var jobCount int
	if err := getDB().QueryRow("SELECT COUNT(1) FROM jobs").Scan(&jobCount); err != nil {
		t.Fatalf("query jobs count: %v", err)
	}
	if jobCount != 0 {
		t.Fatalf("job count after rejected backup routes = %d, want 0", jobCount)
	}
}

func TestBackupRestoreInvalidArchiveKeepsPublicFailureShape(t *testing.T) {
	app := newIsolatedTestApp(t)
	sessionCookie := app.authenticate(t)

	backupBlob, err := encryptBackupPayload([]byte("not-a-tar"), "very-strong-passphrase")
	if err != nil {
		t.Fatalf("encrypt invalid archive payload: %v", err)
	}
	var restoreBody bytes.Buffer
	writer := multipart.NewWriter(&restoreBody)
	part, err := writer.CreateFormFile("file", "invalid"+backupFileExtension)
	if err != nil {
		t.Fatalf("CreateFormFile() unexpected error: %v", err)
	}
	if _, err := part.Write(backupBlob); err != nil {
		t.Fatalf("part.Write() unexpected error: %v", err)
	}
	if err := writer.WriteField("passphrase", "very-strong-passphrase"); err != nil {
		t.Fatalf("WriteField(passphrase) unexpected error: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close() unexpected error: %v", err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/backup/restore", &restoreBody)
	req.AddCookie(sessionCookie)
	markSameOriginAuthRequest(req)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("backup restore invalid archive status = %d, want %d (body=%s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"invalid backup payload"`) {
		t.Fatalf("backup restore invalid archive body = %s, want invalid payload error", rec.Body.String())
	}
	jobID := strings.TrimSpace(rec.Header().Get("X-Job-ID"))
	if jobID == "" {
		t.Fatalf("backup restore invalid archive missing X-Job-ID header")
	}
	var jobStatus, jobSummary, jobClass string
	if err := app.Deps.DB().QueryRow("SELECT status, summary, error_class FROM jobs WHERE id = ?", jobID).Scan(&jobStatus, &jobSummary, &jobClass); err != nil {
		t.Fatalf("query restore failure job: %v", err)
	}
	if jobStatus != jobStatusFailed || jobSummary != "Invalid backup payload" || jobClass != "archive" {
		t.Fatalf("restore failure job = status %q summary %q class %q, want failed/invalid payload/archive", jobStatus, jobSummary, jobClass)
	}
}
