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

const (
	backupHealthyRetentionLimit = 100
	backupLossRetentionLimit    = 100
	backupCatalogLimit          = backupHealthyRetentionLimit + backupLossRetentionLimit
)

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
	ID                 string                 `json:"id"`
	PublishedAt        string                 `json:"published_at,omitempty"`
	CapturedAt         string                 `json:"captured_at,omitempty"`
	SizeBytes          *int64                 `json:"size_bytes"`
	KnownHostsIncluded *bool                  `json:"known_hosts_included"`
	SHA256             string                 `json:"sha256"`
	LastVerifiedAt     string                 `json:"last_verified_at,omitempty"`
	Integrity          backupArchiveIntegrity `json:"integrity,omitzero"`
	Verification       string                 `json:"verification"`
	Availability       string                 `json:"availability"`
	ReverifyPossible   bool                   `json:"reverify_possible"`
	JobID              string                 `json:"job_id,omitempty"`
}

func backupArchiveOwned(archive ownedBackupArchive, owner string) bool {
	return owner != "" && archive.Owner == owner && filepath.Base(archive.Name) == archive.Name &&
		strings.HasPrefix(archive.Name, "slu-auto-"+owner+"-") && strings.HasSuffix(archive.Name, internalbackup.FileExtension)
}

func backupArchiveAvailabilityAt(root *os.Root, archive ownedBackupArchive, owner, directory string) (availability string, reverifyPossible bool) {
	if owner == "" {
		return "unavailable", false
	}
	if !backupArchiveOwned(archive, owner) {
		return "foreign", false
	}
	if root == nil || (archive.DirectoryIdentity != "" && archive.DirectoryIdentity != directory) {
		return "unavailable", false
	}
	info, err := root.Lstat(archive.Name)
	switch {
	case os.IsNotExist(err):
		availability = "missing"
	case err != nil:
		availability = "unavailable"
	case !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > internalbackup.MaxUploadBytes:
		availability = "invalid"
	case archive.SizeBytes != nil && *archive.SizeBytes != info.Size():
		availability = "changed"
	case archive.FileIdentity != "" && archive.FileIdentity != backupCatalogFileIdentity(info):
		availability = "changed"
	case archive.FileModifiedAt != "" && !parseBackupStamp(archive.FileModifiedAt).Equal(info.ModTime()):
		// Full verification can check the digest when only the mtime changed.
		availability, reverifyPossible = "changed", true
	default:
		availability, reverifyPossible = "present", true
	}
	if archive.DirectoryIdentity == "" && archiveAvailabilityLost(availability) {
		// A missing or changed legacy file cannot establish mount identity.
		availability = "unavailable"
	}
	if reverifyPossible {
		// Metadata alone cannot show whether the service can read the archive.
		// Confirm access without reading bytes or following a replacement symlink.
		if info.Mode().Perm()&0444 == 0 {
			return "unavailable", false
		}
		file, err := openBackupCatalogFile(root, archive.Name)
		if err != nil {
			return "unavailable", false
		}
		defer file.Close()
		opened, err := file.Stat()
		if err != nil || !os.SameFile(info, opened) || opened.Size() != info.Size() || !opened.ModTime().Equal(info.ModTime()) {
			return "unavailable", false
		}
	}
	return availability, reverifyPossible
}

func (a *backupAutomation) catalogLocked(ctx context.Context, s *backupAutomationState, root *os.Root, owner string) (backupArchiveCatalog, error) {
	catalog := backupArchiveCatalog{Archives: []backupArchiveView{}, Limit: backupCatalogLimit, Truncated: len(s.Archives) > backupCatalogLimit}
	changed := false
	var events []AuditEvent
	directory := backupArchiveDirectoryIdentity(root)
	for i := len(s.Archives) - 1; i >= 0 && len(catalog.Archives) < backupCatalogLimit; i-- {
		archive := &s.Archives[i]
		availability, reverifyPossible := backupArchiveAvailabilityAt(root, *archive, owner, directory)
		// Mere presence is not a new verification. Only observed damage changes
		// existing recovery/notification evidence; repeated listing is deduplicated.
		if archiveAvailabilityLost(availability) && archive.Availability != availability {
			if event := a.observeArchive(archive, availability, backupStamp(a.now()), "backup-scheduler", ""); event != nil {
				events = append(events, *event)
			}
			changed = true
		}
		if availability == "present" && archive.Integrity.State == backupIntegrityMismatch {
			availability = "corrupt"
		}
		verification := archive.Verification
		if verification == "" {
			verification = "unknown"
		}
		catalog.Archives = append(catalog.Archives, backupArchiveView{
			ID: archive.ID, PublishedAt: archive.PublishedAt, CapturedAt: archive.CapturedAt, SizeBytes: archive.SizeBytes,
			KnownHostsIncluded: archive.KnownHostsIncluded, SHA256: archive.SHA256, LastVerifiedAt: archive.LastVerifiedAt,
			Verification: verification, Integrity: archive.Integrity, Availability: availability, ReverifyPossible: reverifyPossible, JobID: archive.JobID,
		})
	}
	if changed {
		if !backupArchiveMountAvailable(root.Name(), directory) {
			return backupArchiveCatalog{}, errBackupCatalogUnavailable
		}
		a.recordArchiveObservation(s, newBackupArchiveObservation(a.now(), "available", owner, directory))
		return catalog, a.save(ctx, *s, events...)
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
var errBackupCatalogDigestMismatch = errors.New("registered backup SHA-256 mismatch")
var errBackupCatalogVersionChanged = errors.New("registered backup changed while being read")

type backupCatalogSnapshot struct {
	file internalbackup.TemporaryFile
	info os.FileInfo
}

// snapshotBackupArchive hashes the descriptor that it reads, after no-follow
// open and inode checks. A private encrypted snapshot pins the validated bytes
// for both the existing verification pipeline and streaming, even if a local
// process modifies the original inode during or after hashing.
func snapshotBackupArchive(ctx context.Context, root *os.Root, archive ownedBackupArchive, owner, directoryIdentity string) (backupCatalogSnapshot, func(), error) {
	empty := backupCatalogSnapshot{}
	noop := func() {}
	if _, reverifyPossible := backupArchiveAvailabilityAt(root, archive, owner, directoryIdentity); !reverifyPossible {
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
	if err != nil {
		return empty, noop, errBackupCatalogUnavailable
	}
	if !sameBackupIntegrityFile(before, opened) {
		return empty, noop, errBackupCatalogVersionChanged
	}
	if (archive.FileIdentity != "" && archive.FileIdentity != backupCatalogFileIdentity(opened)) ||
		(archive.SizeBytes != nil && *archive.SizeBytes != opened.Size()) {
		return empty, noop, errBackupCatalogUnavailable
	}
	after, err := root.Lstat(archive.Name)
	if err != nil {
		return empty, noop, errBackupCatalogUnavailable
	}
	if !sameBackupIntegrityFile(opened, after) {
		return empty, noop, errBackupCatalogVersionChanged
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
	if statErr == nil && nameErr == nil && (!sameBackupIntegrityFile(opened, current) || !sameBackupIntegrityFile(current, named)) {
		cleanup()
		return empty, noop, errBackupCatalogVersionChanged
	}
	if copyErr != nil || closeErr != nil || statErr != nil || nameErr != nil || size <= 0 || size > internalbackup.MaxUploadBytes ||
		size != opened.Size() || !backupArchiveMountAvailable(root.Name(), directoryIdentity) || ctx.Err() != nil {
		cleanup()
		return empty, noop, errBackupCatalogUnavailable
	}
	if hex.EncodeToString(digest.Sum(nil)) != archive.SHA256 {
		cleanup()
		return backupCatalogSnapshot{info: current}, noop, errBackupCatalogDigestMismatch
	}
	return backupCatalogSnapshot{file: internalbackup.TemporaryFile{Path: dst.Name(), Size: size}, info: current}, cleanup, nil
}

func (a *backupAutomation) saveBackupCatalogSnapshotFailure(ctx context.Context, s backupAutomationState, index int, root *os.Root, directory, actor, ip string, snapshot backupCatalogSnapshot, snapshotErr error) error {
	digestMismatch := errors.Is(snapshotErr, errBackupCatalogDigestMismatch)
	versionChanged := errors.Is(snapshotErr, errBackupCatalogVersionChanged)
	if ctx.Err() != nil || (!digestMismatch && !versionChanged && !errors.Is(snapshotErr, errBackupCatalogUnavailable)) ||
		!backupArchiveMountAvailable(root.Name(), directory) {
		return nil
	}
	archive := &s.Archives[index]
	availability, _ := backupArchiveAvailabilityAt(root, *archive, archive.Owner, directory)
	if digestMismatch {
		named, err := root.Lstat(archive.Name)
		digestMismatch = err == nil && sameBackupIntegrityFile(snapshot.info, named)
		versionChanged = !digestMismatch
	}
	if !backupArchiveMountAvailable(root.Name(), directory) {
		return nil
	}
	a.recordArchiveObservation(&s, newBackupArchiveObservation(a.now(), "available", archive.Owner, directory))
	at := s.Observation.CheckedAt
	var event *AuditEvent
	switch {
	case digestMismatch:
		event = a.observeArchiveIntegrity(archive, backupIntegrityMismatch, at, actor, ip)
	case archiveAvailabilityLost(availability):
		event = a.observeArchive(archive, availability, at, actor, ip)
	case versionChanged && archive.Integrity.State != backupIntegrityMismatch:
		// The metadata probe may still see the registered inode and mtime.
		// Coverage stays uncertain until a digest confirms its current bytes.
		a.observeArchiveIntegrity(archive, backupIntegrityUnavailable, at, actor, ip)
	default:
		return nil
	}
	if event != nil {
		return a.save(ctx, s, *event)
	}
	return a.save(ctx, s)
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
	if a.primaryBusy.Load() {
		c.JSON(http.StatusConflict, gin.H{"error": errPrimaryBackupBusy.Error()})
		return
	}
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
	if err != nil || !backupArchiveOwned(*archive, owner) ||
		(s.ApprovedDirectory != "" && !primaryBackupMountAvailable(root, s.ApprovedDirectory)) {
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
	directory := backupArchiveDirectoryIdentity(root)
	snapshot, cleanup, err := snapshotBackupArchive(ctx, root, *archive, owner, directory)
	defer cleanup()
	if err != nil {
		if err := a.saveBackupCatalogSnapshotFailure(ctx, s, index, root, directory, actorFromContext(c), clientIPFromContext(c), snapshot, err); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
			return
		}
		c.JSON(http.StatusConflict, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	if verify {
		result, verifyErr := a.verify(ctx, snapshot.file, pass)
		if ctx.Err() != nil {
			c.JSON(http.StatusRequestTimeout, gin.H{"error": "Backup verification was cancelled."})
			return
		}
		ready := verifyErr == nil && result.RestoreReady
		mountAvailable := backupArchiveMountAvailable(root.Name(), directory)
		if ready || !mountAvailable {
			verified := *archive
			verified.SizeBytes = &snapshot.file.Size
			verified.FileIdentity = backupCatalogFileIdentity(snapshot.info)
			verified.FileModifiedAt = backupStamp(snapshot.info.ModTime())
			verified.DirectoryIdentity = directory

			// The readiness pipeline verified pinned bytes. Confirm that their
			// registered file is still present before restoring local coverage.
			storage, availability := "unavailable", "unavailable"
			if mountAvailable {
				storage = "available"
				availability, _ = backupArchiveAvailabilityAt(root, verified, owner, directory)
				if availability == "present" {
					named, err := root.Lstat(archive.Name)
					if err != nil || !sameBackupIntegrityFile(snapshot.info, named) {
						availability = "unavailable"
					}
				}
			}
			if availability != "present" {
				a.recordArchiveObservation(&s, newBackupArchiveObservation(a.now(), storage, owner, directory))
				actor, ip := actorFromContext(c), clientIPFromContext(c)
				if archiveAvailabilityLost(availability) {
					archive.DirectoryIdentity = directory
				}
				// Metadata observation can see this path as present again. Keep
				// coverage uncertain until its current bytes pass a digest check.
				if availability == "unavailable" && archive.Integrity.State != backupIntegrityMismatch {
					a.observeArchiveIntegrity(archive, backupIntegrityUnavailable, s.Observation.CheckedAt, actor, ip)
				}
				var events []AuditEvent
				if event := a.observeArchive(archive, availability, s.Observation.CheckedAt, actor, ip); event != nil {
					events = append(events, *event)
				}
				if err := a.save(ctx, s, events...); err != nil {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
					return
				}
				c.JSON(http.StatusConflict, gin.H{"error": errBackupCatalogUnavailable.Error()})
				return
			}
			verified.Verification = "verified"
			verified.LossActive = false
			captureLimit := a.now()
			if published := parseBackupStamp(archive.PublishedAt); !published.IsZero() && published.Before(captureLimit) {
				captureLimit = published
			}
			if capture := verifiedBackupCapture(result.Manifest, captureLimit); capture != "" {
				verified.CapturedAt = capture
			}
			verified.KnownHostsIncluded = &result.KnownHostsIncluded
			*archive = verified
		} else {
			archive.Verification = "failed"
		}
		archive.Availability = "present"
		a.recordArchiveObservation(&s, newBackupArchiveObservation(a.now(), "available", owner, directory))
		archive.ObservedAt = s.Observation.CheckedAt
		archive.ObservationProcess = backupArchiveObservationProcess
		if ready {
			archive.LastVerifiedAt = archive.ObservedAt
			archive.Integrity = confirmedBackupIntegrity(archive.ObservedAt)
		}
		// Recovery health and notifications consume the audit, so never report
		// a completed review or persist its marker without that shared evidence.
		if err = a.catalogAudit(actorFromContext(c), clientIPFromContext(c), "backup.verify", *archive, ready); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
			return
		}
		if err = a.save(ctx, s); err != nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
			return
		}
		if verifyErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Backup could not be decrypted or verified with the deployment secret.", "archive_id": id, "restore_ready": false})
			return
		}
		// Reuse the public readiness shape; the manifest's filenames and raw metadata
		// are intentionally omitted from this catalog response.
		c.JSON(http.StatusOK, gin.H{"archive_id": id, "restore_ready": ready, "compatible": result.Compatible, "database_valid": result.DatabaseValid, "config_valid": result.ConfigValid, "known_hosts_included": result.KnownHostsIncluded, "safe_counts": result.SafeCounts, "resources": result.Resources, "blockers": result.Blockers, "warnings": result.Warnings, "last_verified_at": archive.LastVerifiedAt})
		return
	}
	file, err := os.Open(snapshot.file.Path)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup streaming is unavailable."})
		return
	}
	defer file.Close()
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", `attachment; filename="simplelinuxupdater-automated-backup.slubkp"`)
	c.Header("Content-Length", strconv.FormatInt(snapshot.file.Size, 10))
	c.Status(http.StatusOK)
	if _, err = io.Copy(c.Writer, backupCatalogReader{ctx, file}); err == nil && ctx.Err() == nil {
		a.catalogAudit(actorFromContext(c), clientIPFromContext(c), "backup.download", *archive, true)
	}
}
