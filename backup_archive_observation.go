package main

import (
	"context"
	"crypto/rand"
	"errors"
	"log"
	"os"
	"reflect"
	"strconv"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

const backupArchiveObservationInterval = 5 * time.Minute
const backupArchiveObservationBudget = 2 * time.Second

var errBackupArchiveObservationDeferred = errors.New("backup archive observation deferred while catalog changes")

// A mount can change while the process is stopped. A persisted observation
// needs a new probe before it can describe availability in this process.
var backupArchiveObservationProcess = rand.Text()

type backupArchiveObservation struct {
	CheckedAt         string               `json:"checked_at,omitempty"`
	Storage           string               `json:"storage,omitempty"`
	Owner             string               `json:"owner,omitempty"`
	DirectoryIdentity string               `json:"directory_identity,omitempty"`
	Process           string               `json:"process,omitempty"`
	StorageHistory    backupStorageHistory `json:"storage_history,omitzero"`
}

func newBackupArchiveObservation(at time.Time, storage, owner, directory string) backupArchiveObservation {
	return backupArchiveObservation{
		CheckedAt: backupStamp(at), Storage: storage, Owner: owner, DirectoryIdentity: directory,
		Process: backupArchiveObservationProcess,
	}
}

type backupArchiveScan struct {
	observation  backupArchiveObservation
	availability map[int]string
}

type backupArchiveCoverage struct {
	State          string               `json:"state"`
	Storage        string               `json:"storage"`
	StorageHealth  *backupStorageHealth `json:"storage_health,omitempty"`
	CheckedAt      string               `json:"checked_at,omitempty"`
	Verified       int                  `json:"verified"`
	Lost           int                  `json:"lost"`
	Unknown        int                  `json:"unknown"`
	LastVerifiedAt string               `json:"last_verified_at,omitempty"`
	RecoveryPoint  backupRecoveryPoint  `json:"recovery_point"`
}

type backupRecoveryPoint struct {
	State      string   `json:"state"`
	CapturedAt string   `json:"captured_at,omitempty"`
	VerifiedAt string   `json:"verified_at,omitempty"`
	AgeSeconds *float64 `json:"age_seconds,omitempty"`
}

// Only a manifest read from integrity-checked, restore-ready bytes can supply
// a capture date. Manifest creation happens separately from the SQLite snapshot.
func verifiedBackupCapture(manifest internalbackup.Manifest, latest time.Time) string {
	at := parseBackupStamp(manifest.CapturedAt)
	created := parseBackupStamp(manifest.CreatedAt)
	if at.IsZero() || at.After(latest) || (!created.IsZero() && at.Truncate(time.Second).After(created.Truncate(time.Second))) {
		return ""
	}
	return backupStamp(at)
}

func (s backupAutomationState) archiveCoverage(now time.Time) backupArchiveCoverage {
	facts := backupArchiveCoverage{State: "unknown", Storage: "not_observed", RecoveryPoint: backupRecoveryPoint{State: "unknown"}}
	observation := s.Observation
	checked := parseBackupStamp(observation.CheckedAt)
	if checked.IsZero() || observation.Process != backupArchiveObservationProcess {
		return facts
	}
	facts.CheckedAt = observation.CheckedAt
	if now.Sub(checked) > 2*backupArchiveObservationInterval {
		facts.Storage = "stale"
		return facts
	}
	facts.Storage = observation.Storage
	if observation.Owner == "" || observation.Storage != "available" {
		return facts
	}
	var lastVerified, captured time.Time
	registered := 0
	for _, archive := range s.Archives {
		if !backupArchiveOwned(archive, observation.Owner) {
			continue
		}
		registered++
		observed := parseBackupStamp(archive.ObservedAt)
		verified := parseBackupStamp(archive.LastVerifiedAt)
		switch {
		case archive.ObservationProcess != observation.Process || archive.DirectoryIdentity != observation.DirectoryIdentity ||
			observed.IsZero() || now.Sub(observed) > 2*backupArchiveObservationInterval || archive.Availability == "unavailable" ||
			(archive.Availability == "present" && archive.Integrity.State == backupIntegrityUnavailable && !archive.LossActive):
			facts.Unknown++
		case archive.Availability == "present" && archive.Verification == "verified" && !archive.LossActive && !verified.IsZero():
			facts.Verified++
			if verified.After(lastVerified) {
				facts.LastVerifiedAt, lastVerified = archive.LastVerifiedAt, verified
			}
			if at := parseBackupStamp(archive.CapturedAt); at.After(captured) && !at.After(now) && !at.After(verified) {
				captured = at
				facts.RecoveryPoint.CapturedAt = archive.CapturedAt
				facts.RecoveryPoint.VerifiedAt = archive.LastVerifiedAt
			}
		case archive.LossActive || archiveAvailabilityLost(archive.Availability):
			facts.Lost++
		}
	}
	switch {
	case facts.Verified > 0:
		facts.State = "covered"
	case facts.Unknown > 0:
		facts.State = "unknown"
	case registered == 0:
		facts.State = "no_registered"
	default:
		facts.State = "missing"
	}
	switch {
	case !captured.IsZero():
		age := now.Sub(captured)
		seconds := age.Seconds()
		facts.RecoveryPoint.AgeSeconds = &seconds
		facts.RecoveryPoint.State = "recent"
		if age > internalbackup.DefaultRecoveryStaleAfter {
			facts.RecoveryPoint.State = "stale"
		}
	case facts.State == "missing" || facts.State == "no_registered":
		facts.RecoveryPoint.State = "missing"
	}
	return facts
}

func (a *backupAutomation) monitorArchiveAvailability(ctx context.Context) {
	for {
		delay := backupArchiveObservationInterval
		err := a.observeArchiveAvailability(ctx)
		if errors.Is(err, errBackupArchiveObservationDeferred) {
			delay = time.Second
		} else if err != nil && ctx.Err() == nil {
			log.Printf("automated backup availability observation failed: %v", err)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (a *backupAutomation) observeArchiveAvailability(ctx context.Context) error {
	return a.observeArchiveAvailabilityWithScan(ctx, a.scanBackupArchives)
}

func (a *backupAutomation) observeArchiveAvailabilityWithScan(ctx context.Context, scanArchives func(context.Context, []ownedBackupArchive) backupArchiveScan) error {
	if a.deployment.Directory == "" {
		return nil
	}
	scanCtx, cancelScan := context.WithTimeout(ctx, backupArchiveObservationBudget)
	defer cancelScan()
	coord := a.deps.MaintenanceCoordinator
	lease, decision := coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	if !a.mu.TryLock() {
		lease.Close()
		return errBackupArchiveObservationDeferred
	}
	before, err := a.load(scanCtx)
	a.mu.Unlock()
	lease.Close()
	if err != nil {
		return err
	}

	// The filesystem can stall independently of SQLite. Keep the publication
	// mutex and maintenance admission free until observations are ready. A timed
	// out probe can outlive its deadline, so keep only one filesystem scan alive.
	var scan backupArchiveScan
	if a.scanMu.TryLock() {
		results := make(chan backupArchiveScan, 1)
		go func() {
			var result backupArchiveScan
			defer func() {
				a.scanMu.Unlock()
				results <- result
			}()
			result = scanArchives(scanCtx, before.Archives)
			observed := result.observation
			if scanCtx.Err() == nil && observed.DirectoryIdentity != "" &&
				!backupArchiveMountAvailable(a.deployment.Directory, observed.DirectoryIdentity) {
				// A pinned directory remains readable after it is withdrawn.
				// Discard file results when it no longer backs this destination.
				result = backupArchiveScan{observation: newBackupArchiveObservation(a.now(), "unavailable", observed.Owner, "")}
			}
		}()
		select {
		case result := <-results:
			if scanCtx.Err() == nil {
				scan = result
			}
		case <-scanCtx.Done():
		}
	}
	cancelScan()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if scan.observation.CheckedAt == "" {
		scan.observation = newBackupArchiveObservation(a.now(), "unavailable", "", "")
	}
	// Persist timeouts with a fresh SQLite budget. They describe storage rather
	// than individual archives, so a partial scan cannot open a loss episode.
	commitCtx, cancelCommit := context.WithTimeout(ctx, backupArchiveObservationBudget)
	defer cancelCommit()
	lease, decision = coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		return errBackupArchiveObservationDeferred
	}
	defer a.mu.Unlock()
	current, err := a.readState(commitCtx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, current) {
		return errBackupArchiveObservationDeferred
	}
	var events []AuditEvent
	for i, value := range scan.availability {
		archive := &current.Archives[i]
		if value == "present" && archive.DirectoryIdentity == "" {
			archive.DirectoryIdentity = scan.observation.DirectoryIdentity
		}
		if event := a.observeArchive(archive, value, scan.observation.CheckedAt, "backup-scheduler", ""); event != nil {
			events = append(events, *event)
		}
	}
	a.recordArchiveObservation(&current, scan.observation)
	return a.save(commitCtx, current, events...)
}

func (a *backupAutomation) scanBackupArchives(ctx context.Context, archives []ownedBackupArchive) backupArchiveScan {
	scan := backupArchiveScan{observation: newBackupArchiveObservation(a.now(), "unavailable", "", "")}
	observation := &scan.observation
	owner, err := a.localIdentity(false)
	if err != nil {
		return scan
	}
	observation.Owner = owner
	dir, err := backupRealPath(a.deployment.Directory)
	if err != nil {
		return scan
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return scan
	}
	defer root.Close()
	info, err := root.Lstat(".")
	if err != nil || !backupArchiveDirectoryAccessible(info) {
		return scan
	}
	observation.DirectoryIdentity = backupCatalogFileIdentity(info)
	probes := make(map[int]string)
	tracked, comparable := 0, 0
	for i := len(archives) - 1; i >= 0 && len(probes) < backupCatalogLimit && ctx.Err() == nil; i-- {
		archive := archives[i]
		if !backupArchiveOwned(archive, owner) {
			continue
		}
		tracked++
		value, _ := backupArchiveAvailabilityAt(root, archive, owner, observation.DirectoryIdentity)
		if archive.DirectoryIdentity == "" && archive.FileIdentity == "" {
			// Full verification establishes identity for archives registered before
			// the catalog recorded file metadata.
			value = "unavailable"
		}
		if value == "present" && archive.Integrity.State == backupIntegrityMismatch {
			value = "corrupt"
		}
		if value != "unavailable" {
			comparable++
		}
		probes[i] = value
	}
	if tracked == 0 || comparable > 0 {
		observation.Storage = "available"
	}
	scan.availability = probes
	return scan
}

func archiveAvailabilityLost(availability string) bool {
	return availability == "missing" || availability == "changed" || availability == "invalid" || availability == "corrupt"
}

func backupArchiveDirectoryIdentity(root *os.Root) string {
	if root != nil {
		if info, err := root.Lstat("."); err == nil {
			return backupCatalogFileIdentity(info)
		}
	}
	return ""
}

func backupArchiveDirectoryAccessible(info os.FileInfo) bool {
	return info.IsDir() && info.Mode().Perm()&0022 == 0 && info.Mode().Perm()&0111 != 0
}

func backupArchiveMountAvailable(directory, identity string) bool {
	info, err := os.Lstat(directory)
	return err == nil && backupArchiveDirectoryAccessible(info) && backupCatalogFileIdentity(info) == identity
}

func (a *backupAutomation) observeArchive(archive *ownedBackupArchive, availability, observedAt, actor, ip string) *AuditEvent {
	var event *AuditEvent
	if archiveAvailabilityLost(availability) && !archive.LossActive {
		archive.LossEpisode++
		if a.deps.AuditService != nil {
			key := "backup-archive-loss:" + archive.ID + ":" + strconv.FormatUint(archive.LossEpisode, 10)
			prepared := a.deps.AuditService.PrepareEvent(key, actor, ip, "backup.verify", "backup", "automatic", "failure",
				"Registered backup is unavailable. Review the backup catalog.", map[string]any{
					"archive_id": archive.ID, "restore_ready": false, "archive_loss_episode": archive.LossEpisode, "availability": availability,
				})
			event = &prepared
		}
		archive.LossActive = true
		archive.Verification = "failed"
	}
	archive.Availability = availability
	archive.ObservedAt = observedAt
	archive.ObservationProcess = backupArchiveObservationProcess
	return event
}
