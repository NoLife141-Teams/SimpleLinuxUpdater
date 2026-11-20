package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"sort"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"

	"github.com/gin-gonic/gin"
)

type backupSecondaryRetryCandidate struct {
	ArchiveID  string `json:"archive_id"`
	CapturedAt string `json:"captured_at,omitempty"`
}

var errSecondaryRetryUnavailable = errors.New("secondary backup retry requires an eligible verified primary archive")
var errSecondaryRetryQueueFull = errors.New("secondary backup queue is full")

func secondaryPrimaryEligible(archive ownedBackupArchive, owner string) bool {
	return backupArchiveOwned(archive, owner) && validBackupArchiveID(archive.ID) &&
		archive.DirectoryIdentity != "" && archive.FileIdentity != "" &&
		archive.Verification == "verified" && archive.Availability == "present" &&
		!archive.LossActive && archive.Integrity.State != backupIntegrityMismatch &&
		validBackupIntegrityDigest(archive.SHA256) && archive.SizeBytes != nil &&
		*archive.SizeBytes > 0 && *archive.SizeBytes <= internalbackup.MaxUploadBytes
}

func secondaryPrimaryArchive(state *backupAutomationState, id string) (ownedBackupArchive, bool) {
	for _, archive := range state.Archives {
		if archive.ID == id && secondaryPrimaryEligible(archive, state.Secondary.Binding.Owner) {
			return archive, true
		}
	}
	return ownedBackupArchive{}, false
}

func secondaryRetryCandidates(state backupAutomationState) []backupSecondaryRetryCandidate {
	var candidates []backupSecondaryRetryCandidate
	for i := len(state.Secondary.Copies) - 1; i >= 0 && len(candidates) < backupSecondaryQueueLimit; i-- {
		copy := state.Secondary.Copies[i]
		if copy.retryable() {
			if archive, ok := secondaryPrimaryArchive(&state, copy.Archive.ID); ok {
				candidates = append(candidates, backupSecondaryRetryCandidate{ArchiveID: archive.ID, CapturedAt: archive.CapturedAt})
			}
		}
	}
	return candidates
}

func secondaryPendingCount(state backupSecondaryState) int {
	pending := 0
	for _, copy := range state.Copies {
		if copy.pending() {
			pending++
		}
	}
	return pending
}

// A catch-up is scoped to the approved mount and admits at most one archive.
// An exhausted transfer needs explicit retry. A skipped archive can still use
// its first budget, and a completed copy can be replaced if its name is absent.
func (a *backupAutomation) catchUpSecondaryCopy(ctx context.Context, root *os.Root, binding backupSecondaryBinding) error {
	var candidates []ownedBackupArchive
	copies := make(map[string]backupSecondaryCopy)
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding ||
			secondaryPendingCount(state.Secondary) >= backupSecondaryQueueLimit {
			return false, nil
		}
		for _, copy := range state.Secondary.Copies {
			copies[copy.Archive.ID] = copy
		}
		for _, archive := range state.Archives {
			copy, exists := copies[archive.ID]
			if (!exists || copy.Failure == "queue_full" || copy.VerifiedAt != "") &&
				secondaryPrimaryEligible(archive, binding.Owner) {
				candidates = append(candidates, archive)
			}
		}
		return false, nil
	})
	if err != nil {
		return err
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return parseBackupStamp(candidates[i].CapturedAt).After(parseBackupStamp(candidates[j].CapturedAt))
	})
	unavailable := false
	for _, candidate := range candidates {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := root.Lstat(candidate.Name); !errors.Is(err, os.ErrNotExist) {
			unavailable = unavailable || err != nil
			continue
		}
		if a.scanArchiveIntegrity(ctx, candidate) != backupIntegrityMatched {
			unavailable = true
			continue
		}
		if _, err := root.Lstat(candidate.Name); !errors.Is(err, os.ErrNotExist) {
			unavailable = unavailable || err != nil
			continue
		}
		if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
			return errBackupSecondaryDeferred
		}
		return a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
			if !a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding ||
				secondaryPendingCount(state.Secondary) >= backupSecondaryQueueLimit {
				return false, nil
			}
			current, ok := secondaryPrimaryArchive(state, candidate.ID)
			if !ok || !sameBackupIntegrityRegistration(current, candidate) {
				return false, nil
			}
			replacement := backupSecondaryCopy{Archive: current}
			existing, exists := copies[candidate.ID]
			if exists {
				for i := range state.Secondary.Copies {
					if copy := &state.Secondary.Copies[i]; copy.Archive.ID == candidate.ID &&
						copy.VerifiedAt == existing.VerifiedAt && copy.Failure == existing.Failure {
						*copy = replacement
						state.Secondary.CatchUp = false
						return true, nil
					}
				}
				return false, nil
			}
			a.queueSecondaryCopy(state, current)
			state.Secondary.CatchUp = false
			return true, nil
		})
	}
	if !unavailable && backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return a.updateCurrentSecondary(ctx, binding, "", func(state *backupSecondaryState, _ *backupSecondaryCopy) (bool, error) {
			if secondaryPendingCount(*state) >= backupSecondaryQueueLimit || !state.CatchUp {
				return false, nil
			}
			state.CatchUp = false
			return true, nil
		})
	}
	return nil
}

func (a *backupAutomation) retrySecondaryCopy(parent context.Context, admission *maintenancepkg.SharedLease, id string) error {
	if !validBackupArchiveID(id) {
		return errSecondaryRetryUnavailable
	}
	if !a.secondaryMu.TryLock() {
		return errBackupSecondaryDeferred
	}
	defer a.secondaryMu.Unlock()
	if admission == nil {
		lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
		if !decision.Allowed {
			return errBackupSecondaryDeferred
		}
		admission = lease
	}
	ctx, cancel := context.WithTimeout(parent, backupSecondaryCopyTimeout)
	a.setSecondaryCancellation(cancel)
	defer func() {
		cancel()
		a.setSecondaryCancellation(nil)
	}()
	// Register cancellation before releasing the authenticated admission, so
	// this request cannot cross a restore between authentication and inspection.
	admission.Close()
	var source ownedBackupArchive
	var binding backupSecondaryBinding
	err := a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		if !a.secondaryConfigured(state.Secondary) {
			return false, errSecondaryRetryUnavailable
		}
		for _, copy := range state.Secondary.Copies {
			if copy.Archive.ID != id || !copy.retryable() {
				continue
			}
			if secondaryPendingCount(state.Secondary) >= backupSecondaryQueueLimit {
				return false, errSecondaryRetryQueueFull
			}
			if archive, ok := secondaryPrimaryArchive(state, id); ok {
				source, binding = archive, state.Secondary.Binding
				return false, nil
			}
		}
		return false, errSecondaryRetryUnavailable
	})
	if err != nil {
		return err
	}
	root, actual, err := a.openSecondaryDestination()
	if err != nil {
		return err
	}
	defer root.Close()
	if actual != binding {
		return errBackupSecondaryDeferred
	}
	if a.scanArchiveIntegrity(ctx, source) != backupIntegrityMatched ||
		!backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return errSecondaryRetryUnavailable
	}
	return a.withSecondaryState(ctx, func(state *backupAutomationState) (bool, error) {
		current, ok := secondaryPrimaryArchive(state, id)
		if !ok || !sameBackupIntegrityRegistration(current, source) ||
			!a.secondaryConfigured(state.Secondary) || state.Secondary.Binding != binding {
			return false, errSecondaryRetryUnavailable
		}
		if secondaryPendingCount(state.Secondary) >= backupSecondaryQueueLimit {
			return false, errSecondaryRetryQueueFull
		}
		for i := range state.Secondary.Copies {
			copy := &state.Secondary.Copies[i]
			if copy.Archive.ID != id || !copy.retryable() {
				continue
			}
			if copy.Failure == "queue_full" {
				// Older versions used the attempt limit as a sentinel for a
				// copy which had never obtained a queue slot.
				copy.Attempts = 0
			} else {
				copy.Retried = true
			}
			if state.Secondary.LastFailure == copy.Failure {
				state.Secondary.LastFailure = ""
			}
			copy.Archive, copy.RetryAt, copy.Failure = current, "", ""
			return true, nil
		}
		return false, errSecondaryRetryUnavailable
	})
}

func handleBackupSecondaryRetry(c *gin.Context, automation *backupAutomation) {
	setNoStoreHeaders(c)
	var request struct {
		ArchiveID string `json:"archive_id"`
	}
	if err := c.ShouldBindJSON(&request); err != nil || !validBackupArchiveID(request.ArchiveID) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Choose a registered primary archive to retry."})
		return
	}
	value, _ := c.Get(maintenanceSharedLeaseContextKey)
	lease, _ := value.(*maintenancepkg.SharedLease)
	if err := automation.retrySecondaryCopy(c.Request.Context(), lease, request.ArchiveID); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"state": "pending"})
}
