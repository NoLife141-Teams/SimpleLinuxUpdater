package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	serverpkg "debian-updater/internal/servers"

	"golang.org/x/crypto/scrypt"
)

const (
	FileExtension         = ".slubkp"
	FormatName            = "simplelinuxupdater-backup"
	FormatVersion         = 1
	MaxUploadBytes        = 256 * 1024 * 1024
	MaxExtractedBytes     = MaxUploadBytes
	MaxExportRequestBytes = 1024 * 1024
	MinPassphraseLength   = 12
	rollbackTimeout       = 30 * time.Second
	ScryptN               = 32768
	ScryptR               = 8
	ScryptP               = 1
	KeyLen                = 32
)

var (
	ErrInvalidPassphrase = errors.New("passphrase must be at least 12 characters")
	ErrMalformed         = errors.New("malformed backup file")
	ErrUnsupportedFormat = errors.New("unsupported backup format")
	ErrMissingFile       = errors.New("backup missing required file")
)

type ExportRequest struct {
	Passphrase        string `json:"passphrase"`
	IncludeKnownHosts *bool  `json:"include_known_hosts"`
	DBSnapshot        []byte `json:"-"`
}

type StatusResponse struct {
	DBPath           string         `json:"db_path"`
	ConfigPath       string         `json:"config_path"`
	KnownHostsPath   string         `json:"known_hosts_path"`
	KnownHostsExists bool           `json:"known_hosts_exists"`
	RecoveryHealth   RecoveryHealth `json:"recovery_health"`
}

type Manifest struct {
	Format    string                  `json:"format"`
	Version   int                     `json:"version"`
	CreatedAt string                  `json:"created_at"`
	Files     map[string]ManifestFile `json:"files"`
}

type ManifestFile struct {
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

type KDFSpec struct {
	Name    string `json:"name"`
	N       int    `json:"N"`
	R       int    `json:"r"`
	P       int    `json:"p"`
	SaltB64 string `json:"salt_b64"`
}

type CipherSpec struct {
	Name     string `json:"name"`
	NonceB64 string `json:"nonce_b64"`
}

type Envelope struct {
	Format     string     `json:"format"`
	Version    int        `json:"version"`
	CreatedAt  string     `json:"created_at"`
	KDF        KDFSpec    `json:"kdf"`
	Cipher     CipherSpec `json:"cipher"`
	PayloadB64 string     `json:"payload_b64"`
}

type RestoreSnapshot struct {
	Path   string
	Exists bool
	Data   []byte
}

type ExportResult struct {
	Bytes              []byte
	KnownHostsIncluded bool
}

type RestoreResult struct {
	Manifest            Manifest
	GlobalKeyPresent    bool
	KnownHostsRestored  bool
	SessionsInvalidated bool
}

type VerifyResult struct {
	Manifest           Manifest         `json:"manifest"`
	ArchiveFormat      string           `json:"archive_format"`
	ArchiveVersion     int              `json:"archive_version"`
	ArchiveCreatedAt   string           `json:"archive_created_at"`
	ArchiveSizeBytes   int64            `json:"archive_size_bytes"`
	FileNames          []string         `json:"file_names"`
	ManifestFileCount  int              `json:"manifest_file_count"`
	TotalBytes         int64            `json:"total_bytes"`
	KnownHostsIncluded bool             `json:"known_hosts_included"`
	DatabaseValid      bool             `json:"database_valid"`
	ConfigValid        bool             `json:"config_valid"`
	Compatible         bool             `json:"compatible"`
	RestoreReady       bool             `json:"restore_ready"`
	Resources          []ResourceReview `json:"resources"`
	MissingResources   []string         `json:"missing_resources"`
	SafeCounts         SafeCounts       `json:"safe_counts"`
	Impact             RestoreImpact    `json:"impact"`
	Blockers           []ReadinessIssue `json:"blockers"`
	Warnings           []ReadinessIssue `json:"warnings"`
}

type ResourceReview struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Required  bool   `json:"required"`
	Included  bool   `json:"included"`
}

type SafeCounts struct {
	Servers  int64 `json:"servers"`
	Policies int64 `json:"policies"`
	Jobs     int64 `json:"jobs"`
	Sessions int64 `json:"sessions"`
}

type RestoreImpact struct {
	SessionsInvalidated   bool `json:"sessions_invalidated"`
	MetricsAccessReplaced bool `json:"metrics_access_replaced"`
	MaintenanceRequired   bool `json:"maintenance_required"`
	DowntimeExpected      bool `json:"downtime_expected"`
	RestartRequired       bool `json:"restart_required"`
}

type ReadinessIssue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type ArchiveInspection struct {
	Files            map[string][]byte
	Manifest         Manifest
	Compatible       bool
	MissingResources []string
}

type RestoreOptions struct {
	BeforeApply    func()
	RestoreHandoff func(context.Context) error
}

type ExportStage string

const (
	ExportStageSnapshot ExportStage = "snapshot"
	ExportStageConfig   ExportStage = "config"
	ExportStageArchive  ExportStage = "archive"
	ExportStageEncrypt  ExportStage = "encrypt"
)

type ExportError struct {
	Stage ExportStage
	Err   error
}

func (e *ExportError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *ExportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type RestoreStage string

const (
	RestoreStageDecrypt RestoreStage = "decrypt"
	RestoreStageArchive RestoreStage = "archive"
	RestoreStageApply   RestoreStage = "apply"
)

type RestoreError struct {
	Stage RestoreStage
	Err   error
}

func (e *RestoreError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *RestoreError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type ServiceDeps struct {
	DB                            func() *sql.DB
	DBPath                        func() string
	ConfigPath                    func() string
	KnownHostsWritePath           func() (string, error)
	EnsurePrivateDirForFile       func(string) error
	EnsureSchema                  func(*sql.DB) error
	DecodeEncryptionKey           func(string) ([]byte, error)
	CurrentEncryptionKey          func() []byte
	DecryptSecretWithKey          func(string, []byte) (string, error)
	EncryptSecretWithKey          func(string, []byte) (string, error)
	GlobalSSHCredential           *serverpkg.GlobalSSHCredential
	RestoredRuntime               RestoredRuntime
	Now                           func() time.Time
	RecoveryStaleAfter            time.Duration
	RecoveryEvidenceRetentionDays int
	Logf                          func(string, ...any)
}

// RestoredRuntime prepares persistence replacement and rehydrates app-scoped state afterward.
type RestoredRuntime interface {
	PreparePersistenceReplacement(context.Context) error
	ReloadRestoredState(context.Context) error
}

type unavailableRestoredRuntime struct{}

func (unavailableRestoredRuntime) PreparePersistenceReplacement(context.Context) error {
	return errors.New("runtime composition restored-state rehydration is unavailable")
}

func (unavailableRestoredRuntime) ReloadRestoredState(context.Context) error {
	return errors.New("runtime composition restored-state rehydration is unavailable")
}

type Service struct {
	deps ServiceDeps
}

func NewService(deps ServiceDeps) *Service {
	return &Service{deps: deps.withDefaults()}
}

func (deps ServiceDeps) withDefaults() ServiceDeps {
	if deps.RestoredRuntime == nil {
		deps.RestoredRuntime = unavailableRestoredRuntime{}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.RecoveryStaleAfter <= 0 {
		deps.RecoveryStaleAfter = DefaultRecoveryStaleAfter
	}
	if deps.RecoveryEvidenceRetentionDays <= 0 {
		deps.RecoveryEvidenceRetentionDays = DefaultRecoveryEvidenceRetentionDays
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return deps
}

func sqliteStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func (s *Service) Status() StatusResponse {
	khPath, khExists := KnownHostsBackupPath(s.deps.KnownHostsWritePath, s.deps.DBPath)
	return StatusResponse{
		DBPath:           s.deps.DBPath(),
		ConfigPath:       s.deps.ConfigPath(),
		KnownHostsPath:   khPath,
		KnownHostsExists: khExists,
	}
}

func (s *Service) CreateDBSnapshot() ([]byte, error) {
	tmp, err := os.CreateTemp("", "slu-backup-db-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return nil, err
	}
	defer os.Remove(tmpPath)
	if err := ValidateSnapshotPath(tmpPath); err != nil {
		return nil, err
	}

	vacuumSQL := "VACUUM INTO " + sqliteStringLiteral(tmpPath)
	if _, err := s.deps.DB().Exec(vacuumSQL); err != nil {
		return nil, fmt.Errorf("snapshot database: %w", err)
	}

	data, err := os.ReadFile(tmpPath)
	if err != nil {
		return nil, fmt.Errorf("read db snapshot: %w", err)
	}
	return data, nil
}

func (s *Service) ExportArchive(ctx context.Context, req ExportRequest) (ExportResult, error) {
	_ = ctx
	req.Passphrase = strings.TrimSpace(req.Passphrase)
	if err := ValidatePassphrase(req.Passphrase); err != nil {
		return ExportResult{}, err
	}
	dbSnapshot := req.DBSnapshot
	if dbSnapshot == nil {
		var err error
		dbSnapshot, err = s.CreateDBSnapshot()
		if err != nil {
			return ExportResult{}, &ExportError{Stage: ExportStageSnapshot, Err: err}
		}
	}
	configData, err := os.ReadFile(s.deps.ConfigPath())
	if err != nil {
		return ExportResult{}, &ExportError{Stage: ExportStageConfig, Err: err}
	}

	files := map[string][]byte{
		"servers.db":  dbSnapshot,
		"config.json": configData,
	}
	includeKnownHosts := true
	if req.IncludeKnownHosts != nil {
		includeKnownHosts = *req.IncludeKnownHosts
	}
	knownHostsIncluded := false
	if includeKnownHosts {
		if path, exists := KnownHostsBackupPath(s.deps.KnownHostsWritePath, s.deps.DBPath); exists {
			if data, readErr := os.ReadFile(path); readErr == nil {
				files["known_hosts"] = data
				knownHostsIncluded = true
			}
		}
	}
	tarGz, err := BuildTarGz(files)
	if err != nil {
		return ExportResult{}, &ExportError{Stage: ExportStageArchive, Err: err}
	}
	encrypted, err := EncryptPayload(tarGz, req.Passphrase)
	if err != nil {
		return ExportResult{}, &ExportError{Stage: ExportStageEncrypt, Err: err}
	}
	return ExportResult{Bytes: encrypted, KnownHostsIncluded: knownHostsIncluded}, nil
}

func (s *Service) RestoreArchive(ctx context.Context, encrypted []byte, passphrase string) (RestoreResult, error) {
	return s.RestoreArchiveWithOptions(ctx, encrypted, passphrase, RestoreOptions{})
}

func (s *Service) RestoreArchiveWithOptions(ctx context.Context, encrypted []byte, passphrase string, opts RestoreOptions) (RestoreResult, error) {
	passphrase = strings.TrimSpace(passphrase)
	if err := ValidatePassphrase(passphrase); err != nil {
		return RestoreResult{}, err
	}
	plain, err := DecryptPayload(encrypted, passphrase)
	if err != nil {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	files, manifest, err := ExtractTarGz(plain)
	if err != nil {
		return RestoreResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	if opts.BeforeApply != nil {
		opts.BeforeApply()
	}
	if err := s.applyFiles(ctx, files, opts.RestoreHandoff); err != nil {
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
	_, knownHostsRestored := files["known_hosts"]
	return RestoreResult{
		Manifest:            manifest,
		GlobalKeyPresent:    globalKeyPresent,
		KnownHostsRestored:  knownHostsRestored,
		SessionsInvalidated: true,
	}, nil
}

func (s *Service) VerifyArchive(ctx context.Context, encrypted []byte, passphrase string) (VerifyResult, error) {
	passphrase = strings.TrimSpace(passphrase)
	if err := ValidatePassphrase(passphrase); err != nil {
		return VerifyResult{}, err
	}
	envelope, err := InspectEnvelope(encrypted)
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	result := VerifyResult{
		ArchiveFormat:    envelope.Format,
		ArchiveVersion:   envelope.Version,
		ArchiveCreatedAt: envelope.CreatedAt,
		ArchiveSizeBytes: int64(len(encrypted)),
		Impact:           defaultRestoreImpact(),
		Blockers:         []ReadinessIssue{},
		Warnings:         defaultRestoreWarnings(),
		Resources:        []ResourceReview{},
		MissingResources: []string{},
	}
	if envelope.Format != FormatName {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "unsupported_format",
			Message: fmt.Sprintf("Backup format %q is not supported by this application.", envelope.Format),
		})
		return result, nil
	}
	if envelope.Version != FormatVersion {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "unsupported_version",
			Message: fmt.Sprintf("Backup version %d is not supported; this application accepts version %d.", envelope.Version, FormatVersion),
		})
		return result, nil
	}
	plain, err := DecryptPayload(encrypted, passphrase)
	if err != nil {
		if errors.Is(err, ErrUnsupportedFormat) {
			result.Blockers = append(result.Blockers, ReadinessIssue{
				Code:    "unsupported_encryption",
				Message: "The backup uses encryption settings that this application does not support.",
			})
			return result, nil
		}
		return VerifyResult{}, &RestoreError{Stage: RestoreStageDecrypt, Err: err}
	}
	inspection, err := InspectTarGzWithLimits(plain, MaxUploadBytes, MaxExtractedBytes)
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	files := inspection.Files
	manifest := inspection.Manifest
	result.Manifest = manifest
	result.ArchiveFormat = manifest.Format
	result.ArchiveVersion = manifest.Version
	result.ArchiveCreatedAt = manifest.CreatedAt
	result.Compatible = inspection.Compatible
	result.MissingResources = append([]string(nil), inspection.MissingResources...)
	result.Resources = restoreResourceReviews(manifest, files)
	result.FileNames, result.ManifestFileCount, result.TotalBytes = manifestResourceFacts(manifest)
	_, result.KnownHostsIncluded = manifest.Files["known_hosts"]
	if !inspection.Compatible {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "unsupported_manifest",
			Message: fmt.Sprintf("Backup manifest %q version %d is not supported.", manifest.Format, manifest.Version),
		})
		return result, nil
	}
	if len(inspection.MissingResources) > 0 {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "missing_required_resource",
			Message: "The backup is missing required resources: " + strings.Join(inspection.MissingResources, ", ") + ".",
		})
		return result, nil
	}
	backupKey, err := s.ValidateConfigData(files["config.json"])
	if err != nil {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "invalid_configuration",
			Message: "The archived configuration cannot be restored safely.",
		})
		return result, nil
	}
	result.ConfigValid = true
	if err := s.ValidateDatabaseData(ctx, files["servers.db"], backupKey); err != nil {
		result.Blockers = append(result.Blockers, ReadinessIssue{
			Code:    "invalid_database",
			Message: "The archived database is not compatible with this application.",
		})
		return result, nil
	}
	result.DatabaseValid = true
	counts, err := InspectDatabaseSafeCounts(ctx, files["servers.db"])
	if err != nil {
		return VerifyResult{}, &RestoreError{Stage: RestoreStageArchive, Err: err}
	}
	result.SafeCounts = counts
	if !result.KnownHostsIncluded {
		result.Warnings = append(result.Warnings, ReadinessIssue{
			Code:    "known_hosts_not_included",
			Message: "known_hosts is not included; the current host-key trust file will remain unchanged.",
		})
	}
	result.RestoreReady = true
	return result, nil
}

func defaultRestoreImpact() RestoreImpact {
	return RestoreImpact{
		SessionsInvalidated:   true,
		MetricsAccessReplaced: true,
		MaintenanceRequired:   true,
		DowntimeExpected:      true,
		RestartRequired:       false,
	}
}

func defaultRestoreWarnings() []ReadinessIssue {
	return []ReadinessIssue{
		{Code: "sessions_invalidated", Message: "All active Admin sessions will be invalidated after replacement."},
		{Code: "metrics_access_replaced", Message: "The current Metrics API credential will be replaced by the archived state."},
		{Code: "maintenance_downtime", Message: "Exclusive maintenance mode pauses requests while the archived state is applied and reloaded."},
	}
}

func restoreResourceReviews(manifest Manifest, files map[string][]byte) []ResourceReview {
	resources := make([]ResourceReview, 0, 3)
	for _, name := range []string{"servers.db", "config.json", "known_hosts"} {
		meta, declared := manifest.Files[name]
		_, included := files[name]
		resources = append(resources, ResourceReview{
			Name:      name,
			SizeBytes: meta.Size,
			Required:  name != "known_hosts",
			Included:  declared && included,
		})
	}
	return resources
}

func manifestResourceFacts(manifest Manifest) ([]string, int, int64) {
	names := make([]string, 0, len(manifest.Files))
	var totalBytes int64
	for name, meta := range manifest.Files {
		names = append(names, name)
		totalBytes += meta.Size
	}
	sort.Strings(names)
	return names, len(manifest.Files), totalBytes
}

func ValidatePassphrase(passphrase string) error {
	if len(strings.TrimSpace(passphrase)) < MinPassphraseLength {
		return ErrInvalidPassphrase
	}
	return nil
}

func ValidateSnapshotPath(path string) error {
	cleanPath := filepath.Clean(strings.TrimSpace(path))
	if cleanPath == "" || !filepath.IsAbs(cleanPath) {
		return errors.New("invalid backup snapshot path")
	}
	if strings.ContainsAny(cleanPath, "\r\n") {
		return errors.New("invalid backup snapshot path")
	}
	tempRoot := filepath.Clean(os.TempDir())
	rel, err := filepath.Rel(tempRoot, cleanPath)
	if err != nil {
		return errors.New("invalid backup snapshot path")
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return errors.New("invalid backup snapshot path")
	}
	return nil
}

func KnownHostsBackupPath(knownHostsWritePath func() (string, error), dbPath func() string) (string, bool) {
	if knownHostsWritePath != nil {
		if p, err := knownHostsWritePath(); err == nil {
			if st, statErr := os.Stat(p); statErr == nil && !st.IsDir() {
				return p, true
			}
		}
	}
	defaultPath := ""
	if dbPath != nil {
		defaultPath = filepath.Join(filepath.Dir(dbPath()), "known_hosts")
	}
	if st, err := os.Stat(defaultPath); err == nil && !st.IsDir() {
		return defaultPath, true
	}
	return defaultPath, false
}

func BuildTarGz(files map[string][]byte) ([]byte, error) {
	manifest := Manifest{
		Format:    FormatName,
		Version:   FormatVersion,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		Files:     make(map[string]ManifestFile, len(files)),
	}
	for name, data := range files {
		sum := sha256.Sum256(data)
		manifest.Files[name] = ManifestFile{
			Size:   int64(len(data)),
			SHA256: hex.EncodeToString(sum[:]),
		}
	}
	manifestData, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	files["manifest.json"] = manifestData

	var raw bytes.Buffer
	gz := gzip.NewWriter(&raw)
	tw := tar.NewWriter(gz)
	for _, name := range []string{"manifest.json", "servers.db", "config.json", "known_hosts"} {
		data, ok := files[name]
		if !ok {
			continue
		}
		hdr := &tar.Header{
			Name:    name,
			Mode:    0600,
			Size:    int64(len(data)),
			ModTime: time.Now().UTC(),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			_ = tw.Close()
			_ = gz.Close()
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return raw.Bytes(), nil
}

func EncryptPayload(plain []byte, passphrase string) ([]byte, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return nil, err
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	key, err := scrypt.Key([]byte(passphrase), salt, ScryptN, ScryptR, ScryptP, KeyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ciphertext := gcm.Seal(nil, nonce, plain, nil)
	env := Envelope{
		Format:    FormatName,
		Version:   FormatVersion,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		KDF: KDFSpec{
			Name:    "scrypt",
			N:       ScryptN,
			R:       ScryptR,
			P:       ScryptP,
			SaltB64: base64.StdEncoding.EncodeToString(salt),
		},
		Cipher: CipherSpec{
			Name:     "aes-256-gcm",
			NonceB64: base64.StdEncoding.EncodeToString(nonce),
		},
		PayloadB64: base64.StdEncoding.EncodeToString(ciphertext),
	}
	return json.Marshal(env)
}

func DecryptPayload(encrypted []byte, passphrase string) ([]byte, error) {
	env, err := InspectEnvelope(encrypted)
	if err != nil {
		return nil, err
	}
	if env.Format != FormatName || env.Version != FormatVersion {
		return nil, ErrUnsupportedFormat
	}
	if env.KDF.Name != "scrypt" || env.Cipher.Name != "aes-256-gcm" {
		return nil, ErrUnsupportedFormat
	}
	if env.KDF.N <= 0 || env.KDF.R <= 0 || env.KDF.P <= 0 {
		return nil, ErrUnsupportedFormat
	}
	if env.KDF.N != ScryptN || env.KDF.R != ScryptR || env.KDF.P != ScryptP {
		return nil, ErrUnsupportedFormat
	}
	salt, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env.KDF.SaltB64))
	if err != nil || len(salt) == 0 {
		return nil, ErrMalformed
	}
	nonce, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env.Cipher.NonceB64))
	if err != nil || len(nonce) != 12 {
		return nil, ErrMalformed
	}
	ciphertext, err := base64.StdEncoding.DecodeString(strings.TrimSpace(env.PayloadB64))
	if err != nil || len(ciphertext) == 0 {
		return nil, ErrMalformed
	}
	key, err := scrypt.Key([]byte(passphrase), salt, ScryptN, ScryptR, ScryptP, KeyLen)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		log.Printf("decryptBackupPayload: gcm open failed: %v", err)
		return nil, errors.New("invalid passphrase or corrupted backup")
	}
	return plain, nil
}

func InspectEnvelope(encrypted []byte) (Envelope, error) {
	var env Envelope
	if err := json.Unmarshal(encrypted, &env); err != nil {
		return Envelope{}, ErrMalformed
	}
	if strings.TrimSpace(env.Format) == "" || env.Version <= 0 {
		return Envelope{}, ErrMalformed
	}
	return env, nil
}

func ExtractTarGz(payload []byte) (map[string][]byte, Manifest, error) {
	return ExtractTarGzWithLimits(payload, MaxUploadBytes, MaxExtractedBytes)
}

func ExtractTarGzWithLimits(payload []byte, maxFileBytes, maxTotalBytes int64) (map[string][]byte, Manifest, error) {
	inspection, err := InspectTarGzWithLimits(payload, maxFileBytes, maxTotalBytes)
	if err != nil {
		return nil, Manifest{}, err
	}
	if !inspection.Compatible {
		return nil, Manifest{}, ErrUnsupportedFormat
	}
	if len(inspection.MissingResources) > 0 {
		return nil, Manifest{}, fmt.Errorf("%w: %s", ErrMissingFile, inspection.MissingResources[0])
	}
	return inspection.Files, inspection.Manifest, nil
}

func InspectTarGzWithLimits(payload []byte, maxFileBytes, maxTotalBytes int64) (ArchiveInspection, error) {
	files := make(map[string][]byte)
	var totalExtracted int64
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return ArchiveInspection{}, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return ArchiveInspection{}, err
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		if hdr.Size < 0 || hdr.Size > maxFileBytes {
			return ArchiveInspection{}, fmt.Errorf("%w: backup entry %q is too large", ErrMalformed, hdr.Name)
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxFileBytes+1))
		if err != nil {
			return ArchiveInspection{}, err
		}
		if int64(len(data)) > maxFileBytes {
			return ArchiveInspection{}, fmt.Errorf("%w: backup entry %q is too large", ErrMalformed, hdr.Name)
		}
		if totalExtracted+int64(len(data)) > maxTotalBytes {
			return ArchiveInspection{}, fmt.Errorf("%w: backup payload is too large", ErrMalformed)
		}
		totalExtracted += int64(len(data))
		name := filepath.Base(strings.TrimSpace(hdr.Name))
		if name == "" {
			continue
		}
		if name != "manifest.json" && name != "servers.db" && name != "config.json" && name != "known_hosts" {
			continue
		}
		files[name] = data
	}

	manifestData, ok := files["manifest.json"]
	if !ok {
		return ArchiveInspection{}, ErrMalformed
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestData, &manifest); err != nil {
		return ArchiveInspection{}, ErrMalformed
	}
	if manifest.Files == nil {
		return ArchiveInspection{}, ErrMalformed
	}
	missing := make([]string, 0, 2)
	for _, name := range []string{"servers.db", "config.json"} {
		if _, ok := files[name]; !ok {
			missing = append(missing, name)
			continue
		}
		if _, ok := manifest.Files[name]; !ok {
			missing = append(missing, name)
		}
	}
	if _, ok := files["known_hosts"]; ok {
		if _, ok := manifest.Files["known_hosts"]; !ok {
			return ArchiveInspection{}, fmt.Errorf("%w: known_hosts is present but not declared", ErrMalformed)
		}
	}
	for name, meta := range manifest.Files {
		if name != "servers.db" && name != "config.json" && name != "known_hosts" {
			return ArchiveInspection{}, fmt.Errorf("%w: unexpected file %s", ErrMalformed, name)
		}
		data, exists := files[name]
		if !exists {
			missing = append(missing, name)
			continue
		}
		if int64(len(data)) != meta.Size {
			return ArchiveInspection{}, fmt.Errorf("checksum size mismatch for %s", name)
		}
		sum := sha256.Sum256(data)
		if !strings.EqualFold(meta.SHA256, hex.EncodeToString(sum[:])) {
			return ArchiveInspection{}, fmt.Errorf("checksum mismatch for %s", name)
		}
	}
	sort.Strings(missing)
	missing = compactStrings(missing)
	return ArchiveInspection{
		Files:            files,
		Manifest:         manifest,
		Compatible:       manifest.Format == FormatName && manifest.Version == FormatVersion,
		MissingResources: missing,
	}, nil
}

func compactStrings(values []string) []string {
	if len(values) < 2 {
		return values
	}
	out := values[:1]
	for _, value := range values[1:] {
		if value != out[len(out)-1] {
			out = append(out, value)
		}
	}
	return out
}

func WriteAtomicFile(path string, data []byte, mode os.FileMode, ensurePrivateDirForFile func(string) error) error {
	if ensurePrivateDirForFile != nil {
		if err := ensurePrivateDirForFile(path); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".restore-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

func SnapshotExistingFiles(paths []string) (map[string]RestoreSnapshot, error) {
	out := make(map[string]RestoreSnapshot, len(paths))
	for _, p := range paths {
		st := RestoreSnapshot{Path: p, Exists: false}
		data, err := os.ReadFile(p)
		if err == nil {
			st.Exists = true
			st.Data = data
		} else if !os.IsNotExist(err) {
			return nil, err
		}
		out[p] = st
	}
	return out, nil
}

func RestoreSnapshots(snaps map[string]RestoreSnapshot, ensurePrivateDirForFile func(string) error) error {
	for _, snap := range snaps {
		if snap.Exists {
			if err := WriteAtomicFile(snap.Path, snap.Data, 0600, ensurePrivateDirForFile); err != nil {
				return err
			}
			continue
		}
		if err := os.Remove(snap.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func SQLiteSidecarPaths(path string) []string {
	return []string{path + "-wal", path + "-shm"}
}

func RemoveSQLiteSidecars(path string) error {
	for _, sidecar := range SQLiteSidecarPaths(path) {
		if err := os.Remove(sidecar); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func requireRestoredDatabaseTables(ctx context.Context, db *sql.DB, names ...string) error {
	for _, name := range names {
		var count int
		if err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?", name).Scan(&count); err != nil {
			return fmt.Errorf("inspect restored database table %s: %w", name, err)
		}
		if count == 0 {
			return fmt.Errorf("restored database is missing required table %s", name)
		}
	}
	return nil
}

func (s *Service) ValidateConfigData(data []byte) ([]byte, error) {
	var cfg map[string]string
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse restored config: %w", err)
	}
	key, err := s.deps.DecodeEncryptionKey(cfg["encryption_key"])
	if err != nil {
		return nil, fmt.Errorf("invalid restored encryption_key: %w", err)
	}
	return key, nil
}

func (s *Service) ValidateDatabaseData(ctx context.Context, data []byte, encryptionKey []byte) error {
	tmp, err := os.CreateTemp("", "slu-restore-validate-*.sqlite")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	db, err := sql.Open("sqlite", tmpPath)
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

func InspectDatabaseSafeCounts(ctx context.Context, data []byte) (SafeCounts, error) {
	tmp, err := os.CreateTemp("", "slu-restore-counts-*.sqlite")
	if err != nil {
		return SafeCounts{}, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return SafeCounts{}, err
	}
	if err := tmp.Close(); err != nil {
		return SafeCounts{}, err
	}
	db, err := sql.Open("sqlite", tmpPath)
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
	return SafeCounts{
		Servers:  servers,
		Policies: policies,
		Jobs:     jobs,
		Sessions: sessions,
	}, nil
}

func countExistingRows(ctx context.Context, db *sql.DB, table string) (int64, error) {
	switch table {
	case "servers", "update_policies", "jobs", "sessions":
	default:
		return 0, fmt.Errorf("unsupported safe-count table %q", table)
	}
	var exists int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM sqlite_master WHERE type = 'table' AND name = ?", table).Scan(&exists); err != nil {
		return 0, fmt.Errorf("inspect restored %s table: %w", table, err)
	}
	if exists == 0 {
		return 0, nil
	}
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(1) FROM "+table).Scan(&count); err != nil {
		return 0, fmt.Errorf("count restored %s rows: %w", table, err)
	}
	return count, nil
}

func (s *Service) ReencryptDatabaseData(ctx context.Context, data []byte, fromKey, toKey []byte) ([]byte, error) {
	if bytes.Equal(fromKey, toKey) {
		return append([]byte(nil), data...), nil
	}
	tmp, err := os.CreateTemp("", "slu-restore-rewrap-*.sqlite")
	if err != nil {
		return nil, err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, err
	}
	if err := tmp.Close(); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", tmpPath)
	if err != nil {
		return nil, fmt.Errorf("open restored database for rewrap: %w", err)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
	}()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(ctx, "PRAGMA busy_timeout=5000"); err != nil {
		return nil, fmt.Errorf("set restored database rewrap busy_timeout: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "servers"); err != nil {
		return nil, err
	}
	if err := s.deps.EnsureSchema(db); err != nil {
		return nil, fmt.Errorf("prepare restored database rewrap schema: %w", err)
	}
	if err := requireRestoredDatabaseTables(ctx, db, "jobs", "job_log_chunks"); err != nil {
		return nil, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin restored database rewrap: %w", err)
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
		return nil, fmt.Errorf("read restored server secrets for rewrap: %w", err)
	}
	var secretRows []encryptedServerSecrets
	for rows.Next() {
		var row encryptedServerSecrets
		if err := rows.Scan(&row.name, &row.passEnc, &row.keyEnc); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan restored server secret for rewrap: %w", err)
		}
		secretRows = append(secretRows, row)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close restored server secret rows for rewrap: %w", err)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read restored server secret rows for rewrap: %w", err)
	}

	updateServerStmt, err := tx.PrepareContext(ctx, "UPDATE servers SET pass_enc = ?, key_enc = ? WHERE name = ?")
	if err != nil {
		return nil, fmt.Errorf("prepare restored server secret rewrap: %w", err)
	}
	defer func() {
		if updateServerStmt != nil {
			_ = updateServerStmt.Close()
		}
	}()
	for _, row := range secretRows {
		pass, err := s.deps.DecryptSecretWithKey(row.passEnc, fromKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt restored password for %s during rewrap: %w", row.name, err)
		}
		passEnc, err := s.deps.EncryptSecretWithKey(pass, toKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt restored password for %s during rewrap: %w", row.name, err)
		}
		key, err := s.deps.DecryptSecretWithKey(row.keyEnc, fromKey)
		if err != nil {
			return nil, fmt.Errorf("decrypt restored SSH key for %s during rewrap: %w", row.name, err)
		}
		keyEnc, err := s.deps.EncryptSecretWithKey(key, toKey)
		if err != nil {
			return nil, fmt.Errorf("encrypt restored SSH key for %s during rewrap: %w", row.name, err)
		}
		if _, err := updateServerStmt.ExecContext(ctx, passEnc, keyEnc, row.name); err != nil {
			return nil, fmt.Errorf("update restored server secret for %s during rewrap: %w", row.name, err)
		}
	}
	if err := updateServerStmt.Close(); err != nil {
		return nil, fmt.Errorf("close restored server secret rewrap statement: %w", err)
	}
	updateServerStmt = nil

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
		return nil, fmt.Errorf("rewrap restored Global SSH Credential: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit restored database rewrap: %w", err)
	}
	committed = true
	if err := db.Close(); err != nil {
		return nil, fmt.Errorf("close restored database after rewrap: %w", err)
	}
	db = nil
	return os.ReadFile(tmpPath)
}

func (s *Service) PrepareRuntimeFiles(ctx context.Context, files map[string][]byte) (map[string][]byte, error) {
	backupKey, err := s.ValidateConfigData(files["config.json"])
	if err != nil {
		return nil, err
	}
	if err := s.ValidateDatabaseData(ctx, files["servers.db"], backupKey); err != nil {
		return nil, err
	}
	rewrappedDB, err := s.ReencryptDatabaseData(ctx, files["servers.db"], backupKey, s.deps.CurrentEncryptionKey())
	if err != nil {
		return nil, err
	}
	prepared := make(map[string][]byte, len(files))
	for name, data := range files {
		prepared[name] = data
	}
	prepared["servers.db"] = rewrappedDB
	return prepared, nil
}

func (s *Service) ApplyFiles(ctx context.Context, files map[string][]byte) error {
	return s.applyFiles(ctx, files, nil)
}

func (s *Service) applyFiles(ctx context.Context, files map[string][]byte, restoreHandoff func(context.Context) error) error {
	dbTarget := s.deps.DBPath()
	knownHostsTarget := filepath.Join(filepath.Dir(s.deps.DBPath()), "known_hosts")
	if p, err := s.deps.KnownHostsWritePath(); err == nil && strings.TrimSpace(p) != "" {
		knownHostsTarget = p
	}
	files, err := s.PrepareRuntimeFiles(ctx, files)
	if err != nil {
		return err
	}

	targets := []string{dbTarget}
	targets = append(targets, SQLiteSidecarPaths(dbTarget)...)
	if _, ok := files["known_hosts"]; ok {
		targets = append(targets, knownHostsTarget)
	}

	snaps, err := SnapshotExistingFiles(targets)
	if err != nil {
		return err
	}

	if err := s.deps.RestoredRuntime.PreparePersistenceReplacement(ctx); err != nil {
		return fmt.Errorf("prepare restored persistence replacement: %w", err)
	}
	rollback := func(cause error) error {
		rollbackCtx, cancelRollback := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
		defer cancelRollback()
		errs := []error{cause}
		if restoreErr := RestoreSnapshots(snaps, s.deps.EnsurePrivateDirForFile); restoreErr != nil {
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
	if err := WriteAtomicFile(dbTarget, files["servers.db"], 0600, s.deps.EnsurePrivateDirForFile); err != nil {
		return rollback(err)
	}
	if err := RemoveSQLiteSidecars(dbTarget); err != nil {
		return rollback(err)
	}
	if khData, ok := files["known_hosts"]; ok {
		if err := WriteAtomicFile(knownHostsTarget, khData, 0600, s.deps.EnsurePrivateDirForFile); err != nil {
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

func (s *Service) ClearPersistedSessions() error {
	tx, err := s.deps.DB().BeginTx(context.Background(), nil)
	if err != nil {
		return fmt.Errorf("begin session clear: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.Exec("DELETE FROM sessions"); err != nil {
		return fmt.Errorf("clear sessions: %w", err)
	}
	if _, err := tx.Exec("DELETE FROM auth_session_metadata"); err != nil {
		return fmt.Errorf("clear session metadata: %w", err)
	}
	return tx.Commit()
}

func ReadUploadedFile(file *multipart.FileHeader) ([]byte, error) {
	if file == nil {
		return nil, errors.New("missing backup file")
	}
	if file.Size > MaxUploadBytes {
		return nil, fmt.Errorf("backup file too large (max %d bytes)", MaxUploadBytes)
	}
	src, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer src.Close()
	data, err := io.ReadAll(io.LimitReader(src, MaxUploadBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxUploadBytes {
		return nil, fmt.Errorf("backup file too large (max %d bytes)", MaxUploadBytes)
	}
	if len(data) == 0 {
		return nil, errors.New("empty backup file")
	}
	return data, nil
}
