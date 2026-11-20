package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
)

func TestBackupArchiveIntegrityDetectsSameSizeAndMTimeCorruption(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, a)
	older, latest := state.Archives[0], state.Archives[1]
	name := filepath.Join(a.deployment.Directory, latest.Name)
	info, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	payload[len(payload)/2] ^= 1
	if err := os.WriteFile(name, payload, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(name, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(a.deployment.SecretFile); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(backupIntegrityOtherAfter)
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	state = backupAutomationStateForTest(t, a)
	if state.Archives[1].Availability != "present" {
		t.Fatal("metadata observation should still see the unchanged registration")
	}
	if err := a.checkArchiveIntegrity(t.Context()); err != nil {
		t.Fatal(err)
	}
	state = backupAutomationStateForTest(t, a)
	damaged := state.Archives[1]
	if damaged.Integrity.State != backupIntegrityMismatch || damaged.Integrity.CheckedAt != backupStamp(*now) ||
		damaged.Availability != "corrupt" || !damaged.LossActive || damaged.CapturedAt != latest.CapturedAt ||
		damaged.LastVerifiedAt != latest.LastVerifiedAt || state.Archives[0].Integrity != older.Integrity {
		t.Fatalf("newest archive should be checked first with separate evidence: %+v", damaged)
	}
	verifyCoverage := func() {
		t.Helper()
		facts := installationSnapshot(t.Context(), app.Deps)
		if facts.BackupArchives.Lost != 1 || facts.BackupArchives.Verified != 1 ||
			facts.BackupArchives.RecoveryPoint.CapturedAt != older.CapturedAt {
			t.Fatalf("coverage did not fall back to the intact point: %+v", facts.BackupArchives)
		}
		metrics := installationMetrics(facts)
		if strings.Contains(metrics, damaged.ID) || strings.Contains(metrics, name) {
			t.Fatal("integrity failure exposed archive identity to metrics")
		}
	}
	verifyCoverage()
	if err := os.WriteFile(a.deployment.SecretFile, []byte("a separate automated backup secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	catalog := catalogFacts(t, app, app.authenticate(t))
	if catalog.Archives[0].Availability != "corrupt" || catalog.Archives[0].Integrity != damaged.Integrity {
		t.Fatalf("catalog disagrees with coverage: %+v", catalog.Archives[0])
	}
	// Simulate a former process's persisted observation. The corruption
	// evidence remains durable when the next process reobserves the mount.
	state.Observation.Process = "former-fixture-process"
	if err := a.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(backupIntegrityInterval)
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := a.checkArchiveIntegrity(t.Context()); err != nil {
		t.Fatal(err)
	}
	after := backupAutomationStateForTest(t, a)
	if after.Archives[0].Integrity.State != backupIntegrityMatched || after.Archives[0].Integrity.CheckedAt != backupStamp(*now) ||
		after.Archives[0].LastVerifiedAt != older.LastVerifiedAt || after.Archives[0].CapturedAt != older.CapturedAt ||
		after.Archives[1].Integrity != damaged.Integrity {
		t.Fatalf("older archive check or durable corruption evidence: %+v", after.Archives)
	}
	verifyCoverage()
	var failures int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("corruption should open one loss episode: %d, %v", failures, err)
	}
}

func TestBackupArchiveIntegrityUnavailableAndSupersededChecksDoNotConfirmBytes(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	original := backupAutomationStateForTest(t, a).Archives[0]
	name := filepath.Join(a.deployment.Directory, original.Name)
	*now = now.Add(backupIntegrityNewestAfter)
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(name, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0600) })
	if err := a.checkArchiveIntegrity(t.Context()); err != nil {
		t.Fatal(err)
	}
	current := backupAutomationStateForTest(t, a).Archives[0]
	if current.Integrity.State != backupIntegrityUnavailable || current.Integrity.CheckedAt != original.Integrity.CheckedAt ||
		current.Integrity.AttemptedAt != backupStamp(*now) || current.LossActive {
		t.Fatalf("unavailable read must not create proof or loss: %+v", current)
	}
	if facts := installationSnapshot(t.Context(), app.Deps).BackupArchives; facts.Unknown != 1 || facts.Lost != 0 {
		t.Fatalf("interrupted read health = %+v", facts)
	}
	if err := os.Chmod(name, 0600); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(backupIntegrityRetryAfter)
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	err := a.checkArchiveIntegrityWithScan(t.Context(), func(ctx context.Context, archive ownedBackupArchive) string {
		lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
		if !decision.Allowed {
			t.Error("an integrity read must let exclusive maintenance start")
		} else {
			lease.Close()
		}
		// A foreground verification can complete while a background read is
		// outstanding. Preserve its result against a stale mismatch.
		a.mu.Lock()
		defer a.mu.Unlock()
		state := backupAutomationStateForTest(t, a)
		state.Archives[0].LastVerifiedAt = backupStamp(*now)
		state.Archives[0].Integrity = confirmedBackupIntegrity(backupStamp(*now))
		if err := a.save(ctx, state); err != nil {
			t.Error(err)
		}
		return backupIntegrityMismatch
	})
	if !errors.Is(err, errBackupArchiveObservationDeferred) {
		t.Fatalf("superseded check must be discarded: %v", err)
	}
	after := backupAutomationStateForTest(t, a).Archives[0]
	if after.Integrity.State != backupIntegrityMatched || after.LossActive {
		t.Fatalf("stale result changed new evidence: %+v", after)
	}
}
