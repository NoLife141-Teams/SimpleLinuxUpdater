package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"reflect"
	"strconv"
	"time"

	internalbackup "debian-updater/internal/backup"
	"github.com/gin-gonic/gin"
)

type backupCatalogProbe struct {
	snapshot                                       backupCatalogSnapshot
	result                                         internalbackup.VerifyResult
	verifyErr                                      error
	availability, directory, owner                 string
	mountAvailable, digestMismatch, versionChanged bool
	reverifyPossible                               bool
	checkedAt                                      time.Time
}

func (a *backupAutomation) probePrimaryArchive(ctx context.Context, ref primaryAdminReference, archive ownedBackupArchive, verify bool) primaryAdminIOResult[backupCatalogProbe] {
	var probe backupCatalogProbe
	root, pass, owner, _, err := a.openPrimaryAdminDestination(false)
	if root != nil {
		defer root.Close()
	}
	if err != nil || ctx.Err() != nil || !backupArchiveOwned(archive, owner) ||
		(ref.state.ApprovedDirectory != "" && !primaryBackupMountAvailable(root, ref.state.ApprovedDirectory)) {
		return primaryAdminIOResult[backupCatalogProbe]{}
	}
	probe.owner = owner
	probe.directory = backupArchiveDirectoryIdentity(root)
	snapshot, cleanup, snapshotErr := snapshotBackupArchive(ctx, root, archive, owner, probe.directory)
	probe.snapshot = snapshot
	if snapshotErr == nil && verify && ctx.Err() == nil {
		probe.result, probe.verifyErr = a.verify(ctx, snapshot.file, pass)
	}
	if ctx.Err() != nil {
		return primaryAdminIOResult[backupCatalogProbe]{cleanup: cleanup}
	}
	probe.classifySnapshotFailure(root, archive.Name, snapshotErr)
	if snapshotErr == nil {
		archive.SizeBytes = &snapshot.file.Size
		archive.FileIdentity = backupCatalogFileIdentity(snapshot.info)
		archive.FileModifiedAt = backupStamp(snapshot.info.ModTime())
		archive.DirectoryIdentity = probe.directory
	}
	probe.availability, probe.reverifyPossible = backupArchiveAvailabilityAt(root, archive, owner, probe.directory)
	if snapshotErr == nil && probe.availability == "present" {
		named, err := root.Lstat(archive.Name)
		if err != nil || !sameBackupIntegrityFile(snapshot.info, named) {
			probe.availability = "unavailable"
		}
	}
	probe.checkedAt = a.now()
	probe.mountAvailable = backupArchiveMountAvailable(root.Name(), probe.directory)
	if !probe.mountAvailable {
		probe.availability = "unavailable"
	}
	return primaryAdminIOResult[backupCatalogProbe]{value: probe, cleanup: cleanup}
}

func (probe *backupCatalogProbe) classifySnapshotFailure(root *os.Root, name string, snapshotErr error) {
	if errors.Is(snapshotErr, errBackupCatalogDigestMismatch) || errors.Is(snapshotErr, errBackupCatalogVersionChanged) {
		named, err := root.Lstat(name)
		probe.digestMismatch = errors.Is(snapshotErr, errBackupCatalogDigestMismatch) && err == nil && sameBackupIntegrityFile(probe.snapshot.info, named)
		probe.versionChanged = !probe.digestMismatch
	}
}

func catalogArchiveIndex(s backupAutomationState, id string) int {
	for i := range s.Archives {
		if s.Archives[i].ID == id {
			return i
		}
	}
	return -1
}

func (a *backupAutomation) applyPrimaryArchiveProbe(ctx context.Context, ref primaryAdminReference, archive ownedBackupArchive, probe backupCatalogProbe, verify bool, cancelDownload *context.CancelFunc, actor, ip string) (string, error) {
	var lastVerifiedAt string
	err := a.withPrimaryAdminState(ctx, func(s backupAutomationState) error {
		index := catalogArchiveIndex(s, archive.ID)
		if !a.matchesPrimaryAdminReference(ref, s) || index < 0 || s.PrimaryRetiring == archive.ID {
			return errPrimaryBackupInvalidated
		}
		current := &s.Archives[index]
		if current.Owner != archive.Owner || current.Name != archive.Name || current.SHA256 != archive.SHA256 ||
			current.FileIdentity != archive.FileIdentity || current.FileModifiedAt != archive.FileModifiedAt ||
			current.DirectoryIdentity != archive.DirectoryIdentity || !reflect.DeepEqual(current.SizeBytes, archive.SizeBytes) ||
			parseBackupStamp(current.ObservedAt).After(probe.checkedAt) {
			return errPrimaryBackupInvalidated
		}
		if probe.directory == "" {
			return errBackupCatalogUnavailable
		}
		var events []AuditEvent
		addEvent := func(event *AuditEvent) {
			if event != nil {
				events = append(events, *event)
			}
		}
		available := probe.mountAvailable && probe.availability == "present" && probe.snapshot.file.Path != ""
		if parseBackupStamp(s.Observation.CheckedAt).After(probe.checkedAt) {
			return errPrimaryBackupInvalidated
		}
		if available && !verify {
			// The primary slot belongs to this request. Bind the HTTP transfer's
			// cancellation while restore still cannot replace the authorization.
			a.primaryCancel.Store(cancelDownload)
			return nil
		}
		observedAt := backupStamp(probe.checkedAt)
		storage := "unavailable"
		if probe.mountAvailable {
			storage = "available"
		}
		a.recordArchiveObservation(&s, newBackupArchiveObservation(probe.checkedAt, storage, probe.owner, probe.directory))
		if !available {
			switch {
			case probe.mountAvailable && probe.digestMismatch:
				addEvent(a.observeArchiveIntegrity(current, backupIntegrityMismatch, observedAt, actor, ip))
			case probe.mountAvailable && archiveAvailabilityLost(probe.availability):
				addEvent(a.observeArchive(current, probe.availability, observedAt, actor, ip))
				current.ReverifyChanged = probe.availability == "changed" && probe.reverifyPossible
			case current.Integrity.State != backupIntegrityMismatch &&
				(probe.versionChanged || probe.snapshot.file.Path != "" || !probe.mountAvailable):
				addEvent(a.observeArchiveIntegrity(current, backupIntegrityUnavailable, observedAt, actor, ip))
			}
			if err := a.save(ctx, s, events...); err != nil {
				return err
			}
			return errBackupCatalogUnavailable
		}

		ready := probe.verifyErr == nil && probe.result.RestoreReady
		if ready {
			current.SizeBytes = &probe.snapshot.file.Size
			current.FileIdentity = backupCatalogFileIdentity(probe.snapshot.info)
			current.FileModifiedAt = backupStamp(probe.snapshot.info.ModTime())
			current.DirectoryIdentity = probe.directory
			current.Verification = "verified"
			current.LossActive = false
			limit := probe.checkedAt
			if published := parseBackupStamp(current.PublishedAt); !published.IsZero() && published.Before(limit) {
				limit = published
			}
			if capture := verifiedBackupCapture(probe.result.Manifest, limit); capture != "" {
				current.CapturedAt = capture
			}
			current.KnownHostsIncluded = &probe.result.KnownHostsIncluded
			current.LastVerifiedAt = observedAt
			current.Integrity = confirmedBackupIntegrity(observedAt)
		} else {
			current.Verification = "failed"
		}
		current.Availability = "present"
		current.ReverifyChanged = false
		current.ObservedAt = observedAt
		current.ObservationProcess = backupArchiveObservationProcess
		addEvent(a.catalogAuditEvent(actor, ip, "backup.verify", *current, ready))
		lastVerifiedAt = current.LastVerifiedAt
		return a.save(ctx, s, events...)
	})
	return lastVerifiedAt, err
}

func handleBackupCatalogOperation(c *gin.Context, a *backupAutomation, verify bool) {
	setNoStoreHeaders(c)
	id := c.Param("id")
	if !validBackupArchiveID(id) {
		c.JSON(http.StatusNotFound, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	ref, err := a.primaryAdminReference(c.Request.Context())
	if err != nil {
		writePrimaryArchiveOperationError(c, err)
		return
	}
	index := catalogArchiveIndex(ref.state, id)
	if index < 0 {
		c.JSON(http.StatusNotFound, gin.H{"error": errBackupCatalogUnavailable.Error()})
		return
	}
	if ref.state.PrimaryRetiring == id {
		writePrimaryArchiveOperationError(c, errBackupCatalogUnavailable)
		return
	}
	archive := ref.state.Archives[index]
	actor, ip := actorFromContext(c), clientIPFromContext(c)
	releaseBackupRequestAdmission(c)
	requestCtx, cancelDownload := context.WithCancel(c.Request.Context())
	defer cancelDownload()
	c.Request = c.Request.WithContext(requestCtx)
	finish, ok := beginHTTPTransferBudget(c, false, false)
	if !ok {
		return
	}
	defer finish()
	ctx, cancel := context.WithTimeout(c.Request.Context(), defaultHTTPResourceBudgets.backup)
	defer cancel()
	probe, release, err := runPrimaryAdminIO(ctx, a, func() primaryAdminIOResult[backupCatalogProbe] {
		return a.probePrimaryArchive(ctx, ref, archive, verify)
	})
	if err != nil {
		writePrimaryArchiveOperationError(c, err)
		return
	}
	defer release()
	// Clear the cancellation before releasing the primary slot to another caller.
	defer a.primaryCancel.CompareAndSwap(&cancelDownload, nil)
	lastVerified, err := a.applyPrimaryArchiveProbe(ctx, ref, archive, probe, verify, &cancelDownload, actor, ip)
	if err != nil {
		writePrimaryArchiveOperationError(c, err)
		return
	}
	if verify {
		if probe.verifyErr != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Backup could not be decrypted or verified with the deployment secret.", "archive_id": id, "restore_ready": false})
			return
		}
		result := probe.result
		c.JSON(http.StatusOK, gin.H{"archive_id": id, "restore_ready": result.RestoreReady, "compatible": result.Compatible, "database_valid": result.DatabaseValid, "config_valid": result.ConfigValid, "known_hosts_included": result.KnownHostsIncluded, "safe_counts": result.SafeCounts, "resources": result.Resources, "blockers": result.Blockers, "warnings": result.Warnings, "last_verified_at": lastVerified})
		return
	}
	file, err := os.Open(probe.snapshot.file.Path)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup streaming is unavailable."})
		return
	}
	defer file.Close()
	if err := ctx.Err(); err != nil {
		writePrimaryArchiveOperationError(c, err)
		return
	}
	c.Header("Content-Type", "application/octet-stream")
	c.Header("Content-Disposition", `attachment; filename="simplelinuxupdater-automated-backup.slubkp"`)
	c.Header("Content-Length", strconv.FormatInt(probe.snapshot.file.Size, 10))
	c.Status(http.StatusOK)
	if _, err = io.Copy(c.Writer, backupCatalogReader{ctx, file}); err == nil && ctx.Err() == nil {
		_ = a.withPrimaryAdminState(ctx, func(s backupAutomationState) error {
			if a.matchesPrimaryAdminReference(ref, s) {
				if event := a.catalogAuditEvent(actor, ip, "backup.download", archive, true); event != nil {
					return a.save(ctx, s, *event)
				}
			}
			return nil
		})
	}
}

func writePrimaryArchiveOperationError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		c.JSON(http.StatusRequestTimeout, gin.H{"error": "Backup archive operation was cancelled or timed out."})
	case errors.Is(err, errBackupCatalogUnavailable), errors.Is(err, errPrimaryBackupInvalidated), errors.Is(err, errPrimaryBackupBusy):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Backup verification evidence could not be saved."})
	}
}
