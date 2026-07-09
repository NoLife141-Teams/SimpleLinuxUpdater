package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"sync"
	"time"

	internalbackup "debian-updater/internal/backup"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"
)

const (
	backupFileExtension         = internalbackup.FileExtension
	backupFormatName            = internalbackup.FormatName
	backupFormatVersion         = internalbackup.FormatVersion
	backupMaxUploadBytes        = internalbackup.MaxUploadBytes
	backupMaxExtractedBytes     = internalbackup.MaxExtractedBytes
	backupMaxExportRequestBytes = internalbackup.MaxExportRequestBytes
	backupMinPassphraseLength   = internalbackup.MinPassphraseLength
	backupScryptN               = internalbackup.ScryptN
	backupScryptR               = internalbackup.ScryptR
	backupScryptP               = internalbackup.ScryptP
	backupKeyLen                = internalbackup.KeyLen
)

type BackupService = internalbackup.Service
type BackupBarrier = internalbackup.Barrier
type backupExportRequest = internalbackup.ExportRequest
type backupManifest = internalbackup.Manifest
type backupManifestFile = internalbackup.ManifestFile

var (
	backupRestoreBarrier = internalbackup.NewBarrier()
	backupRestoreMu      = backupRestoreBarrier
)

func NewBackupService() *BackupService {
	return NewBackupServiceWithResetRuntimeCaches(resetRuntimeCaches)
}

func NewBackupServiceWithResetRuntimeCaches(reset func()) *BackupService {
	deps := backupServiceDepsWithDefaults(internalbackup.ServiceDeps{})
	if reset != nil {
		deps.ResetRuntimeCaches = reset
	}
	return internalbackup.NewService(deps)
}

func NewBackupServiceWithDeps(deps internalbackup.ServiceDeps) *BackupService {
	return internalbackup.NewService(backupServiceDepsWithDefaults(deps))
}

func defaultBackupService() *BackupService {
	return NewBackupService()
}

func backupServiceDepsWithDefaults(deps internalbackup.ServiceDeps) internalbackup.ServiceDeps {
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.DBPath == nil {
		deps.DBPath = dbPath
	}
	if deps.ConfigPath == nil {
		deps.ConfigPath = configPath
	}
	if deps.KnownHostsWritePath == nil {
		deps.KnownHostsWritePath = knownHostsWritePath
	}
	if deps.EnsurePrivateDirForFile == nil {
		deps.EnsurePrivateDirForFile = ensurePrivateDirForFile
	}
	if deps.EnsureSchema == nil {
		deps.EnsureSchema = ensureSchema
	}
	if deps.DecodeEncryptionKey == nil {
		deps.DecodeEncryptionKey = decodeEncryptionKeyValue
	}
	if deps.CurrentEncryptionKey == nil {
		deps.CurrentEncryptionKey = getEncryptionKey
	}
	if deps.DecryptSecretWithKey == nil {
		deps.DecryptSecretWithKey = decryptSecretWithKey
	}
	if deps.EncryptSecretWithKey == nil {
		deps.EncryptSecretWithKey = encryptSecretWithKey
	}
	if deps.ResetRuntimeCaches == nil {
		deps.ResetRuntimeCaches = resetRuntimeCaches
	}
	if deps.ReloadRuntimeState == nil {
		deps.ReloadRuntimeState = reloadRuntimeState
	}
	if deps.CurrentMaintenanceState == nil {
		deps.CurrentMaintenanceState = func() internalbackup.MaintenanceState {
			return internalbackup.MaintenanceState(currentMaintenanceState())
		}
	}
	if deps.PersistMaintenanceState == nil {
		deps.PersistMaintenanceState = func(state internalbackup.MaintenanceState) error {
			return persistMaintenanceState(MaintenanceState(state))
		}
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return deps
}

func expireSessionCookieWithManager(c *gin.Context, sm *scs.SessionManager) {
	if c == nil {
		return
	}
	if sm == nil {
		return
	}
	if sm.Cookie.SameSite == http.SameSiteDefaultMode {
		c.SetCookie(sm.Cookie.Name, "", -1, sm.Cookie.Path, sm.Cookie.Domain, sm.Cookie.Secure, sm.Cookie.HttpOnly)
		return
	}
	http.SetCookie(c.Writer, &http.Cookie{
		Name:     sm.Cookie.Name,
		Value:    "",
		Domain:   sm.Cookie.Domain,
		Path:     sm.Cookie.Path,
		MaxAge:   -1,
		HttpOnly: sm.Cookie.HttpOnly,
		Secure:   sm.Cookie.Secure,
		SameSite: sm.Cookie.SameSite,
	})
}

func validateBackupPassphrase(passphrase string) error {
	return internalbackup.ValidatePassphrase(passphrase)
}

func createDBBackupSnapshot() ([]byte, error) {
	return defaultBackupService().CreateDBSnapshot()
}

func buildBackupTarGz(files map[string][]byte) ([]byte, error) {
	return internalbackup.BuildTarGz(files)
}

func encryptBackupPayload(plain []byte, passphrase string) ([]byte, error) {
	return internalbackup.EncryptPayload(plain, passphrase)
}

func decryptBackupPayload(encrypted []byte, passphrase string) ([]byte, error) {
	return internalbackup.DecryptPayload(encrypted, passphrase)
}

func extractBackupTarGz(payload []byte) (map[string][]byte, backupManifest, error) {
	return internalbackup.ExtractTarGz(payload)
}

func extractBackupTarGzWithLimits(payload []byte, maxFileBytes, maxTotalBytes int64) (map[string][]byte, backupManifest, error) {
	return internalbackup.ExtractTarGzWithLimits(payload, maxFileBytes, maxTotalBytes)
}

func persistActiveMaintenanceStateForRestore() error {
	return defaultBackupService().PersistActiveMaintenanceStateForRestore()
}

func sqliteSidecarPaths(path string) []string {
	return internalbackup.SQLiteSidecarPaths(path)
}

func resetRuntimeCaches() {
	runtimeStateMu.Lock()
	defer runtimeStateMu.Unlock()
	if db != nil {
		_ = db.Close()
	}
	db = nil
	dbOnce = sync.Once{}

	encryptionKey = nil
	keyOnce = sync.Once{}

	globalKeyMu.Lock()
	globalKey = ""
	globalKeyMu.Unlock()

	metricsBearerTokenHashMu.Lock()
	metricsBearerTokenHash = ""
	metricsBearerTokenHashLoaded = false
	metricsBearerTokenHashDBPath = ""
	metricsBearerTokenHashMu.Unlock()
	setCurrentJobManager(nil)
}

func reloadRuntimeState() error {
	_ = getDB()
	maintenanceActive := currentMaintenanceState().Active
	if !maintenanceActive {
		if err := initializeMaintenanceState(); err != nil {
			return err
		}
	}
	if err := initializeJobManager(); err != nil {
		return err
	}
	loadServers()
	mu.Lock()
	statusMap = make(map[string]*ServerStatus, len(servers))
	for _, s := range servers {
		statusMap[s.Name] = &ServerStatus{
			Name:           s.Name,
			Host:           s.Host,
			Port:           normalizePort(s.Port),
			User:           s.User,
			Status:         "idle",
			Logs:           "",
			Upgradable:     []string{},
			PendingUpdates: []PendingUpdate{},
			HasPassword:    s.Pass != "",
			HasKey:         s.Key != "",
			Tags:           append([]string(nil), s.Tags...),
		}
	}
	mu.Unlock()
	_ = getGlobalKey()
	_ = getMetricsBearerTokenHash()
	sm, err := newSessionManager(getDB())
	if err != nil {
		return err
	}
	sessionManagerMu.Lock()
	sessionManager = sm
	sessionManagerMu.Unlock()
	return nil
}

func applyBackupFiles(ctx context.Context, files map[string][]byte) error {
	return defaultBackupService().ApplyFiles(ctx, files)
}

func handleBackupStatusWithService(c *gin.Context, service *BackupService) {
	if service == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "backup service unavailable"})
		return
	}
	c.JSON(http.StatusOK, service.Status())
}

func handleBackupExportWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	handleBackupExportWithLifecycle(c, newBackupOperationLifecycle(deps))
}

func handleBackupExportWithLifecycle(c *gin.Context, lifecycle *backupOperationLifecycle) {
	if lifecycle == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "backup lifecycle unavailable"})
		return
	}
	var req backupExportRequest
	if c.Request != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, backupMaxExportRequestBytes)
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		audit(c, "backup.export", "backup", "state", "failure", "Invalid backup export payload", nil)
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "request payload too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	req.Passphrase = strings.TrimSpace(req.Passphrase)
	if err := validateBackupPassphrase(req.Passphrase); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	exportCtx := context.Background()
	if c.Request != nil {
		exportCtx = c.Request.Context()
	}
	outcome := lifecycle.Export(exportCtx, backupExportCommand{
		Actor:    actorFromContext(c),
		ClientIP: clientIPFromContext(c),
		Request:  req,
	})
	switch outcome.Kind {
	case backupOperationSucceeded:
		c.Header("X-Job-ID", outcome.JobID)
		filename := fmt.Sprintf("simplelinuxupdater-backup-%s%s", time.Now().UTC().Format("20060102T150405Z"), backupFileExtension)
		c.Header("Content-Type", "application/octet-stream")
		c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
		c.Header("Cache-Control", "no-store")
		c.Data(http.StatusOK, "application/octet-stream", outcome.ExportBytes)
	case backupOperationActiveServerActions:
		c.JSON(http.StatusConflict, gin.H{
			"error":          outcome.PublicError,
			"active_servers": outcome.ActiveServers,
		})
	case backupOperationSnapshotFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": outcome.PublicError})
	case backupOperationJobManagerUnavailable:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "job manager unavailable"})
	case backupOperationMaintenanceBlocked:
		writeMaintenanceBlockedResponse(c)
	case backupOperationJobCreateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create backup export job"})
	case backupOperationMaintenanceActivateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate maintenance mode"})
	case backupOperationExportFailed:
		c.Header("X-Job-ID", outcome.JobID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": outcome.PublicError})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "backup export failed"})
	}
}

func readUploadedBackupFile(file *multipart.FileHeader) ([]byte, error) {
	return internalbackup.ReadUploadedFile(file)
}

func handleBackupRestoreWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	handleBackupRestoreWithLifecycle(c, newBackupOperationLifecycle(deps), deps.CurrentSessionManager)
}

func handleBackupVerifyWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	handleBackupVerifyWithService(c, deps.BackupService)
}

func handleBackupVerifyWithService(c *gin.Context, service *BackupService) {
	if service == nil {
		service = defaultBackupService()
	}
	if c.Request != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, backupMaxUploadBytes+1024)
	}
	passphrase := strings.TrimSpace(c.PostForm("passphrase"))
	if err := validateBackupPassphrase(passphrase); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		audit(c, "backup.verify", "backup", "state", "failure", "Missing backup file", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": "backup file is required"})
		return
	}
	blob, err := readUploadedBackupFile(file)
	if err != nil {
		audit(c, "backup.verify", "backup", "state", "failure", "Invalid backup file", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	verifyCtx := context.Background()
	if c.Request != nil {
		verifyCtx = c.Request.Context()
	}
	result, err := service.VerifyArchive(verifyCtx, blob, passphrase)
	if err != nil {
		var restoreErr *internalbackup.RestoreError
		stage := internalbackup.RestoreStageArchive
		if errors.As(err, &restoreErr) {
			stage = restoreErr.Stage
		}
		switch stage {
		case internalbackup.RestoreStageDecrypt:
			audit(c, "backup.verify", "backup", "state", "failure", "Failed to decrypt backup", map[string]any{"error": err.Error()})
			c.JSON(http.StatusBadRequest, gin.H{"error": "failed to decrypt backup"})
		default:
			audit(c, "backup.verify", "backup", "state", "failure", "Invalid backup payload", map[string]any{"error": err.Error()})
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid backup payload"})
		}
		return
	}
	audit(c, "backup.verify", "backup", "state", "success", "Backup verified", map[string]any{
		"manifest_files":       result.ManifestFileCount,
		"known_hosts_included": result.KnownHostsIncluded,
		"total_bytes":          result.TotalBytes,
	})
	c.JSON(http.StatusOK, gin.H{
		"message":              "backup verified",
		"valid":                true,
		"format":               result.Manifest.Format,
		"version":              result.Manifest.Version,
		"created_at":           result.Manifest.CreatedAt,
		"manifest_files":       result.ManifestFileCount,
		"file_names":           result.FileNames,
		"total_bytes":          result.TotalBytes,
		"known_hosts_included": result.KnownHostsIncluded,
		"database_valid":       result.DatabaseValid,
		"config_valid":         result.ConfigValid,
	})
}

func handleBackupRestoreWithLifecycle(c *gin.Context, lifecycle *backupOperationLifecycle, currentSession func() *scs.SessionManager) {
	if lifecycle == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "backup lifecycle unavailable"})
		return
	}
	if currentSession == nil {
		currentSession = currentSessionManager
	}
	if c.Request != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, backupMaxUploadBytes+1024)
	}
	passphrase := strings.TrimSpace(c.PostForm("passphrase"))
	if err := validateBackupPassphrase(passphrase); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	file, err := c.FormFile("file")
	if err != nil {
		audit(c, "backup.restore", "backup", "state", "failure", "Missing backup file", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": "backup file is required"})
		return
	}
	blob, err := readUploadedBackupFile(file)
	if err != nil {
		audit(c, "backup.restore", "backup", "state", "failure", "Invalid backup file", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	restoreCtx := context.Background()
	if c.Request != nil {
		restoreCtx = c.Request.Context()
	}
	outcome := lifecycle.Restore(restoreCtx, backupRestoreCommand{
		Actor:      actorFromContext(c),
		ClientIP:   clientIPFromContext(c),
		Blob:       blob,
		Passphrase: passphrase,
	})
	switch outcome.Kind {
	case backupOperationSucceeded:
		c.Header("X-Job-ID", outcome.JobID)
		if outcome.SessionsInvalidated {
			expireSessionCookieWithManager(c, currentSession())
		}
		c.JSON(http.StatusOK, gin.H{
			"message":              "backup restored",
			"job_id":               outcome.JobID,
			"restart_required":     false,
			"sessions_invalidated": outcome.SessionsInvalidated,
			"global_key_present":   outcome.GlobalKeyPresent,
			"known_hosts_restored": outcome.KnownHostsRestored,
		})
	case backupOperationActiveServerActions:
		c.JSON(http.StatusConflict, gin.H{
			"error":          outcome.PublicError,
			"active_servers": outcome.ActiveServers,
		})
	case backupOperationJobManagerUnavailable:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "job manager unavailable"})
	case backupOperationMaintenanceBlocked:
		writeMaintenanceBlockedResponse(c)
	case backupOperationJobCreateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create backup restore job"})
	case backupOperationMaintenanceActivateFailed:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to activate maintenance mode"})
	case backupOperationRestoreDecryptFailed:
		c.Header("X-Job-ID", outcome.JobID)
		c.JSON(http.StatusBadRequest, gin.H{"error": outcome.PublicError})
	case backupOperationRestoreArchiveFailed:
		c.Header("X-Job-ID", outcome.JobID)
		c.JSON(http.StatusBadRequest, gin.H{"error": outcome.PublicError})
	case backupOperationRestoreApplyFailed:
		c.Header("X-Job-ID", outcome.JobID)
		c.JSON(http.StatusInternalServerError, gin.H{"error": outcome.PublicError})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "backup restore failed"})
	}
}
