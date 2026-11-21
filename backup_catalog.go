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
	"strings"

	internalbackup "debian-updater/internal/backup"
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

func archiveView(archive ownedBackupArchive, availability string, reverifyPossible bool) backupArchiveView {
	verification := archive.Verification
	if verification == "" {
		verification = "unknown"
	}
	return backupArchiveView{
		ID: archive.ID, PublishedAt: archive.PublishedAt, CapturedAt: archive.CapturedAt, SizeBytes: archive.SizeBytes,
		KnownHostsIncluded: archive.KnownHostsIncluded, SHA256: archive.SHA256, LastVerifiedAt: archive.LastVerifiedAt,
		Verification: verification, Integrity: archive.Integrity, Availability: availability, ReverifyPossible: reverifyPossible, JobID: archive.JobID,
	}
}

func (a *backupAutomation) observedPrimaryBackupStatus(s backupAutomationState) backupAutomationStatus {
	catalog := backupArchiveCatalog{Archives: []backupArchiveView{}, Limit: backupCatalogLimit, Truncated: len(s.Archives) > backupCatalogLimit}
	owner, _, approvalHasOwner := strings.Cut(s.Approval, "\x00")
	observed := s.Observation
	if !approvalHasOwner {
		owner = observed.Owner
	}
	now := a.now()
	checked := parseBackupStamp(observed.CheckedAt)
	fresh := owner != "" && observed.Owner == owner &&
		observed.StorageHistory.Binding == backupStorageBinding(a.deployment.Directory) &&
		observed.DirectoryIdentity != "" && (s.ApprovedDirectory == "" || observed.DirectoryIdentity == s.ApprovedDirectory) &&
		observed.Process == backupArchiveObservationProcess && observed.Storage == "available" &&
		!checked.IsZero() && !checked.After(now) && now.Sub(checked) <= 2*backupArchiveObservationInterval
	for i := len(s.Archives) - 1; i >= 0 && len(catalog.Archives) < backupCatalogLimit; i-- {
		archive := s.Archives[i]
		availability := "unavailable"
		reverifyPossible := false
		at := parseBackupStamp(archive.ObservedAt)
		if owner != "" && !backupArchiveOwned(archive, owner) {
			availability = "foreign"
		} else if fresh && archive.ID != s.PrimaryRetiring && archive.DirectoryIdentity == observed.DirectoryIdentity &&
			archive.ObservationProcess == observed.Process && !at.IsZero() && !at.After(now) &&
			now.Sub(at) <= 2*backupArchiveObservationInterval {
			availability = archive.Availability
			reverifyPossible = availability == "present" || availability == "corrupt" || (availability == "changed" && archive.ReverifyChanged)
			if availability == "present" && archive.Integrity.State == backupIntegrityMismatch {
				availability = "corrupt"
			}
		} else if backupArchiveOwned(archive, owner) && archive.ID != s.PrimaryRetiring &&
			archive.DirectoryIdentity == "" && archive.FileIdentity == "" && archive.SHA256 != "" {
			// Older registrations need an explicit digest check to establish file
			// identity. Offer that check while leaving availability unknown.
			reverifyPossible = true
		}
		catalog.Archives = append(catalog.Archives, archiveView(archive, availability, reverifyPossible))
	}
	return a.primaryBackupStatus(s, catalog, a.primaryApprovalMatchesDeployment(s))
}

func (a *backupAutomation) catalogAuditEvent(actor, ip, action string, archive ownedBackupArchive, ready bool) *AuditEvent {
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
	event := a.deps.AuditService.PrepareEvent("", actor, ip, action, "backup", "automatic", status, message, meta)
	return &event
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

func handleBackupCatalog(c *gin.Context, a *backupAutomation) {
	setNoStoreHeaders(c)
	status, err := a.Status(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup catalog is unavailable."})
		return
	}
	c.JSON(http.StatusOK, status.Catalog)
}
