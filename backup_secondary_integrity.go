package main

import (
	"context"
	"errors"
	"os"
	"time"
)

func secondaryCapturedAfter(copy, other backupSecondaryCopy) bool {
	captured, otherCaptured := parseBackupStamp(copy.Archive.CapturedAt), parseBackupStamp(other.Archive.CapturedAt)
	if !captured.Equal(otherCaptured) {
		return captured.After(otherCaptured)
	}
	return parseBackupStamp(copy.VerifiedAt).After(parseBackupStamp(other.VerifiedAt))
}

func secondaryIntegrityFresh(copy backupSecondaryCopy, now time.Time) bool {
	checked := parseBackupStamp(copy.Integrity.CheckedAt)
	return copy.VerifiedAt != "" && !copy.Unavailable && copy.Integrity.State == backupIntegrityMatched &&
		!checked.IsZero() && !checked.After(now) && now.Sub(checked) < backupIntegrityOtherAfter
}

func secondaryIntegrityCandidate(copies []backupSecondaryCopy, now time.Time) (backupSecondaryCopy, bool) {
	var newest, oldestDue *backupSecondaryCopy
	for i := range copies {
		copy := &copies[i]
		if newest == nil || secondaryCapturedAfter(*copy, *newest) {
			newest = copy
		}
		if secondaryIntegrityDue(*copy, now, backupIntegrityOtherAfter) && (oldestDue == nil ||
			parseBackupStamp(copy.Integrity.AttemptedAt).Before(parseBackupStamp(oldestDue.Integrity.AttemptedAt))) {
			oldestDue = copy
		}
	}
	if newest != nil && secondaryIntegrityDue(*newest, now, backupIntegrityNewestAfter) {
		return *newest, true
	}
	if oldestDue != nil {
		return *oldestDue, true
	}
	return backupSecondaryCopy{}, false
}

func secondaryIntegrityDue(copy backupSecondaryCopy, now time.Time, interval time.Duration) bool {
	if copy.Unavailable && copy.Integrity.State == backupIntegrityMatched {
		return true
	}
	return backupIntegrityCheckDue(copy.Integrity, now, interval)
}

// The secondary worker calls this at most once per cycle. The digest reader
// is bounded to one archive and uses the worker's timeout and restore cancellation.
func (a *backupAutomation) checkSecondaryIntegrity(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copies []backupSecondaryCopy) error {
	copy, due := secondaryIntegrityCandidate(copies, a.now())
	if !due {
		return nil
	}
	_, err := a.verifyAndRecordSecondaryIntegrity(ctx, root, binding, copy)
	return err
}

func (a *backupAutomation) verifyAndRecordSecondaryIntegrity(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy) (backupSecondaryCopy, error) {
	state, info := inspectSecondaryIntegrity(ctx, root, binding, copy)
	at := backupStamp(a.now())
	if err := ctx.Err(); err != nil {
		return backupSecondaryCopy{}, err
	}
	if info != nil && !secondaryIntegrityNameMatches(root, copy.Archive.Name, info) {
		state, info = backupIntegrityUnavailable, nil
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return backupSecondaryCopy{}, errBackupSecondaryDeferred
	}
	var verified backupSecondaryCopy
	err := a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !sameSecondaryIntegrityRegistration(*current, copy) {
			return false, errBackupSecondaryDeferred
		}
		current.Integrity.State = state
		current.Integrity.AttemptedAt = at
		if info != nil {
			current.Integrity.CheckedAt = at
		}
		if state == backupIntegrityMatched {
			current.Unavailable = false
			current.ModifiedAt = backupStamp(info.ModTime())
			current.ChangedAt = backupStamp(backupIntegrityChangeTime(info))
		}
		verified = *current
		return true, nil
	})
	if err != nil || info == nil {
		return verified, err
	}
	if secondaryIntegrityNameMatches(root, copy.Archive.Name, info) &&
		backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return verified, nil
	}
	// The name can change while SQLite is saving the observation. Discard that
	// proof without checking the mount under maintenance admission.
	retractCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), backupArchiveObservationBudget)
	defer cancel()
	err = a.updateCurrentSecondary(retractCtx, binding, copy.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if !sameSecondaryIntegrityRegistration(*current, verified) {
			return false, errBackupSecondaryDeferred
		}
		current.Integrity = backupArchiveIntegrity{
			State: backupIntegrityUnavailable, CheckedAt: copy.Integrity.CheckedAt, AttemptedAt: at,
		}
		current.Unavailable = true
		current.ModifiedAt, current.ChangedAt = copy.ModifiedAt, copy.ChangedAt
		verified = *current
		return true, nil
	})
	return verified, err
}

func sameSecondaryIntegrityRegistration(current, observed backupSecondaryCopy) bool {
	return current.VerifiedAt == observed.VerifiedAt && current.FileIdentity == observed.FileIdentity &&
		current.Archive.Name == observed.Archive.Name && current.Archive.SHA256 == observed.Archive.SHA256 &&
		current.Integrity == observed.Integrity
}

func secondaryIntegrityNameMatches(root *os.Root, name string, observed os.FileInfo) bool {
	named, err := root.Lstat(name)
	return err == nil && sameBackupIntegrityFile(observed, named)
}

func inspectSecondaryIntegrity(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy) (string, os.FileInfo) {
	if !backupArchiveOwned(copy.Archive, binding.Owner) || !validBackupIntegrityDigest(copy.Archive.SHA256) {
		return backupIntegrityUnavailable, nil
	}
	before, err := root.Lstat(copy.Archive.Name)
	if err != nil || !before.Mode().IsRegular() || before.Mode().Perm()&0444 == 0 ||
		backupCatalogFileIdentity(before) != copy.FileIdentity {
		return backupIntegrityUnavailable, nil
	}
	checked, err := verifySecondaryFile(ctx, root, copy.Archive.Name, copy.FileIdentity, copy.Archive)
	if checked == nil || (err != nil && !errors.Is(err, errBackupSecondaryMismatch)) {
		return backupIntegrityUnavailable, nil
	}
	if !sameBackupIntegrityFile(before, checked) {
		return backupIntegrityUnavailable, nil
	}
	if err != nil {
		return backupIntegrityMismatch, checked
	}
	return backupIntegrityMatched, checked
}
