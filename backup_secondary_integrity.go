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
	state, info := inspectSecondaryIntegrity(ctx, root, binding, copy)
	if err := ctx.Err(); err != nil {
		return err
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return errBackupSecondaryDeferred
	}
	return a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
		if current.VerifiedAt != copy.VerifiedAt || current.FileIdentity != copy.FileIdentity ||
			current.Archive.Name != copy.Archive.Name || current.Archive.SHA256 != copy.Archive.SHA256 ||
			current.Integrity != copy.Integrity {
			return false, errBackupSecondaryDeferred
		}
		at := backupStamp(a.now())
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
		return true, nil
	})
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
	after, statErr := root.Lstat(copy.Archive.Name)
	if statErr != nil || !sameBackupIntegrityFile(before, checked) || !sameBackupIntegrityFile(checked, after) {
		return backupIntegrityUnavailable, nil
	}
	if err != nil {
		return backupIntegrityMismatch, checked
	}
	return backupIntegrityMatched, checked
}
