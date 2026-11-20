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
	return copy.VerifiedAt != "" && !copy.Unavailable && copy.Integrity.State == backupIntegrityMatched &&
		secondaryDigestRecent(copy.Integrity.CheckedAt, now)
}

func secondaryDigestRecent(timestamp string, now time.Time) bool {
	checked := parseBackupStamp(timestamp)
	return !checked.IsZero() && !checked.After(now) && now.Sub(checked) < backupIntegrityOtherAfter
}

func secondaryRetentionConfirmed(copy backupSecondaryCopy, beforeID string, now time.Time) bool {
	return secondaryIntegrityFresh(copy, now) && copy.Retention.BeforeID == beforeID &&
		secondaryDigestRecent(copy.Retention.CheckedAt, now)
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
	_, err := a.verifyAndRecordSecondaryIntegrity(ctx, root, binding, copy, "")
	return err
}

func (a *backupAutomation) verifyAndRecordSecondaryIntegrity(ctx context.Context, root *os.Root, binding backupSecondaryBinding, copy backupSecondaryCopy, beforeID string) (backupSecondaryCopy, error) {
	state, info := inspectSecondaryIntegrity(ctx, root, binding, copy)
	if err := ctx.Err(); err != nil {
		return backupSecondaryCopy{}, err
	}
	if !backupArchiveMountAvailable(root.Name(), binding.Directory) {
		return backupSecondaryCopy{}, errBackupSecondaryDeferred
	}
	var verified backupSecondaryCopy
	err := a.updateCurrentSecondary(ctx, binding, copy.Archive.ID, func(_ *backupSecondaryState, current *backupSecondaryCopy) (bool, error) {
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
			modifiedAt := backupStamp(info.ModTime())
			changedAt := backupStamp(backupIntegrityChangeTime(info))
			if modifiedAt != current.ModifiedAt || changedAt != current.ChangedAt {
				current.Retention = secondaryRetentionProof{}
			}
			current.ModifiedAt, current.ChangedAt = modifiedAt, changedAt
			if beforeID != "" {
				current.Retention = secondaryRetentionProof{BeforeID: beforeID, CheckedAt: at}
			}
		} else {
			current.Retention = secondaryRetentionProof{}
		}
		verified = *current
		return true, nil
	})
	return verified, err
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
