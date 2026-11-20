package main

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

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
