package main

import (
	"context"
	"errors"
	"log"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	notificationpkg "debian-updater/internal/notifications"
)

const backupArchiveObservationInterval = 5 * time.Minute
const backupArchiveObservationBudget = 2 * time.Second
const backupArchiveLossConditionPrefix = "backup/archive-loss/"

var errBackupArchiveObservationDeferred = errors.New("backup archive observation deferred while catalog changes")

type backupArchiveObservation struct {
	CheckedAt string `json:"checked_at,omitempty"`
	Storage   string `json:"storage,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

type backupArchiveScan struct {
	observation  backupArchiveObservation
	availability map[int]string
	directory    string
}

type backupArchiveCoverage struct {
	State          string `json:"state"`
	Storage        string `json:"storage"`
	CheckedAt      string `json:"checked_at,omitempty"`
	Verified       int    `json:"verified"`
	Lost           int    `json:"lost"`
	Unknown        int    `json:"unknown"`
	LastVerifiedAt string `json:"last_verified_at,omitempty"`
}

func (s backupAutomationState) archiveCoverage(now time.Time) backupArchiveCoverage {
	facts := backupArchiveCoverage{State: "unknown", Storage: "not_observed"}
	observation := s.Observation
	checked := parseBackupStamp(observation.CheckedAt)
	if checked.IsZero() {
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
	var lastVerified time.Time
	registered := 0
	for _, archive := range s.Archives {
		if !backupArchiveOwned(archive, observation.Owner) {
			continue
		}
		registered++
		observed := parseBackupStamp(archive.ObservedAt)
		verified := parseBackupStamp(archive.LastVerifiedAt)
		switch {
		case observed.IsZero() || now.Sub(observed) > 2*backupArchiveObservationInterval || archive.Availability == "unavailable":
			facts.Unknown++
		case archive.Availability == "present" && archive.Verification == "verified" && !archive.LossActive && !verified.IsZero():
			facts.Verified++
			if verified.After(lastVerified) {
				facts.LastVerifiedAt, lastVerified = archive.LastVerifiedAt, verified
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
	ctx, cancel := context.WithTimeout(ctx, backupArchiveObservationBudget)
	defer cancel()
	if a.deployment.Directory == "" {
		return nil
	}
	coord := a.deps.MaintenanceCoordinator
	lease, decision := coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	if !a.mu.TryLock() {
		lease.Close()
		return errBackupArchiveObservationDeferred
	}
	before, err := a.load(ctx)
	a.mu.Unlock()
	lease.Close()
	if err != nil {
		return err
	}

	// The filesystem can stall independently of SQLite. Keep the publication
	// mutex and maintenance admission free until observations are ready.
	if !a.scanMu.TryLock() {
		return errBackupArchiveObservationDeferred
	}
	results := make(chan backupArchiveScan, 1)
	go func() {
		defer a.scanMu.Unlock()
		observation, availability, directory := a.scanBackupArchives(ctx, before.Archives)
		results <- backupArchiveScan{observation, availability, directory}
	}()
	var scan backupArchiveScan
	select {
	case scan = <-results:
		if err := ctx.Err(); err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	lease, decision = coord.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return errBackupArchiveObservationDeferred
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		return errBackupArchiveObservationDeferred
	}
	defer a.mu.Unlock()
	current, err := a.readState(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, current) {
		return errBackupArchiveObservationDeferred
	}
	for i, value := range scan.availability {
		archive := &current.Archives[i]
		if value == "present" && archive.DirectoryIdentity == "" {
			archive.DirectoryIdentity = scan.directory
		}
		if err := a.observeArchive(archive, value, scan.observation.CheckedAt, "backup-scheduler", ""); err != nil {
			return err
		}
	}
	current.Observation = scan.observation
	return a.save(ctx, current)
}

func (a *backupAutomation) scanBackupArchives(ctx context.Context, archives []ownedBackupArchive) (backupArchiveObservation, map[int]string, string) {
	observation := backupArchiveObservation{CheckedAt: backupStamp(a.now()), Storage: "unavailable"}
	owner, err := a.localIdentity(false)
	if err != nil {
		return observation, nil, ""
	}
	observation.Owner = owner
	dir, err := backupRealPath(a.deployment.Directory)
	if err != nil {
		return observation, nil, ""
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return observation, nil, ""
	}
	defer root.Close()
	info, err := root.Lstat(".")
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return observation, nil, ""
	}
	directory := backupCatalogFileIdentity(info)
	probes := make(map[int]string)
	tracked, comparable := 0, 0
	for i := len(archives) - 1; i >= 0 && len(probes) < backupCatalogLimit && ctx.Err() == nil; i-- {
		archive := archives[i]
		if !backupArchiveOwned(archive, owner) {
			continue
		}
		tracked++
		value := backupArchiveAvailability(root, archive, owner)
		switch {
		case archive.DirectoryIdentity != "" && archive.DirectoryIdentity == directory:
			comparable++
		case archive.DirectoryIdentity == "" && value == "present" && archive.FileIdentity != "":
			// A legacy archive can identify its directory through a matching
			// registered file. Missing legacy files cannot identify a mount.
			comparable++
		default:
			value = "unavailable"
		}
		probes[i] = value
	}
	if tracked == 0 || comparable > 0 {
		observation.Storage = "available"
	}
	return observation, probes, directory
}

func archiveAvailabilityLost(availability string) bool {
	return availability == "missing" || availability == "changed" || availability == "invalid"
}

func backupArchiveDirectoryIdentity(root *os.Root) string {
	if root != nil {
		if info, err := root.Lstat("."); err == nil {
			return backupCatalogFileIdentity(info)
		}
	}
	return ""
}

func backupArchiveAvailabilityAt(root *os.Root, archive ownedBackupArchive, owner, directory string) string {
	if backupArchiveOwned(archive, owner) && archive.DirectoryIdentity != "" && archive.DirectoryIdentity != directory {
		return "unavailable"
	}
	return backupArchiveAvailability(root, archive, owner)
}

func (a *backupAutomation) observeArchive(archive *ownedBackupArchive, availability, observedAt, actor, ip string) error {
	if availability == "foreign" {
		return errors.New("cannot observe a foreign backup archive")
	}
	if archiveAvailabilityLost(availability) && !archive.LossActive {
		if archive.LossEpisode == 0 && archiveAvailabilityLost(archive.Availability) && archive.Verification == "failed" {
			// Older releases already emitted the generic loss fact.
			archive.Availability, archive.ObservedAt, archive.LossActive = availability, observedAt, true
			return nil
		}
		archive.LossEpisode++
		if a.deps.AuditService != nil {
			key := "backup-archive-loss:" + archive.ID + ":" + strconv.FormatUint(archive.LossEpisode, 10)
			if err := a.deps.AuditService.RecordOnce(key, actor, ip, "backup.verify", "backup", "automatic", "failure",
				"Registered backup is unavailable. Review the backup catalog.", map[string]any{
					"archive_id": archive.ID, "restore_ready": false, "archive_loss": true, "availability": availability,
				}); err != nil {
				return err
			}
		}
		archive.LossActive = true
		archive.LossObservedAt = observedAt
		archive.Verification = "failed"
	}
	archive.Availability = availability
	archive.ObservedAt = observedAt
	return nil
}

func (s backupAutomationState) archiveLossConditions(previous []notificationpkg.Condition) []notificationpkg.ConditionFact {
	if s.Observation.Owner == "" {
		// A storage outage or a restore cannot establish a new loss or clear
		// previously confirmed evidence simply by making the catalog unknown.
		return nil
	}
	var facts []notificationpkg.ConditionFact
	active := make(map[string]bool)
	for _, archive := range s.Archives {
		if !backupArchiveOwned(archive, s.Observation.Owner) || !archive.LossActive || archive.LossEpisode == 0 || !validBackupArchiveID(archive.ID) {
			continue
		}
		key := backupArchiveLossConditionPrefix + archive.ID + "/" + strconv.FormatUint(archive.LossEpisode, 10)
		active[key] = true
		facts = append(facts, notificationpkg.ConditionFact{
			ID: key, EventType: notificationpkg.EventBackupVerifyFailed, Active: true,
			TargetType: "backup", TargetName: "Application backup", CreatedAt: archive.LossObservedAt,
			Message: "A registered automated backup is missing or has changed. Review the backup catalog in Administration.",
		})
	}
	for _, condition := range previous {
		fact := condition.Fact
		if fact.Active && strings.HasPrefix(fact.ID, backupArchiveLossConditionPrefix) && !active[fact.ID] {
			fact.Active = false
			facts = append(facts, fact)
		}
	}
	return facts
}
