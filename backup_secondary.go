package main

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"

	"github.com/gin-gonic/gin"
)

const backupSecondaryRetryRoute = "/api/backup/automation/secondary/retry"

const (
	backupSecondaryMaxAttempts   = 5
	backupSecondaryQueueLimit    = 16
	backupSecondaryRetryBase     = time.Minute
	backupSecondaryValidateRoute = "/api/backup/automation/secondary/validate"
)

type backupSecondaryBinding struct {
	Path      string `json:"path_binding,omitempty"`
	Directory string `json:"directory_identity,omitempty"`
	Owner     string `json:"owner,omitempty"`
}

func (binding backupSecondaryBinding) sameMount(other backupSecondaryBinding) bool {
	return binding.Directory != "" && binding.Directory == other.Directory && binding.Owner == other.Owner
}

type backupSecondaryCopy struct {
	Archive      ownedBackupArchive     `json:"archive"`
	Attempts     int                    `json:"attempts"`
	Transferring bool                   `json:"transferring,omitempty"`
	RetryAt      string                 `json:"retry_at,omitempty"`
	Failure      string                 `json:"failure,omitempty"`
	Temporary    string                 `json:"temporary,omitempty"`
	FileIdentity string                 `json:"file_identity,omitempty"`
	ModifiedAt   string                 `json:"modified_at,omitempty"`
	ChangedAt    string                 `json:"changed_at,omitempty"`
	VerifiedAt   string                 `json:"verified_at,omitempty"`
	Unavailable  bool                   `json:"unavailable,omitempty"`
	Integrity    backupArchiveIntegrity `json:"integrity,omitzero"`
	Retried      bool                   `json:"retried,omitempty"`
}

func (copy backupSecondaryCopy) pending() bool {
	return copy.VerifiedAt == "" && copy.Failure != "queue_full" &&
		(copy.Transferring || copy.Attempts < copy.attemptLimit())
}

func (copy backupSecondaryCopy) attemptLimit() int {
	if copy.Retried {
		return 2 * backupSecondaryMaxAttempts
	}
	return backupSecondaryMaxAttempts
}

func (copy backupSecondaryCopy) retryable() bool {
	return copy.VerifiedAt == "" && !copy.Retried && !copy.pending()
}

type backupSecondaryState struct {
	Binding          backupSecondaryBinding `json:"binding,omitzero"`
	Approved         bool                   `json:"approved"`
	Copies           []backupSecondaryCopy  `json:"copies,omitempty"`
	LastFailure      string                 `json:"last_failure,omitempty"`
	MountUnavailable bool                   `json:"mount_unavailable,omitempty"`
	CatchUp          bool                   `json:"catch_up,omitempty"`
	Retired          []string               `json:"retired,omitempty"`
}

type backupSecondaryStatus struct {
	State           string                          `json:"state"`
	Pending         int                             `json:"pending"`
	Failed          int                             `json:"failed"`
	Failure         string                          `json:"failure,omitempty"`
	CapturedAt      string                          `json:"captured_at,omitempty"`
	VerifiedAt      string                          `json:"verified_at,omitempty"`
	Integrity       backupArchiveIntegrity          `json:"integrity,omitzero"`
	Transferring    int                             `json:"transferring"`
	WaitingForMount bool                            `json:"waiting_for_mount"`
	RetryCandidates []backupSecondaryRetryCandidate `json:"retry_candidates,omitempty"`
}

func (a *backupAutomation) secondaryConfigured(state backupSecondaryState) bool {
	return state.Approved && state.Binding.Path != "" &&
		state.Binding.Path == backupStorageBinding(a.deployment.SecondaryDirectory)
}

func (state *backupAutomationState) selectSecondaryDestination(binding backupSecondaryBinding) {
	if state.Secondary.Binding == binding {
		return
	}
	current := state.Secondary
	next := current
	if !current.Binding.sameMount(binding) {
		next = backupSecondaryState{Binding: binding}
		for i, previous := range state.SecondaryHistory {
			if previous.Binding.sameMount(binding) {
				next = previous
				state.SecondaryHistory = slices.Delete(state.SecondaryHistory, i, i+1)
				break
			}
		}
		if current.Binding.Directory != "" && (len(current.Copies) > 0 || len(current.Retired) > 0) {
			// Keep the catalog and spent attempts with their original directory.
			current.Approved = false
			state.SecondaryHistory = append(state.SecondaryHistory, current)
		}
	}
	for i := range next.Copies {
		if next.Copies[i].VerifiedAt != "" {
			next.Copies[i].Unavailable = true
		}
	}
	next.Binding = binding
	next.Approved = false
	state.Secondary = next
}

func (state *backupAutomationState) recordSecondaryRetirement(id string) bool {
	if slices.Contains(state.Secondary.Retired, id) {
		return false
	}
	for _, source := range state.Archives {
		if source.ID == id {
			state.Secondary.Retired = append(state.Secondary.Retired, id)
			return true
		}
	}
	return false
}

func (a *backupAutomation) secondaryStatus(current backupAutomationState) *backupSecondaryStatus {
	state := current.Secondary
	if a.deployment.SecondaryDirectory == "" {
		return nil
	}
	status := &backupSecondaryStatus{State: "validation_required"}
	if state.Binding.Path != backupStorageBinding(a.deployment.SecondaryDirectory) {
		return status
	}
	var latestVerified, latestCapture time.Time
	var latestPublished backupSecondaryCopy
	hasUnavailableCopies, hasCorruptCopies, hasSkippedCopies := false, false, false
	now := a.now()
	for _, copy := range state.Copies {
		if copy.VerifiedAt != "" && (latestPublished.VerifiedAt == "" || secondaryCapturedAfter(copy, latestPublished)) {
			latestPublished = copy
		}
		if copy.Integrity.State == backupIntegrityMismatch {
			hasCorruptCopies = true
			status.Failed++
		} else if copy.Unavailable || copy.Integrity.State == backupIntegrityUnavailable {
			hasUnavailableCopies = true
			status.Failed++
		} else if secondaryIntegrityFresh(copy, now) {
			at := parseBackupStamp(copy.VerifiedAt)
			captured := parseBackupStamp(copy.Archive.CapturedAt)
			if latestVerified.IsZero() || captured.After(latestCapture) || (captured.Equal(latestCapture) && at.After(latestVerified)) {
				status.CapturedAt, status.VerifiedAt = copy.Archive.CapturedAt, copy.VerifiedAt
				latestCapture, latestVerified = captured, at
			}
		} else if copy.pending() {
			status.Pending++
			if copy.Transferring && copy.Failure != "storage_unavailable" && copy.Failure != "publication_pending" {
				status.Transferring++
			}
		} else if copy.VerifiedAt == "" {
			status.Failed++
			hasSkippedCopies = hasSkippedCopies || copy.Failure == "queue_full"
		}
	}
	if a.secondaryConfigured(state) {
		status.WaitingForMount = state.MountUnavailable
		status.RetryCandidates = secondaryRetryCandidates(current)
		status.Integrity = latestPublished.Integrity
		switch {
		case status.WaitingForMount:
			status.Integrity.State = backupIntegrityUnavailable
		case latestPublished.Unavailable && status.Integrity.State != backupIntegrityMismatch:
			status.Integrity.State = backupIntegrityUnavailable
		case latestPublished.VerifiedAt != "" && !secondaryIntegrityFresh(latestPublished, now) &&
			(status.Integrity.State == "" || status.Integrity.State == backupIntegrityMatched):
			status.Integrity.State = "check_due"
		}
		status.Failure = state.LastFailure
		if status.Failure == "" && hasCorruptCopies {
			status.Failure = "copy_corrupt"
		}
		if status.Failure == "" && hasUnavailableCopies {
			status.Failure = "copy_unavailable"
		}
		if status.Failure == "" && hasSkippedCopies {
			status.Failure = "queue_full"
		}
		if status.Failure == "" && status.WaitingForMount {
			status.Failure = "storage_unavailable"
		}
		switch {
		case status.Failure != "" || status.Failed > 0:
			status.State = "degraded"
		case status.Pending > 0:
			status.State = "pending"
		case status.VerifiedAt != "":
			status.State = "verified"
		case latestPublished.VerifiedAt != "":
			status.State = "integrity_pending"
		default:
			status.State = "waiting"
		}
	}
	return status
}

func (a *backupAutomation) queueSecondaryCopy(state *backupAutomationState, archive ownedBackupArchive) {
	if !a.secondaryConfigured(state.Secondary) || archive.Owner != state.Secondary.Binding.Owner ||
		slices.Contains(state.Secondary.Retired, archive.ID) {
		return
	}
	pending := 0
	for _, copy := range state.Secondary.Copies {
		if copy.Archive.ID == archive.ID {
			return
		}
		if copy.pending() {
			pending++
		}
	}
	copy := backupSecondaryCopy{Archive: archive}
	if pending >= backupSecondaryQueueLimit {
		// Record the uncovered archive without increasing the queue or pinning
		// its primary source. A successful transfer can clear LastFailure.
		copy.Failure = "queue_full"
		state.Secondary.LastFailure = copy.Failure
	}
	state.Secondary.Copies = append(state.Secondary.Copies, copy)
	// Bound historical failures alongside the pending source pins. Temp
	// registrations remain until the worker can safely clean the approved mount.
	sources := make(map[string]bool, len(state.Archives))
	for _, archive := range state.Archives {
		sources[archive.ID] = true
	}
	state.Secondary.Retired = slices.DeleteFunc(state.Secondary.Retired, func(id string) bool {
		return !sources[id]
	})
	failed := 0
	for i := len(state.Secondary.Copies) - 1; i >= 0; i-- {
		copy := state.Secondary.Copies[i]
		if copy.VerifiedAt == "" && !copy.pending() && copy.Temporary == "" && !sources[copy.Archive.ID] {
			failed++
			if failed > backupSecondaryQueueLimit {
				state.Secondary.Copies = slices.Delete(state.Secondary.Copies, i, i+1)
			}
		}
	}
}

func (a *backupAutomation) secondaryProtectsSource(state backupSecondaryState, id string) bool {
	if !a.secondaryConfigured(state) {
		return false
	}
	for _, copy := range state.Copies {
		if copy.Archive.ID == id && copy.pending() {
			return true
		}
	}
	return false
}

func (a *backupAutomation) openSecondaryDestination() (*os.Root, backupSecondaryBinding, error) {
	var binding backupSecondaryBinding
	if a.deployment.SecondaryDirectory == "" {
		return nil, binding, errors.New("a secondary backup directory has not been configured")
	}
	directory, err := backupRealPath(a.deployment.SecondaryDirectory)
	if err != nil {
		return nil, binding, errors.New("secondary backup storage is unavailable or contains a symbolic link")
	}
	primary := filepath.Clean(a.deployment.Directory)
	paths := a.deps.BackupService.Status()
	dataPaths := []string{primary, string(filepath.Separator)}
	protected := []string{primary, filepath.Dir(a.deployment.SecretFile)}
	for _, path := range []string{paths.DBPath, paths.ConfigPath, paths.KnownHostsPath} {
		if path != "" {
			dataPaths = append(dataPaths, filepath.Dir(path))
			protected = append(protected, filepath.Dir(path))
		}
	}
	for _, path := range protected {
		if pathWithin(path, directory) || pathWithin(directory, path) {
			return nil, binding, errors.New("secondary backup storage must be separate from the primary archive, secret and application data directories")
		}
	}
	owner, err := a.localIdentity(false)
	if err != nil {
		return nil, binding, errors.New("validate the primary backup destination before the secondary destination")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return nil, binding, errors.New("secondary backup storage cannot be opened")
	}
	identity := backupArchiveDirectoryIdentity(root)
	if identity == "" || !backupArchiveMountAvailable(directory, identity) {
		_ = root.Close()
		return nil, binding, errors.New("secondary backup storage could not be identified safely")
	}
	for _, path := range protected {
		if info, err := os.Stat(path); err == nil && backupCatalogFileIdentity(info) == identity {
			_ = root.Close()
			return nil, binding, errors.New("secondary backup storage aliases another application directory")
		}
	}
	for _, path := range dataPaths {
		if info, err := os.Stat(path); err == nil && backupFilesystemDevice(backupCatalogFileIdentity(info)) == backupFilesystemDevice(identity) {
			_ = root.Close()
			return nil, binding, errors.New("secondary backup storage must be mounted separately from the primary archives and application data")
		}
	}
	return root, backupSecondaryBinding{Path: backupStorageBinding(directory), Directory: identity, Owner: owner}, nil
}

func backupFilesystemDevice(identity string) string {
	device, _, _ := strings.Cut(identity, ":")
	return device
}

func (a *backupAutomation) validateSecondaryDestination(ctx context.Context, admission *maintenancepkg.SharedLease) (*backupSecondaryStatus, error) {
	if !a.secondaryMu.TryLock() {
		return nil, errors.New("wait for the current secondary backup before validating the destination")
	}
	defer a.secondaryMu.Unlock()
	if admission == nil {
		lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
		if !decision.Allowed {
			return nil, errBackupSecondaryDeferred
		}
		admission = lease
	}
	ctx, cancel := context.WithCancel(ctx)
	a.setSecondaryCancellation(cancel)
	defer func() {
		cancel()
		a.setSecondaryCancellation(nil)
	}()
	// A restore can cancel validation once we release authentication's lease.
	// All operations on the secondary filesystem run outside maintenance admission.
	admission.Close()
	root, binding, err := a.openSecondaryDestination()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := a.probeSecondaryPublication(ctx, root); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return nil, errors.New("secondary backup storage changed during validation")
	}

	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		return nil, errBackupSecondaryDeferred
	}
	defer lease.Close()
	if !a.mu.TryLock() {
		return nil, errBackupSecondaryDeferred
	}
	defer a.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	state, err := a.load(ctx)
	if err != nil {
		return nil, errors.New("secondary backup settings are unavailable")
	}
	state.selectSecondaryDestination(binding)
	if !state.Secondary.Approved {
		state.Secondary.CatchUp = true
	}
	state.Secondary.Approved = true
	if err := a.save(ctx, state); err != nil {
		return nil, errors.New("secondary backup validation could not be saved")
	}
	return a.secondaryStatus(state), nil
}

func (a *backupAutomation) probeSecondaryPublication(ctx context.Context, root *os.Root) error {
	name := ".slu-secondary-validation-" + rand.Text()
	linkedName := name + ".link"
	probe, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("secondary backup storage is not writable")
	}
	info, statErr := probe.Stat()
	writeErr := errors.Join(statErr, probe.Sync(), probe.Close())
	var linkErr, removeLinkErr error
	linkedToProbe := false
	if writeErr == nil && ctx.Err() == nil {
		linkErr = a.linkSecondaryArchive(root, name, linkedName)
		linked, err := root.Lstat(linkedName)
		linkedToProbe = err == nil && os.SameFile(info, linked)
		// A failed response can still leave the link on remote storage. Remove
		// it only when it refers to the probe we created.
		if linkedToProbe {
			removeLinkErr = root.Remove(linkedName)
		}
	}
	cleanupErr := errors.Join(removeLinkErr, root.Remove(name), a.syncSecondaryDirectory(root))
	if err := ctx.Err(); err != nil {
		return err
	}
	if writeErr != nil || cleanupErr != nil {
		return errors.New("secondary backup storage write validation or cleanup failed")
	}
	if linkErr != nil || !linkedToProbe {
		return errors.New("secondary backup storage must support atomic hard links")
	}
	return nil
}

func handleBackupSecondaryValidate(c *gin.Context, automation *backupAutomation) {
	setNoStoreHeaders(c)
	value, _ := c.Get(maintenanceSharedLeaseContextKey)
	lease, _ := value.(*maintenancepkg.SharedLease)
	status, err := automation.validateSecondaryDestination(c.Request.Context(), lease)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, status)
}
