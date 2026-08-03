package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"debian-updater/internal/jobs"

	_ "modernc.org/sqlite"
)

type testRestoredRuntime struct {
	prepare func()
	reload  func(context.Context) error
}

func (r testRestoredRuntime) PreparePersistenceReplacement(context.Context) error {
	if r.prepare != nil {
		r.prepare()
	}
	return nil
}

func (r testRestoredRuntime) ReloadRestoredState(ctx context.Context) error {
	if r.reload != nil {
		return r.reload(ctx)
	}
	return nil
}

const testPassphrase = "very-strong-passphrase"

type testArchiveEntry struct {
	name     string
	data     []byte
	typeflag byte
	linkname string
}

func buildTestTarGz(t *testing.T, entries []testArchiveEntry) []byte {
	t.Helper()
	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, entry := range entries {
		hdr := &tar.Header{
			Name:     entry.name,
			Mode:     0600,
			Size:     int64(len(entry.data)),
			Typeflag: entry.typeflag,
			Linkname: entry.linkname,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write %q header: %v", entry.name, err)
		}
		if len(entry.data) > 0 {
			if _, err := tw.Write(entry.data); err != nil {
				t.Fatalf("write %q data: %v", entry.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return raw.Bytes()
}

func TestValidatePassphrase(t *testing.T) {
	if err := ValidatePassphrase(testPassphrase); err != nil {
		t.Fatalf("ValidatePassphrase(valid) error = %v", err)
	}
	if err := ValidatePassphrase("short"); err != ErrInvalidPassphrase {
		t.Fatalf("ValidatePassphrase(short) error = %v, want %v", err, ErrInvalidPassphrase)
	}
}

func TestClearPersistedSessionsRemovesSessionMetadata(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`
		CREATE TABLE sessions (token TEXT PRIMARY KEY, data BLOB NOT NULL, expiry REAL NOT NULL);
		CREATE TABLE auth_session_metadata (
			token TEXT PRIMARY KEY,
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL,
			client_ip TEXT NOT NULL,
			client_ip_encrypted TEXT NOT NULL,
			client_label TEXT NOT NULL
		);
		INSERT INTO sessions(token, data, expiry) VALUES('token', x'00', 1);
		INSERT INTO auth_session_metadata VALUES('token', 'created', 'seen', '192.0.2.x', 'encrypted', 'Chrome');
	`); err != nil {
		t.Fatalf("seed session tables: %v", err)
	}
	service := NewService(ServiceDeps{DB: func() *sql.DB { return db }})
	if err := service.ClearPersistedSessions(); err != nil {
		t.Fatalf("ClearPersistedSessions() error = %v", err)
	}
	for _, table := range []string{"sessions", "auth_session_metadata"} {
		var count int
		if err := db.QueryRow("SELECT COUNT(1) FROM " + table).Scan(&count); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("%s count = %d, want 0", table, count)
		}
	}
}

func TestCreateDBSnapshotSupportsApostropheInTempPath(t *testing.T) {
	root := t.TempDir()
	quotedTempRoot := filepath.Join(root, "tmp-with-'quote")
	if err := os.MkdirAll(quotedTempRoot, 0700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", quotedTempRoot, err)
	}
	t.Setenv("TMPDIR", quotedTempRoot)

	dbPath := filepath.Join(root, "servers.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	if _, err := db.Exec("CREATE TABLE sample (id INTEGER PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		t.Fatalf("create sample table: %v", err)
	}
	if _, err := db.Exec("INSERT INTO sample(value) VALUES (?)", "ok"); err != nil {
		t.Fatalf("seed sample row: %v", err)
	}

	service := NewService(ServiceDeps{
		DB:   func() *sql.DB { return db },
		Logf: func(string, ...any) {},
	})
	snapshot, err := service.CreateDBSnapshot()
	if err != nil {
		t.Fatalf("CreateDBSnapshot() error = %v", err)
	}
	if len(snapshot) == 0 {
		t.Fatalf("CreateDBSnapshot() returned empty snapshot")
	}
	if !bytes.HasPrefix(snapshot, []byte("SQLite format 3\x00")) {
		t.Fatalf("CreateDBSnapshot() missing sqlite header")
	}
}

func TestPayloadRoundTrip(t *testing.T) {
	files := map[string][]byte{
		"servers.db":  []byte("sqlite-bytes"),
		"config.json": []byte(`{"encryption_key":"test"}`),
		"known_hosts": []byte("host ssh-ed25519 AAAATEST"),
	}
	tarGz, err := BuildTarGz(files)
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	encrypted, err := EncryptPayload(tarGz, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	plain, err := DecryptPayload(encrypted, testPassphrase)
	if err != nil {
		t.Fatalf("DecryptPayload(valid) error = %v", err)
	}
	restored, manifest, err := ExtractTarGz(plain)
	if err != nil {
		t.Fatalf("ExtractTarGz() error = %v", err)
	}
	for name, want := range files {
		if got := restored[name]; !bytes.Equal(got, want) {
			t.Fatalf("restored %s = %q, want %q", name, got, want)
		}
	}
	if manifest.Format != FormatName || manifest.Version != FormatVersion {
		t.Fatalf("manifest format/version = %q/%d, want %q/%d", manifest.Format, manifest.Version, FormatName, FormatVersion)
	}
	if _, err := DecryptPayload(encrypted, "wrong-passphrase"); err == nil {
		t.Fatalf("DecryptPayload(wrong passphrase) error = nil, want error")
	}
}

func TestDecryptPayloadRejectsMalformedAndUnsupported(t *testing.T) {
	if _, err := DecryptPayload([]byte("not-json"), testPassphrase); err != ErrMalformed {
		t.Fatalf("DecryptPayload(malformed) error = %v, want %v", err, ErrMalformed)
	}
	raw, err := json.Marshal(Envelope{Format: "other", Version: FormatVersion})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if _, err := DecryptPayload(raw, testPassphrase); err != ErrUnsupportedFormat {
		t.Fatalf("DecryptPayload(unsupported) error = %v, want %v", err, ErrUnsupportedFormat)
	}
}

func TestExtractTarGzCountsKnownEntriesAgainstTotalLimit(t *testing.T) {
	payload := buildTestTarGz(t, []testArchiveEntry{
		{name: "servers.db", data: []byte(strings.Repeat("x", 10))},
		{name: "config.json", data: []byte(strings.Repeat("y", 10))},
	})
	_, _, err := ExtractTarGzWithLimits(payload, 1024, 16)
	if err == nil || !strings.Contains(err.Error(), "backup payload is too large") {
		t.Fatalf("ExtractTarGzWithLimits() error = %v, want payload size error", err)
	}
}

func TestInspectTarGzRejectsAmbiguousArchiveEntries(t *testing.T) {
	tests := []struct {
		name       string
		entries    []testArchiveEntry
		wantDetail string
	}{
		{
			name: "duplicate entry",
			entries: []testArchiveEntry{
				{name: "manifest.json", data: []byte(`{}`)},
				{name: "manifest.json", data: []byte(`{}`)},
			},
			wantDetail: `duplicate backup entry "manifest.json"`,
		},
		{
			name:       "unknown entry",
			entries:    []testArchiveEntry{{name: "unknown.bin", data: []byte("unknown")}},
			wantDetail: `unexpected backup entry "unknown.bin"`,
		},
		{
			name:       "unknown directory",
			entries:    []testArchiveEntry{{name: "extra/", typeflag: tar.TypeDir}},
			wantDetail: `unexpected backup entry "extra/"`,
		},
		{
			name:       "nested known entry",
			entries:    []testArchiveEntry{{name: "nested/servers.db", data: []byte("sqlite")}},
			wantDetail: `non-canonical backup entry "nested/servers.db"`,
		},
		{
			name:       "dot-prefixed known entry",
			entries:    []testArchiveEntry{{name: "./config.json", data: []byte(`{}`)}},
			wantDetail: `non-canonical backup entry "./config.json"`,
		},
		{
			name:       "whitespace-padded known entry",
			entries:    []testArchiveEntry{{name: " known_hosts ", data: []byte("host key")}},
			wantDetail: `non-canonical backup entry " known_hosts "`,
		},
		{
			name:       "symlink with known name",
			entries:    []testArchiveEntry{{name: "servers.db", typeflag: tar.TypeSymlink, linkname: "config.json"}},
			wantDetail: `backup entry "servers.db" must be a regular file`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inspection, err := InspectTarGzWithLimits(buildTestTarGz(t, tt.entries), 1024, 4096)
			if !errors.Is(err, ErrMalformed) || !strings.Contains(err.Error(), tt.wantDetail) {
				t.Fatalf("InspectTarGzWithLimits() = %+v, %v; want malformed error containing %q", inspection, err, tt.wantDetail)
			}
		})
	}
}

func TestRestoreArchiveBeforeApplyRunsAfterArchiveExtraction(t *testing.T) {
	tarGz, err := BuildTarGz(map[string][]byte{
		"servers.db":  []byte("not-sqlite"),
		"config.json": []byte(`{"encryption_key":"bad"}`),
	})
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	encrypted, err := EncryptPayload(tarGz, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}

	applyStarted := false
	decodeErr := errors.New("decode failed")
	service := NewService(ServiceDeps{
		DBPath: func() string {
			return t.TempDir() + "/restore.db"
		},
		KnownHostsWritePath: func() (string, error) {
			return "", errors.New("known_hosts unavailable")
		},
		DecodeEncryptionKey: func(string) ([]byte, error) {
			return nil, decodeErr
		},
		Logf: func(string, ...any) {},
	})
	_, err = service.RestoreArchiveWithOptions(context.Background(), encrypted, testPassphrase, RestoreOptions{
		BeforeApply: func() {
			applyStarted = true
		},
	})
	if err == nil {
		t.Fatalf("RestoreArchiveWithOptions() error = nil, want apply error")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Stage != RestoreStageApply {
		t.Fatalf("RestoreArchiveWithOptions() error = %v, want apply-stage restore error", err)
	}
	if !applyStarted {
		t.Fatalf("BeforeApply was not called before applying files")
	}
}

func TestVerifyArchiveValidatesWithoutApplying(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "verify.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE servers (name TEXT PRIMARY KEY, pass_enc TEXT NOT NULL, key_enc TEXT NOT NULL);
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO servers(name, pass_enc, key_enc) VALUES('srv-a', '', '');
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed verify database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close verify database: %v", err)
	}
	dbData, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read verify database: %v", err)
	}
	tarGz, err := BuildTarGz(map[string][]byte{
		"servers.db":  dbData,
		"config.json": []byte(`{"encryption_key":"test-key"}`),
		"known_hosts": []byte("host ssh-ed25519 AAAATEST"),
	})
	if err != nil {
		t.Fatalf("BuildTarGz() error = %v", err)
	}
	encrypted, err := EncryptPayload(tarGz, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	applied := false
	service := NewService(ServiceDeps{
		DecodeEncryptionKey: func(string) ([]byte, error) { return []byte("backup-key"), nil },
		DecryptSecretWithKey: func(string, []byte) (string, error) {
			return "", nil
		},
		EnsureSchema: jobs.EnsureSchema,
		RestoredRuntime: testRestoredRuntime{reload: func(context.Context) error {
			applied = true
			return nil
		}},
		Logf: func(string, ...any) {},
	})
	result, err := service.VerifyArchive(context.Background(), encrypted, testPassphrase)
	if err != nil {
		t.Fatalf("VerifyArchive() error = %v", err)
	}
	if !result.DatabaseValid || !result.ConfigValid || !result.KnownHostsIncluded || result.ManifestFileCount != 3 {
		t.Fatalf("VerifyArchive() result = %+v, want valid archive with known_hosts", result)
	}
	if !result.RestoreReady || !result.Compatible || len(result.Blockers) != 0 {
		t.Fatalf("VerifyArchive() readiness = %+v, want compatible and ready", result)
	}
	if result.SafeCounts.Servers != 1 {
		t.Fatalf("VerifyArchive() safe server count = %d, want 1", result.SafeCounts.Servers)
	}
	if !result.Impact.SessionsInvalidated || !result.Impact.MetricsAccessReplaced || !result.Impact.MaintenanceRequired || !result.Impact.DowntimeExpected || result.Impact.RestartRequired {
		t.Fatalf("VerifyArchive() impact = %+v, want session/metrics/maintenance/downtime without restart", result.Impact)
	}
	if applied {
		t.Fatalf("VerifyArchive() invoked apply/runtime mutation hook")
	}
}

func TestVerifyArchiveReturnsStructuredIncompatibleReview(t *testing.T) {
	raw, err := json.Marshal(Envelope{
		Format:    FormatName,
		Version:   FormatVersion + 1,
		CreatedAt: "2026-05-17T06:00:00Z",
	})
	if err != nil {
		t.Fatalf("marshal incompatible envelope: %v", err)
	}
	service := NewService(ServiceDeps{Logf: func(string, ...any) {}})
	result, err := service.VerifyArchive(context.Background(), raw, testPassphrase)
	if err != nil {
		t.Fatalf("VerifyArchive(incompatible) error = %v, want structured review", err)
	}
	if result.Compatible || result.RestoreReady || result.ArchiveVersion != FormatVersion+1 {
		t.Fatalf("VerifyArchive(incompatible) = %+v, want incompatible version facts", result)
	}
	if len(result.Blockers) != 1 || result.Blockers[0].Code != "unsupported_version" {
		t.Fatalf("VerifyArchive(incompatible) blockers = %+v, want unsupported_version", result.Blockers)
	}
}

func TestVerifyArchiveReportsMissingRequiredResources(t *testing.T) {
	tarGz, err := BuildTarGz(map[string][]byte{
		"servers.db": []byte("not-needed-for-missing-resource-review"),
	})
	if err != nil {
		t.Fatalf("BuildTarGz(missing config) error = %v", err)
	}
	encrypted, err := EncryptPayload(tarGz, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload(missing config) error = %v", err)
	}
	service := NewService(ServiceDeps{Logf: func(string, ...any) {}})
	result, err := service.VerifyArchive(context.Background(), encrypted, testPassphrase)
	if err != nil {
		t.Fatalf("VerifyArchive(missing config) error = %v, want structured review", err)
	}
	if result.RestoreReady || len(result.MissingResources) != 1 || result.MissingResources[0] != "config.json" {
		t.Fatalf("VerifyArchive(missing config) = %+v, want config.json blocker", result)
	}
	if len(result.Blockers) != 1 || result.Blockers[0].Code != "missing_required_resource" {
		t.Fatalf("VerifyArchive(missing config) blockers = %+v, want missing_required_resource", result.Blockers)
	}
}

func TestValidateDatabaseDataRejectsMissingServersTableBeforeMigration(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing-servers.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec("CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		_ = db.Close()
		t.Fatalf("create settings table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read database: %v", err)
	}

	schemaCalled := false
	service := NewService(ServiceDeps{
		EnsureSchema: func(db *sql.DB) error {
			schemaCalled = true
			_, err := db.Exec(`
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
			`)
			return err
		},
		DecryptSecretWithKey: func(string, []byte) (string, error) {
			return "", nil
		},
		Logf: func(string, ...any) {},
	})
	err = service.ValidateDatabaseData(context.Background(), data, []byte("test-key"))
	if err == nil || !strings.Contains(err.Error(), "missing required table servers") {
		t.Fatalf("ValidateDatabaseData() error = %v, want missing servers table", err)
	}
	if schemaCalled {
		t.Fatalf("ValidateDatabaseData() called EnsureSchema before rejecting missing servers table")
	}
}

func TestValidateDatabaseDataRejectsUndecryptableGlobalSSHCredential(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "undecryptable-global-credential.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TABLE servers (name TEXT PRIMARY KEY, pass_enc TEXT NOT NULL, key_enc TEXT NOT NULL);
		CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
		INSERT INTO settings(key, value) VALUES('global_ssh_key', 'invalid-ciphertext');
	`); err != nil {
		_ = db.Close()
		t.Fatalf("seed restored database: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close restored database: %v", err)
	}
	data, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatalf("read restored database: %v", err)
	}

	service := NewService(ServiceDeps{
		EnsureSchema: jobs.EnsureSchema,
		DecryptSecretWithKey: func(encrypted string, _ []byte) (string, error) {
			if encrypted == "invalid-ciphertext" {
				return "", errors.New("authentication failed")
			}
			return "", nil
		},
		Logf: func(string, ...any) {},
	})
	err = service.ValidateDatabaseData(context.Background(), data, []byte("backup-key"))
	if err == nil || !strings.Contains(err.Error(), "validate restored Global SSH Credential") {
		t.Fatalf("ValidateDatabaseData() error = %v, want credential validation failure", err)
	}
}

func TestRestoreArchiveBeforeApplySkipsOnArchiveFailure(t *testing.T) {
	applyStarted := false
	service := NewService(ServiceDeps{Logf: func(string, ...any) {}})
	_, err := service.RestoreArchiveWithOptions(context.Background(), []byte("not-json"), testPassphrase, RestoreOptions{
		BeforeApply: func() {
			applyStarted = true
		},
	})
	if err == nil {
		t.Fatalf("RestoreArchiveWithOptions() error = nil, want decrypt error")
	}
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Stage != RestoreStageDecrypt {
		t.Fatalf("RestoreArchiveWithOptions() error = %v, want decrypt-stage restore error", err)
	}
	if applyStarted {
		t.Fatalf("BeforeApply was called before a successful decrypt/archive")
	}
}

func TestRestoreArchiveRejectsAmbiguousTarBeforeApply(t *testing.T) {
	payload := buildTestTarGz(t, []testArchiveEntry{{name: "unexpected.txt", data: []byte("not part of the backup format")}})
	encrypted, err := EncryptPayload(payload, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	applyStarted := false
	service := NewService(ServiceDeps{Logf: func(string, ...any) {}})
	_, err = service.RestoreArchiveWithOptions(context.Background(), encrypted, testPassphrase, RestoreOptions{
		BeforeApply: func() {
			applyStarted = true
		},
	})
	var restoreErr *RestoreError
	if !errors.As(err, &restoreErr) || restoreErr.Stage != RestoreStageArchive || !errors.Is(err, ErrMalformed) {
		t.Fatalf("RestoreArchiveWithOptions() error = %v, want malformed archive-stage restore error", err)
	}
	if applyStarted {
		t.Fatalf("BeforeApply was called for an ambiguous TAR archive")
	}
}
