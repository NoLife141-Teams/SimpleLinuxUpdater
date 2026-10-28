package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
	"github.com/gin-gonic/gin"
)

const backupCatalogLimit = 100

// A catalog ID is an independent random lookup key, never a fingerprint of
// ownership, a filename or a deployment credential. It grants no authority:
// every command still checks local ownership and the opened encrypted bytes.
const backupArchiveIDBytes = 32

func newBackupArchiveID() (string, error) {
	token := make([]byte, backupArchiveIDBytes)
	if _, err := rand.Read(token); err != nil {
		return "", err
	}
	return hex.EncodeToString(token), nil
}

func validBackupArchiveID(id string) bool {
	if len(id) != backupArchiveIDBytes*2 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// Existing metadata remains untouched. Missing legacy IDs are saved by load
// before any caller can expose them; invalid or duplicate persisted IDs fail
// closed instead of ambiguously resolving an operation to another row.
func ensureBackupArchiveIDs(archives []ownedBackupArchive) (bool, error) {
	used := make(map[string]struct{})
	for _, archive := range archives {
		if archive.ID == "" {
			continue
		}
		if !validBackupArchiveID(archive.ID) {
			return false, errors.New("invalid backup catalog identifier")
		}
		if _, duplicate := used[archive.ID]; duplicate {
			return false, errors.New("duplicate backup catalog identifier")
		}
		used[archive.ID] = struct{}{}
	}
	changed := false
	for i := range archives {
		if archives[i].ID != "" {
			continue
		}
		id, err := newBackupArchiveID()
		if err != nil {
			return false, err
		}
		if _, duplicate := used[id]; duplicate {
			return false, errors.New("backup catalog identifier allocation failed")
		}
		archives[i].ID = id
		used[id] = struct{}{}
		changed = true
	}
	return changed, nil
}

type backupArchiveCatalog struct {
	Archives  []backupArchiveView `json:"archives"`
	Limit     int                 `json:"limit"`
	Truncated bool                `json:"truncated"`
}

type backupArchiveView struct {
	ID                 string `json:"id"`
	PublishedAt        string `json:"published_at,omitempty"`
	SizeBytes          *int64 `json:"size_bytes"`
	KnownHostsIncluded *bool  `json:"known_hosts_included"`
	SHA256             string `json:"sha256"`
	LastVerifiedAt     string `json:"last_verified_at,omitempty"`
	Verification       string `json:"verification"`
	Availability       string `json:"availability"`
	JobID              string `json:"job_id,omitempty"`
}

func backupArchiveOwned(archive ownedBackupArchive, owner string) bool {
	return owner != "" && archive.Owner == owner && filepath.Base(archive.Name) == archive.Name &&
		strings.HasPrefix(archive.Name, "slu-auto-"+owner+"-") && strings.HasSuffix(archive.Name, internalbackup.FileExtension)
}

func backupArchiveAvailability(root *os.Root, archive ownedBackupArchive, owner string) string {
	if owner == "" {
		return "unavailable"
	}
	if !backupArchiveOwned(archive, owner) {
		return "foreign"
	}
	if root == nil {
		return "unavailable"
	}
	info, err := root.Lstat(archive.Name)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "unavailable"
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > internalbackup.MaxUploadBytes {
		return "invalid"
	}
	if archive.SizeBytes != nil && *archive.SizeBytes != info.Size() {
		return "changed"
	}
	if archive.FileIdentity != "" && archive.FileIdentity != backupCatalogFileIdentity(info) {
		return "changed"
	}
	return "present"
}

func (a *backupAutomation) catalogLocked(ctx context.Context, s *backupAutomationState, root *os.Root, owner string) (backupArchiveCatalog, error) {
	catalog := backupArchiveCatalog{Archives: []backupArchiveView{}, Limit: backupCatalogLimit, Truncated: len(s.Archives) > backupCatalogLimit}
	changed := false
	for i := len(s.Archives) - 1; i >= 0 && len(catalog.Archives) < backupCatalogLimit; i-- {
		archive := &s.Archives[i]
		availability := backupArchiveAvailability(root, *archive, owner)
		// Mere presence is not a new verification. Only observed damage changes
		// existing recovery/notification evidence; repeated listing is deduplicated.
		if availability != "present" && availability != "foreign" && availability != "unavailable" && archive.Availability != availability {
			archive.Availability = availability
			archive.Verification = "failed"
			// Do not persist the deduplication marker unless recovery consumers can
			// observe the damage. A later listing must retry a failed audit write.
			if err := a.catalogAudit("backup-scheduler", "", "backup.verify", *archive, false); err != nil {
				return catalog, err
			}
			changed = true
		}
		verification := archive.Verification
		if verification == "" {
			verification = "unknown"
		}
		catalog.Archives = append(catalog.Archives, backupArchiveView{
			ID: archive.ID, PublishedAt: archive.PublishedAt, SizeBytes: archive.SizeBytes,
			KnownHostsIncluded: archive.KnownHostsIncluded, SHA256: archive.SHA256, LastVerifiedAt: archive.LastVerifiedAt,
			Verification: verification, Availability: availability, JobID: archive.JobID,
		})
	}
	if changed {
		return catalog, a.save(ctx, *s)
	}
	return catalog, nil
}

func (a *backupAutomation) catalogAudit(actor, ip, action string, archive ownedBackupArchive, ready bool) error {
	if a.deps.AuditService == nil {
		return nil
	}
	status, message := "failure", "Registered backup is unavailable or did not pass restore readiness verification"
	if ready {
		status, message = "success", "Registered backup passed restore readiness verification"
	}
	if action == "backup.download" && ready {
		message = "Registered encrypted backup downloaded after hash validation"
	}
	// Never include filenames, paths, passphrases, raw pipeline errors or manifest
	// contents. Existing backup.verify consumers own recovery and notifications.
	meta := map[string]any{"archive_id": archive.ID}
	if action == "backup.download" {
		// Encrypted integrity is not a new decryption/readiness review.
		meta["hash_valid"] = ready
	} else {
		meta["restore_ready"] = ready
	}
	if archive.SizeBytes != nil {
		meta["archive_size_bytes"] = *archive.SizeBytes
	}
	return a.deps.AuditService.Record(actor, ip, action, "backup", "automatic", status, message, meta)
}

type backupCatalogReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r backupCatalogReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

var errBackupCatalogUnavailable = errors.New("registered backup is unavailable or failed validation")

// snapshotBackupArchive hashes the descriptor that it reads, after no-follow
// open and inode checks. A private encrypted snapshot pins the validated bytes
// for both the existing verification pipeline and streaming, even if a local
// process modifies the original inode during or after hashing.
func snapshotBackupArchive(ctx context.Context, root *os.Root, archive ownedBackupArchive, owner string) (internalbackup.TemporaryFile, func(), error) {
	empty := internalbackup.TemporaryFile{}
	noop := func() {}
	if backupArchiveAvailability(root, archive, owner) != "present" {
		return empty, noop, errBackupCatalogUnavailable
	}
	before, err := root.Lstat(archive.Name)
	if err != nil {
		return empty, noop, errBackupCatalogUnavailable
	}
	src, err := openBackupCatalogFile(root, archive.Name)
	if err != nil {
		return empty, noop, errBackupCatalogUnavailable
	}
	defer src.Close()
	opened, err := src.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) ||
		(archive.FileIdentity != "" && archive.FileIdentity != backupCatalogFileIdentity(opened)) ||
		(archive.SizeBytes != nil && *archive.SizeBytes != opened.Size()) {
		return empty, noop, errBackupCatalogUnavailable
	}
	after, err := root.Lstat(archive.Name)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(opened, after) {
		return empty, noop, errBackupCatalogUnavailable
	}
	directory, err := os.MkdirTemp("", "slu-backup-catalog-*")
	if err != nil {
		return empty, noop, errors.New("backup staging is unavailable")
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	dst, err := os.OpenFile(filepath.Join(directory, "archive.slubkp"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		cleanup()
		return empty, noop, errors.New("backup staging is unavailable")
	}
	digest := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(dst, digest), io.LimitReader(backupCatalogReader{ctx, src}, internalbackup.MaxUploadBytes+1))
	closeErr := dst.Close()
	current, statErr := src.Stat()
	named, nameErr := root.Lstat(archive.Name)
	if copyErr != nil || closeErr != nil || statErr != nil || nameErr != nil || size <= 0 || size > internalbackup.MaxUploadBytes ||
		size != opened.Size() || current.Size() != opened.Size() || !current.ModTime().Equal(opened.ModTime()) ||
		!named.Mode().IsRegular() || !os.SameFile(current, named) || hex.EncodeToString(digest.Sum(nil)) != archive.SHA256 || ctx.Err() != nil {
		cleanup()
		return empty, noop, errBackupCatalogUnavailable
	}
	return internalbackup.TemporaryFile{Path: dst.Name(), Size: size}, cleanup, nil
}

func handleBackupCatalog(c *gin.Context, a *backupAutomation) {
	setNoStoreHeaders(c)
	status, err := a.Status(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup catalog is unavailable."})
		return
	}
	c.JSON(http.StatusOK, status.Catalog)
}

func handleBackupCatalogOperation(c *gin.Context, a *backupAutomation, verify bool) {
	setNoStoreHeaders(c)
	id := c.Param("id")
	if !validBackupArchiveID(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	// The router already owns a shared maintenance lease. Keep a separate lease
	// here so direct invocation has identical restore/publication exclusion.
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		writeMaintenanceBlockedSnapshotResponseWithTime(c, decision.State, a.deps.ApplicationTime)
		return
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		c.JSON(http.StatusConflict, gin.H{"error": "A backup operation is already in progress."})
		return
	}
	defer a.mu.Unlock()
	s, err := a.load(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup catalog is unavailable."})
		return
	}
	index := -1
	for i := len(s.Archives) - 1; i >= 0; i-- {
		if s.Archives[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	root, pass, owner, _, err := a.destination(false)
	if root != nil {
		defer root.Close()
	}
	archive := &s.Archives[index]
	if err != nil || !backupArchiveOwned(*archive, owner) {
		c.JSON(http.StatusConflict, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	finish, ok := beginHTTPTransferBudget(c, false, false)
	if !ok {
		return
	}
	defer finish()
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHTTPResourceBudgets.backup)
	defer cancel()
	snapshot, cleanup, err := snapshotBackupArchive(ctx, root, *archive, owner)
	defer cleanup()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, errBackupCatalogUnavailable) {
			archive.Availability = backupArchiveAvailability(root, *archive, owner)
			archive.Verification = "failed"
			if err := a.catalogAudit(actorFromContext(c), clientIPFromContext(c), "backup.verify", *archive, false); err != nil {
				c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
				return
			}
			_ = a.save(ctx, s)
		}
		c.JSON(http.StatusConflict, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	if verify {
		result, verifyErr := a.verify(ctx, snapshot, pass)
		if ctx.Err() != nil {
			c.JSON(http.StatusRequestTimeout, gin.H{"error": "Backup verification was cancelled."})
			return
		}
		ready := verifyErr == nil && result.RestoreReady
		archive.Verification = "failed"
		archive.Availability = "present"
		if ready {
			archive.Verification = "verified"
			archive.LastVerifiedAt = backupStamp(a.now())
			archive.SizeBytes = &snapshot.Size
			archive.KnownHostsIncluded = &result.KnownHostsIncluded
		}
		if err = a.save(ctx, s); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
			return
		}
		a.catalogAudit(actorFromContext(c), clientIPFromContext(c), "backup.verify", *archive, ready)
		if verifyErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Backup could not be decrypted or verified with the deployment secret.", "archive_id": id, "restore_ready": false})
			return
		}
		// Reuse the public readiness shape; the manifest's filenames and raw metadata
		// are intentionally omitted from this catalog response.
		c.JSON(http.StatusOK, gin.H{"archive_id": id, "restore_ready": ready, "compatible": result.Compatible, "database_valid": result.DatabaseValid, "config_valid": result.ConfigValid, "known_hosts_included": result.KnownHostsIncluded, "safe_counts": result.SafeCounts, "resources": result.Resources, "blockers": result.Blockers, "warnings": result.Warnings, "last_verified_at": archive.LastVerifiedAt})
		return
	}
	file, err := os.Open(snapshot.Path)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup streaming is unavailable."})
		return
	}
	defer file.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", `attachment; filename="simplelinuxupdater-automated-backup.slubkp"`)
	c.Header("Content-Length", strconv.FormatInt(snapshot.Size, 10))
	c.Status(http.StatusOK)
	if _, err = io.Copy(c.Writer, backupCatalogReader{ctx, file}); err == nil && ctx.Err() == nil {
		a.catalogAudit(actorFromContext(c), clientIPFromContext(c), "backup.download", *archive, true)
	}
}
