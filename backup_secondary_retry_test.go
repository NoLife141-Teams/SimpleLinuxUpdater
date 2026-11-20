package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"

	"github.com/gin-gonic/gin"
)

func TestSecondaryCatchUpSurvivesRestartAndNewMountApproval(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	for range 2 {
		*now = now.Add(time.Minute)
		if err := automation.Tick(t.Context(), true); err != nil {
			t.Fatal(err)
		}
	}
	primary := backupAutomationStateForTest(t, automation).Archives
	archive := primary[len(primary)-1]
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	state := backupAutomationStateForTest(t, restarted)
	if len(state.Secondary.Copies) != 1 || state.Secondary.Copies[0].Archive.ID != archive.ID ||
		!restarted.secondaryProtectsSource(state.Secondary, archive.ID) {
		t.Fatalf("latest existing archive was not queued and pinned: %+v", state.Secondary)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	state = backupAutomationStateForTest(t, restarted)
	state.Secondary.Copies[0].Attempts = backupSecondaryMaxAttempts + 1
	state.Secondary.Copies[0].Retried = true
	if err := restarted.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	offline := secondary + ".offline"
	if err := os.Rename(secondary, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(offline) })
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(offline, archive.Name)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(offline, secondary); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(secondary, archive.Name)); err != nil {
		t.Fatalf("returning mount did not catch up the missing registered copy: %v", err)
	}
	state = backupAutomationStateForTest(t, restarted)
	if copy := state.Secondary.Copies[0]; copy.Attempts != backupSecondaryMaxAttempts+2 || !copy.Retried {
		t.Fatalf("catch-up reset the attempt budget on the same mount: %+v", copy)
	}
	previous := secondary + ".previous"
	if err := os.Rename(secondary, previous); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(previous) })
	if err := os.Mkdir(secondary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(secondary); err != nil || len(entries) != 0 {
		t.Fatalf("replacement mount received writes without approval: %v, %v", entries, err)
	}
	if _, err := restarted.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	state = backupAutomationStateForTest(t, restarted)
	if len(state.Secondary.Copies) != 1 || state.Secondary.Copies[0].Archive.ID != archive.ID ||
		state.Secondary.Copies[0].VerifiedAt == "" {
		t.Fatalf("approved replacement was not caught up: %+v", state.Secondary)
	}
	if _, err := os.Stat(filepath.Join(previous, archive.Name)); err != nil {
		t.Fatalf("old mount was changed after validating its replacement: %v", err)
	}
}

func TestSecondaryCatchUpAdmitsNewestSkippedCopyWhenSlotOpens(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, automation)
	// The next export finds a full queue. Only the first copy is due when
	// the approved mount returns and releases an admission slot.
	for len(state.Secondary.Copies) < backupSecondaryQueueLimit {
		id, err := newBackupArchiveID()
		if err != nil {
			t.Fatal(err)
		}
		future := state.Archives[0]
		future.ID = id
		state.Secondary.Copies = append(state.Secondary.Copies, backupSecondaryCopy{
			Archive: future, RetryAt: backupStamp(now.Add(time.Hour)),
		})
	}
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	offline := secondary + ".offline"
	if err := os.Rename(secondary, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(offline) })
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(backupSecondaryRetryBase)
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || !status.Secondary.WaitingForMount ||
		status.Secondary.Failure != "queue_full" || status.Secondary.Failed != 1 {
		t.Fatalf("queue overflow hid the durable mount outage: %+v, %v", status.Secondary, err)
	}
	if err := os.Rename(offline, secondary); err != nil {
		t.Fatal(err)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err = restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || status.Secondary.WaitingForMount {
		t.Fatalf("mounted destination still reported a mount outage: %+v, %v", status.Secondary, err)
	}
	state = backupAutomationStateForTest(t, restarted)
	newest := state.Archives[len(state.Archives)-1]
	if !restarted.secondaryProtectsSource(state.Secondary, newest.ID) {
		t.Fatal("newest skipped archive did not get the first available queue slot")
	}
}

func TestSecondaryReapprovalUsesOriginalMountCatalogAndRetryBudget(t *testing.T) {
	for _, tc := range []struct {
		name    string
		newPath bool
	}{
		{name: "same configured path"},
		{name: "changed configured path", newPath: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSecondaryReapprovalUsesOriginalMountCatalogAndRetryBudget(t, tc.newPath)
		})
	}
}

func testSecondaryReapprovalUsesOriginalMountCatalogAndRetryBudget(t *testing.T, newPath bool) {
	t.Helper()
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		*now = now.Add(time.Minute)
		if err := automation.Tick(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}

	state := backupAutomationStateForTest(t, automation)
	present, missing := state.Secondary.Copies[0], state.Secondary.Copies[1]
	// Both budgets are already spent for the most recent copy on this mount.
	state.Secondary.Copies[1].Attempts = 2 * backupSecondaryMaxAttempts
	state.Secondary.Copies[1].Retried = true
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(secondary, missing.Archive.Name)); err != nil {
		t.Fatal(err)
	}
	original := secondary + ".original"
	replacement := secondary + ".replacement"
	t.Cleanup(func() {
		_ = os.RemoveAll(original)
		_ = os.RemoveAll(replacement)
	})
	if err := os.Rename(secondary, original); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(secondary, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(secondary, missing.Archive.Name)); err != nil {
		t.Fatalf("replacement mount did not get its own copy: %v", err)
	}
	if err := os.Rename(secondary, replacement); err != nil {
		t.Fatal(err)
	}
	if newPath {
		secondary = original
		automation.deployment.SecondaryDirectory = secondary
	} else if err := os.Rename(original, secondary); err != nil {
		t.Fatal(err)
	}

	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	if _, err := restarted.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || status.Secondary.CapturedAt != present.Archive.CapturedAt ||
		status.Secondary.Pending != 0 || len(status.Secondary.RetryCandidates) != 0 {
		t.Fatalf("returning mount lost original catalog or retried an exhausted copy: %+v, %v", status.Secondary, err)
	}
	if _, err := os.Stat(filepath.Join(secondary, missing.Archive.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("returning mount published another copy with a fresh budget: %v", err)
	}
}

func TestSecondaryCatchUpBudgetSurvivesFailedHistoryPruning(t *testing.T) {
	automation := backupAutomation{deployment: backupAutomationDeployment{SecondaryDirectory: "/backups"}}
	binding := backupSecondaryBinding{Path: backupStorageBinding("/backups"), Owner: "owner"}
	oldest := ownedBackupArchive{ID: "registered", Owner: binding.Owner}
	state := backupAutomationState{
		Archives: []ownedBackupArchive{oldest},
		Secondary: backupSecondaryState{Approved: true, Binding: binding, Copies: []backupSecondaryCopy{
			{Archive: oldest, Attempts: 2 * backupSecondaryMaxAttempts, Retried: true, Failure: "copy_failed"},
		}},
	}
	for range backupSecondaryQueueLimit {
		state.Secondary.Copies = append(state.Secondary.Copies, backupSecondaryCopy{Failure: "queue_full"})
	}
	automation.queueSecondaryCopy(&state, ownedBackupArchive{ID: "new", Owner: binding.Owner})
	automation.queueSecondaryCopy(&state, oldest)
	for _, copy := range state.Secondary.Copies {
		if copy.Archive.ID == oldest.ID && !copy.pending() && !copy.retryable() {
			return
		}
	}
	t.Fatal("pruned terminal outcome reset the budget of an archive still in the primary catalog")
}

func TestSecondaryCatchUpDoesNotRequeueCopiesRemovedByRetention(t *testing.T) {
	for _, tc := range []struct {
		name      string
		syncFails bool
	}{
		{name: "completed retirement"},
		{name: "interrupted after removal", syncFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSecondaryCatchUpDoesNotRequeueCopiesRemovedByRetention(t, tc.syncFails)
		})
	}
}

func testSecondaryCatchUpDoesNotRequeueCopiesRemovedByRetention(t *testing.T, syncFails bool) {
	t.Helper()
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		*now = now.Add(time.Minute)
		if err := automation.Tick(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	state := backupAutomationStateForTest(t, automation)
	oldest := state.Archives[0]
	config := state.Config
	config.Retain = 1
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	var expectedError error
	if syncFails {
		expectedError = errors.New("secondary retirement directory sync interrupted after removal")
		automation.syncSecondaryDirectory = func(*os.Root) error { return expectedError }
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, expectedError) {
		t.Fatalf("secondary retention = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secondary, oldest.Name)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retention did not remove the older secondary copy: %v", err)
	}
	if _, err := os.Stat(filepath.Join(automation.deployment.Directory, oldest.Name)); err != nil {
		t.Fatalf("fixture lost the registered primary source: %v", err)
	}
	offline := secondary + ".offline"
	if err := os.Rename(secondary, offline); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(offline) })
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(offline, secondary); err != nil {
		t.Fatal(err)
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || status.Secondary.Pending != 0 || status.Secondary.State != "verified" {
		t.Fatalf("mount recovery queued a copy already removed by retention: %+v, %v", status.Secondary, err)
	}
}

func TestSecondaryLateMountFailureBeforeTransferPreservesBudget(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires filesystem traversal permissions to be enforced")
	}
	for _, tc := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "not traversable", mode: 0600},
		{name: "readable but not writable", mode: 0500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			testSecondaryLateMountFailureBeforeTransfer(t, tc.mode)
		})
	}
}

func testSecondaryLateMountFailureBeforeTransfer(t *testing.T, mode os.FileMode) {
	t.Helper()
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	t.Cleanup(func() { _ = os.Chmod(secondary, 0700) })
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	published := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
	if err := os.Remove(filepath.Join(secondary, published.Archive.Name)); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	getDB := automation.deps.DB
	calls := 0
	automation.deps.DB = func() *sql.DB {
		calls++
		if calls == 2 {
			// The worker already opened and observed the secondary mount.
			// Remove access just before it starts the new transfer.
			if err := os.Chmod(secondary, mode); err != nil {
				t.Fatal(err)
			}
		}
		return getDB()
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err == nil {
		t.Fatal("late mount failure did not defer the transfer")
	}
	automation.deps.DB = getDB
	state := backupAutomationStateForTest(t, automation)
	if copy := state.Secondary.Copies[1]; copy.Attempts != 0 || copy.Failure != "storage_unavailable" {
		t.Fatalf("late mount failure spent a transfer attempt: %+v", copy)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := automation.Status(t.Context())
	if err != nil || status.Secondary == nil || !status.Secondary.WaitingForMount {
		t.Fatalf("mount wait disappeared during transfer backoff: %+v, %v", status.Secondary, err)
	}
}

func TestSecondaryRetryRegistersRestoreCancellationBeforeReleasingAdmission(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	id, err := newBackupArchiveID()
	if err != nil {
		t.Fatal(err)
	}
	coordinator := automation.deps.MaintenanceCoordinator
	admission, decision := coordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		t.Fatal("retry fixture could not obtain authenticated admission")
	}
	defer admission.Close()
	automation.secondaryCancelMu.Lock()
	result := make(chan error, 1)
	go func() { result <- automation.retrySecondaryCopy(ctx, admission, id) }()
	for automation.secondaryMu.TryLock() {
		automation.secondaryMu.Unlock()
		if ctx.Err() != nil {
			automation.secondaryCancelMu.Unlock()
			t.Fatal("retry did not reach the restore cancellation boundary")
		}
		time.Sleep(time.Millisecond)
	}
	restore, restoreDecision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if restore != nil {
		restore.Close()
	}
	automation.secondaryCancelMu.Unlock()
	select {
	case <-result:
	case <-ctx.Done():
		t.Fatal("retry remained blocked after cancellation registration")
	}
	if restoreDecision.Allowed {
		t.Fatal("restore could start after authentication but before the retry registered cancellation")
	}
}

func TestSecondaryRetryChecksSourceAndAdmitsSingleBoundedRetry(t *testing.T) {
	app, automation, _ := newBackupAutomationTest(t)
	automation.deployment.SecondaryDirectory = secondaryBackupTestDirectory(t, automation)
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, automation)
	archive := state.Archives[0]
	state.Secondary.Copies[0].Attempts = backupSecondaryMaxAttempts
	state.Secondary.Copies[0].Failure = "copy_failed"
	for len(state.Secondary.Copies) <= backupSecondaryQueueLimit {
		id, err := newBackupArchiveID()
		if err != nil {
			t.Fatal(err)
		}
		fixture := archive
		fixture.ID = id
		state.Secondary.Copies = append(state.Secondary.Copies, backupSecondaryCopy{Archive: fixture})
	}
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if err := automation.retrySecondaryCopy(t.Context(), nil, archive.ID); !errors.Is(err, errSecondaryRetryQueueFull) {
		t.Fatalf("retry on a full queue = %v", err)
	}
	state.Secondary.Copies = state.Secondary.Copies[:1]
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(automation.deployment.Directory, archive.Name)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".hidden"); err != nil {
		t.Fatal(err)
	}
	if err := automation.retrySecondaryCopy(t.Context(), nil, archive.ID); !errors.Is(err, errSecondaryRetryUnavailable) {
		t.Fatalf("missing source retry = %v", err)
	}
	if err := os.Rename(path+".hidden", path); err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), original...)
	corrupt[len(corrupt)/2] ^= 1
	rewrite := func(data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		at := parseBackupStamp(archive.FileModifiedAt)
		if err := os.Chtimes(path, at, at); err != nil {
			t.Fatal(err)
		}
	}
	rewrite(corrupt)
	if err := automation.retrySecondaryCopy(t.Context(), nil, archive.ID); !errors.Is(err, errSecondaryRetryUnavailable) {
		t.Fatalf("modified source retry = %v", err)
	}
	rewrite(original)
	admin := app.authenticate(t)
	request := gin.H{"archive_id": archive.ID}
	if rec := identityRequest(app, http.MethodPost, backupSecondaryRetryRoute, admin, "", request); rec.Code != http.StatusAccepted {
		t.Fatalf("verified retry = %d %s", rec.Code, rec.Body.String())
	}
	if rec := identityRequest(app, http.MethodPost, backupSecondaryRetryRoute, admin, "", request); rec.Code != http.StatusConflict {
		t.Fatalf("duplicate retry = %d %s", rec.Code, rec.Body.String())
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	state = backupAutomationStateForTest(t, restarted)
	copy := state.Secondary.Copies[0]
	if len(state.Secondary.Copies) != 1 || !copy.pending() || !copy.Retried ||
		copy.Attempts != backupSecondaryMaxAttempts || !restarted.secondaryProtectsSource(state.Secondary, archive.ID) {
		t.Fatalf("admitted retry did not preserve its budget or pin: %+v", state.Secondary)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	copy = backupAutomationStateForTest(t, restarted).Secondary.Copies[0]
	if copy.VerifiedAt == "" || copy.Attempts != backupSecondaryMaxAttempts+1 {
		t.Fatalf("retried publication = %+v", copy)
	}
}
