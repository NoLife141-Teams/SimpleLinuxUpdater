package main

import (
	"context"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
)

func TestPrimaryBackupBlockedWriteCanBeAbandoned(t *testing.T) {
	for _, interruption := range []string{"cancel", "restore"} {
		t.Run(interruption, func(t *testing.T) {
			app, automation, _ := newBackupAutomationTest(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			config := backupAutomationStateForTest(t, automation).Config
			config.Retain = 1
			if _, err := automation.Configure(ctx, config); err != nil {
				t.Fatal(err)
			}
			if err := automation.Tick(ctx, true); err != nil {
				t.Fatal(err)
			}
			registered := backupAutomationStateForTest(t, automation).Archives[0]
			writing, release := make(chan struct{}), make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			automation.copyArchive = func(dst io.Writer, src io.Reader) (int64, error) {
				close(writing)
				<-release
				return io.Copy(dst, src)
			}
			result := make(chan error, 1)
			go func() { result <- automation.Tick(ctx, true) }()
			select {
			case <-writing:
			case <-time.After(5 * time.Second):
				t.Fatal("backup did not reach the destination write")
			}
			interactive, decision := app.Deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
			if !decision.Allowed {
				t.Fatal("blocked write retained interactive admission")
			}
			interactive.Close()
			if err := automation.observeArchiveAvailabilityWithScan(ctx, func(ctx context.Context, _ []ownedBackupArchive) backupArchiveScan {
				return backupArchiveScan{observation: newBackupArchiveObservation(automation.now(), "unavailable", "", "")}
			}); err != nil {
				t.Fatalf("observer could not record the storage outage: %v", err)
			}
			status, err := automation.Status(ctx)
			if err != nil || status.State != "running" {
				t.Fatalf("status during the write = %q, %v", status.State, err)
			}
			restore, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
			if !decision.Allowed {
				t.Fatal("blocked write retained exclusive admission")
			}
			if interruption == "restore" {
				if err := automation.SuspendAfterRestore(t.Context()); err != nil {
					restore.Close()
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			restore.Close()
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("interrupted write returned success")
				}
			case <-time.After(4 * time.Second):
				t.Fatal("caller waited for blocked storage after interruption")
			}
			state := backupAutomationStateForTest(t, automation)
			wantState := "failed"
			if interruption == "restore" {
				wantState = "suspended"
			}
			if state.Outcome != wantState || state.PrimaryAttempt != "" {
				t.Fatalf("durable state after interruption = %q", state.Outcome)
			}
			if err := automation.Tick(t.Context(), true); !errors.Is(err, errPrimaryBackupBusy) {
				t.Fatalf("another control run attempted the blocked storage: %v", err)
			}
			if err := automation.Tick(t.Context(), false); err != nil {
				t.Fatalf("scheduled tick failed while the storage slot was reserved: %v", err)
			}

			close(release)
			released = true
			waitForPrimaryBackupWorker(t, automation)
			after := backupAutomationStateForTest(t, automation)
			if after.Outcome != wantState || len(after.Archives) != len(state.Archives) ||
				after.LastSuccessAt != state.LastSuccessAt {
				t.Fatalf("late write changed durable backup outcome or archives: %+v", after)
			}
			entries, err := os.ReadDir(automation.deployment.Directory)
			if err != nil || len(entries) != 1 || entries[0].Name() != registered.Name {
				t.Fatalf("late write changed existing backup files: %v, %v", entries, err)
			}
		})
	}
}

func waitForPrimaryBackupWorker(t *testing.T, automation *backupAutomation) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for automation.primaryBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if automation.primaryBusy.Load() {
		t.Fatal("primary backup worker did not finish")
	}
}

func TestPrimaryBackupInterruptedDuringStoragePreparationIsNotReplayed(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	before := backupAutomationStateForTest(t, automation)
	*now = parseBackupStamp(before.NextAt)
	if claim, err := automation.reservePrimaryBackup(t.Context(), false); err != nil || claim.state.PrimaryAttempt == "" {
		t.Fatalf("reserve due occurrence: %v", err)
	}

	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	if err := restarted.Tick(t.Context(), false); err != nil {
		t.Fatal(err)
	}
	after := backupAutomationStateForTest(t, restarted)
	if after.Outcome != "failed" || after.PrimaryAttempt != "" || len(after.Archives) != 0 ||
		!parseBackupStamp(after.NextAt).After(*now) {
		t.Fatalf("interrupted storage preparation replayed the occurrence: %+v", after)
	}
}
