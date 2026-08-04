package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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

func TestExportArchiveFileUsesTemporaryArtifactsAndPreservesLegacyFormat(t *testing.T) {
	workDir := t.TempDir()
	databaseData := []byte(strings.Repeat("database-page-", 128))
	configData := []byte(`{"encryption_key":"test-key"}`)
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, configData, 0600); err != nil {
		t.Fatalf("write config fixture: %v", err)
	}
	includeKnownHosts := false
	service := NewService(ServiceDeps{
		ConfigPath: func() string { return configPath },
		TempDir:    func() string { return workDir },
		Logf:       func(string, ...any) {},
	})

	result, err := service.ExportArchiveFile(context.Background(), ExportRequest{
		Passphrase:        testPassphrase,
		IncludeKnownHosts: &includeKnownHosts,
		DBSnapshot:        databaseData,
	})
	if err != nil {
		t.Fatalf("ExportArchiveFile() error = %v", err)
	}
	if result.File.Path == "" || result.File.Size <= 0 {
		t.Fatalf("ExportArchiveFile() file = %+v, want a file-backed result", result.File)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("read temporary directory: %v", err)
	}
	if len(entries) != 1 || filepath.Join(workDir, entries[0].Name()) != result.File.Path {
		t.Fatalf("temporary entries = %v, want only returned encrypted artifact", entries)
	}

	encrypted, err := os.ReadFile(result.File.Path)
	if err != nil {
		t.Fatalf("read encrypted artifact: %v", err)
	}
	plain, err := DecryptPayload(encrypted, testPassphrase)
	if err != nil {
		t.Fatalf("legacy DecryptPayload(new file-backed export) error = %v", err)
	}
	files, manifest, err := ExtractTarGz(plain)
	if err != nil {
		t.Fatalf("legacy ExtractTarGz(new file-backed export) error = %v", err)
	}
	if !bytes.Equal(files["servers.db"], databaseData) || !bytes.Equal(files["config.json"], configData) {
		t.Fatalf("file-backed export did not preserve source files")
	}
	if manifest.Format != FormatName || manifest.Version != FormatVersion {
		t.Fatalf("manifest = %+v, want current legacy-compatible format", manifest)
	}
	if err := result.File.Remove(); err != nil {
		t.Fatalf("remove encrypted artifact: %v", err)
	}
	entries, err = os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("read cleaned temporary directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary entries after cleanup = %v, want none", entries)
	}
}

func TestExportArchiveFileFromSnapshotRejectsUntrustedArtifacts(t *testing.T) {
	workDir := t.TempDir()
	outsidePath := filepath.Join(t.TempDir(), "outside.sqlite")
	if err := os.WriteFile(outsidePath, []byte("snapshot"), 0600); err != nil {
		t.Fatalf("write outside snapshot: %v", err)
	}
	symlinkPath := filepath.Join(workDir, "snapshot.sqlite")
	if err := os.Symlink(outsidePath, symlinkPath); err != nil {
		t.Fatalf("create snapshot symlink: %v", err)
	}
	service := NewService(ServiceDeps{
		TempDir: func() string { return workDir },
		Logf:    func(string, ...any) {},
	})

	tests := []struct {
		name     string
		snapshot TemporaryFile
	}{
		{name: "outside private temporary directory", snapshot: TemporaryFile{Path: outsidePath, Size: 8}},
		{name: "symbolic link", snapshot: TemporaryFile{Path: symlinkPath, Size: 8}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := service.ExportArchiveFileFromSnapshot(context.Background(), ExportRequest{Passphrase: testPassphrase}, tt.snapshot)
			if err == nil {
				t.Fatal("ExportArchiveFileFromSnapshot() error = nil, want untrusted artifact rejection")
			}
			var exportErr *ExportError
			if !errors.As(err, &exportErr) || exportErr.Stage != ExportStageSnapshot {
				t.Fatalf("ExportArchiveFileFromSnapshot() error = %v, want snapshot-stage error", err)
			}
		})
	}
}

func TestFilePipelineReadsLegacyEnvelopeAndCleansArtifacts(t *testing.T) {
	workDir := t.TempDir()
	archive := buildTestTarGz(t, []testArchiveEntry{
		{name: "manifest.json", data: []byte(`{"format":"simplelinuxupdater-backup","version":1,"files":{}}`)},
	})
	encrypted, err := EncryptPayload(archive, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	legacyPath := filepath.Join(t.TempDir(), "legacy"+FileExtension)
	if err := os.WriteFile(legacyPath, encrypted, 0600); err != nil {
		t.Fatalf("write legacy envelope: %v", err)
	}
	service := NewService(ServiceDeps{TempDir: func() string { return workDir }, Logf: func(string, ...any) {}})
	plain, err := service.decryptFile(legacyPath, testPassphrase)
	if err != nil {
		t.Fatalf("decryptFile(legacy envelope) error = %v", err)
	}
	inspection, err := InspectTarGzFileWithLimits(plain.Path, workDir, MaxUploadBytes, MaxExtractedBytes)
	if err != nil {
		_ = plain.Remove()
		t.Fatalf("InspectTarGzFileWithLimits(legacy envelope) error = %v", err)
	}
	if inspection.Manifest.Format != FormatName || inspection.Manifest.Version != FormatVersion {
		t.Fatalf("inspection manifest = %+v, want legacy manifest", inspection.Manifest)
	}
	if err := inspection.Remove(); err != nil {
		t.Fatalf("remove extracted files: %v", err)
	}
	if err := plain.Remove(); err != nil {
		t.Fatalf("remove plaintext artifact: %v", err)
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("read cleaned temporary directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary entries after legacy read = %v, want none", entries)
	}
}

func TestFileCipherAllocationGrowthStaysNearPayloadGrowth(t *testing.T) {
	workDir := t.TempDir()
	service := NewService(ServiceDeps{TempDir: func() string { return workDir }, Logf: func(string, ...any) {}})
	createInput := func(name string, size int64) string {
		t.Helper()
		path := filepath.Join(workDir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if err := file.Truncate(size); err != nil {
			_ = file.Close()
			t.Fatalf("truncate %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
		return path
	}
	measureEncrypt := func(path string) uint64 {
		t.Helper()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := service.encryptFile(path, testPassphrase)
		if err != nil {
			t.Fatalf("encryptFile(%s): %v", filepath.Base(path), err)
		}
		if err := result.Remove(); err != nil {
			t.Fatalf("remove encrypted result: %v", err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}
	encryptFixture := func(path string) TemporaryFile {
		t.Helper()
		result, err := service.encryptFile(path, testPassphrase)
		if err != nil {
			t.Fatalf("encrypt fixture %s: %v", filepath.Base(path), err)
		}
		t.Cleanup(func() { _ = result.Remove() })
		return result
	}
	measureDecrypt := func(path string) uint64 {
		t.Helper()
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		result, err := service.decryptFile(path, testPassphrase)
		if err != nil {
			t.Fatalf("decryptFile(%s): %v", filepath.Base(path), err)
		}
		if err := result.Remove(); err != nil {
			t.Fatalf("remove decrypted result: %v", err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc
	}

	warmup := createInput("warmup.bin", 64*1024)
	_ = measureEncrypt(warmup)
	const smallSize = int64(1 * 1024 * 1024)
	const largeSize = int64(9 * 1024 * 1024)
	smallInput := createInput("small.bin", smallSize)
	largeInput := createInput("large.bin", largeSize)
	smallAllocated := measureEncrypt(smallInput)
	largeAllocated := measureEncrypt(largeInput)
	growth := largeAllocated - smallAllocated
	payloadGrowth := uint64(largeSize - smallSize)
	t.Logf("encrypt allocation growth = %d bytes for %d payload bytes", growth, payloadGrowth)
	if growth > payloadGrowth*3/2 {
		t.Fatalf("encrypt allocation growth = %d bytes, want at most 1.5x payload growth (%d bytes)", growth, payloadGrowth*3/2)
	}

	warmupEncrypted := encryptFixture(warmup)
	_ = measureDecrypt(warmupEncrypted.Path)
	smallEncrypted := encryptFixture(smallInput)
	largeEncrypted := encryptFixture(largeInput)
	smallAllocated = measureDecrypt(smallEncrypted.Path)
	largeAllocated = measureDecrypt(largeEncrypted.Path)
	growth = largeAllocated - smallAllocated
	t.Logf("decrypt allocation growth = %d bytes for %d payload bytes", growth, payloadGrowth)
	if growth > payloadGrowth*3/2 {
		t.Fatalf("decrypt allocation growth = %d bytes, want at most 1.5x payload growth (%d bytes)", growth, payloadGrowth*3/2)
	}
}

func TestFilePipelineReadsJSONEscapedLegacyPayload(t *testing.T) {
	archive := buildTestTarGz(t, []testArchiveEntry{
		{name: "manifest.json", data: []byte(`{"format":"simplelinuxupdater-backup","version":1,"files":{}}`)},
	})
	encrypted, err := EncryptPayload(archive, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	var envelope Envelope
	if err := json.Unmarshal(encrypted, &envelope); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if envelope.PayloadB64 == "" {
		t.Fatalf("legacy envelope payload is empty")
	}
	original := `"payload_b64":"` + envelope.PayloadB64 + `"`
	escaped := fmt.Sprintf(`"payload_b64":"\u%04x%s"`, envelope.PayloadB64[0], envelope.PayloadB64[1:])
	escapedEnvelope := []byte(strings.Replace(string(encrypted), original, escaped, 1))
	if bytes.Equal(escapedEnvelope, encrypted) {
		t.Fatalf("failed to JSON-escape the payload fixture")
	}
	encrypted = escapedEnvelope
	path := filepath.Join(t.TempDir(), "escaped"+FileExtension)
	if err := os.WriteFile(path, encrypted, 0600); err != nil {
		t.Fatalf("write escaped envelope: %v", err)
	}
	workDir := t.TempDir()
	service := NewService(ServiceDeps{TempDir: func() string { return workDir }, Logf: func(string, ...any) {}})
	plain, err := service.decryptFile(path, testPassphrase)
	if err != nil {
		t.Fatalf("decryptFile(escaped legacy payload) error = %v", err)
	}
	defer plain.Remove()
	if data, err := os.ReadFile(plain.Path); err != nil || !bytes.Equal(data, archive) {
		t.Fatalf("decrypted escaped payload mismatch: bytes=%d err=%v", len(data), err)
	}
}

func TestInspectEnvelopeFileRejectsTrailingComma(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invalid"+FileExtension)
	payload := `{"format":"simplelinuxupdater-backup","version":1,"created_at":"2026-08-03T00:00:00Z","kdf":{},"cipher":{},"payload_b64":"",}`
	if err := os.WriteFile(path, []byte(payload), 0600); err != nil {
		t.Fatalf("write malformed envelope: %v", err)
	}
	if _, err := InspectEnvelopeFile(path); !errors.Is(err, ErrMalformed) {
		t.Fatalf("InspectEnvelopeFile() error = %v, want ErrMalformed", err)
	}
}

func TestInspectEnvelopeFilePreservesUnsupportedVersionReview(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future"+FileExtension)
	if err := os.WriteFile(path, []byte(`{"format":"simplelinuxupdater-backup","version":2}`), 0600); err != nil {
		t.Fatalf("write future envelope: %v", err)
	}
	envelope, err := InspectEnvelopeFile(path)
	if err != nil {
		t.Fatalf("InspectEnvelopeFile() error = %v", err)
	}
	if envelope.Format != FormatName || envelope.Version != 2 {
		t.Fatalf("InspectEnvelopeFile() = %+v, want future version facts", envelope)
	}
}

func TestDecryptFileCleansIntermediateArtifactsOnWrongPassphrase(t *testing.T) {
	workDir := t.TempDir()
	archive := buildTestTarGz(t, []testArchiveEntry{{name: "manifest.json", data: []byte(`{"format":"simplelinuxupdater-backup","version":1,"files":{}}`)}})
	encrypted, err := EncryptPayload(archive, testPassphrase)
	if err != nil {
		t.Fatalf("EncryptPayload() error = %v", err)
	}
	path := filepath.Join(t.TempDir(), "backup"+FileExtension)
	if err := os.WriteFile(path, encrypted, 0600); err != nil {
		t.Fatalf("write encrypted fixture: %v", err)
	}
	service := NewService(ServiceDeps{TempDir: func() string { return workDir }, Logf: func(string, ...any) {}})
	if _, err := service.decryptFile(path, "wrong-passphrase"); err == nil {
		t.Fatalf("decryptFile() error = nil, want authentication failure")
	}
	entries, err := os.ReadDir(workDir)
	if err != nil {
		t.Fatalf("read temporary directory: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("temporary entries after failed decrypt = %v, want none", entries)
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
