package backup

import (
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestWindowsRestoreTargetIdentity(t *testing.T) {
	t.Run("a recovery path with trailing punctuation is distinct", func(t *testing.T) {
		target := filepath.Join(t.TempDir(), "trust-fixture")
		same, err := sameRestoreEntry(target, target+".")
		if err != nil || same {
			t.Fatalf("distinct recovery names compare equal: same=%v err=%v", same, err)
		}
	})

	t.Run("verification and restore reject the conflicting destination", func(t *testing.T) {
		f := newDurabilityFixture(t, t.TempDir(), testRestoredRuntime{prepare: func() {
			t.Fatal("restore proceeded beyond target validation")
		}})
		archive := restoreStorageFixtureArchive(t, f)
		f.service.deps.KnownHostsWritePath = func() (string, error) {
			return capitalizedRestorePath(RestoreRecoveryMarkerPath(f.db)), nil
		}
		review, err := f.service.VerifyArchiveFileWithReview(t.Context(), archive, testPassphrase, nil)
		if err != nil || review.RestoreReady || !slices.ContainsFunc(review.Blockers, func(issue ReadinessIssue) bool {
			return issue.Code == "known_hosts_target_conflict"
		}) {
			t.Fatalf("restore verification did not report the host-trust conflict: ready=%v blockers=%+v err=%v",
				review.RestoreReady, review.Blockers, err)
		}
		if err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{}); !errors.Is(err, ErrRestoreHostTarget) {
			t.Fatalf("restore did not reject the host-trust conflict: %v", err)
		}
		requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
	})

	t.Run("offline inspection refuses a conflict and accepts alternate spelling", func(t *testing.T) {
		f, _, id := interruptedRecoveryFixture(t, true)
		f.service.deps.KnownHostsWritePath = func() (string, error) {
			return capitalizedRestorePath(f.db), nil
		}
		blocked, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
		if !errors.Is(err, ErrRestoreHostTarget) || !blocked.RecoveryRequired || blocked.Blocker != "known_hosts_target_conflict" {
			t.Fatalf("offline inspection did not report the host-trust conflict: report=%+v err=%v", blocked, err)
		}
		f.service.deps.KnownHostsWritePath = func() (string, error) {
			return capitalizedRestorePath(f.hosts), nil
		}
		review, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
		if err != nil || !review.RecoveryRequired || len(review.Sets) != 1 || !review.Sets[0].Ready || review.Sets[0].ID != id {
			t.Fatalf("offline inspection rejected the intact recovery set: report=%+v err=%v", review, err)
		}
	})
}

func capitalizedRestorePath(path string) string {
	return filepath.Join(filepath.Dir(path), strings.ToUpper(filepath.Base(path)))
}
