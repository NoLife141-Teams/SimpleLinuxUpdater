package main

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	notificationpkg "debian-updater/internal/notifications"
)

func TestSecondaryBackupCoverageCondition(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	binding := backupSecondaryBinding{Path: backupStorageBinding("/mounted"), Directory: "secondary", Owner: "installation"}
	state := backupSecondaryState{Approved: true, Binding: binding}
	state.Observation.record(now, true)
	coverage := func(at time.Time, previous ...notificationpkg.Condition) notificationpkg.ConditionFact {
		return secondaryBackupConditions(binding.Path, state, at, previous, nil)[1]
	}
	if coverage(now).Active {
		t.Fatal("first observation opened a missing coverage alert before the grace period")
	}
	now = now.Add(backupSecondaryCoverageGrace)
	state.Observation.record(now, true)
	if !coverage(now).Active {
		t.Fatal("missing recovery point did not open a coverage alert after the grace period")
	}

	state.Copies = []backupSecondaryCopy{
		{Archive: ownedBackupArchive{CapturedAt: backupStamp(now.Add(-8 * 24 * time.Hour))}, VerifiedAt: backupStamp(now), Integrity: confirmedBackupIntegrity(backupStamp(now))},
		{Archive: ownedBackupArchive{CapturedAt: backupStamp(now.Add(-48 * time.Hour))}, VerifiedAt: backupStamp(now), Integrity: confirmedBackupIntegrity(backupStamp(now))},
		{Archive: ownedBackupArchive{CapturedAt: backupStamp(now.Add(-24 * time.Hour))}, VerifiedAt: backupStamp(now), Unavailable: true},
	}
	if coverage(now).Active {
		t.Fatal("a recent fallback copy should maintain coverage")
	}
	state.Copies[1].Integrity.State = backupIntegrityMismatch
	stale := coverage(now)
	if !stale.Active {
		t.Fatal("a new digest check on an old archive did not leave coverage stale")
	}

	previous := notificationpkg.Condition{Fact: stale, Episode: 1}
	state.Observation.Process = "previous process"
	if got := coverage(now, previous); !got.Active || !got.Suspended || got.Generation != stale.Generation {
		t.Fatalf("unknown observation should suspend the previous episode: %+v", got)
	}
	restored := secondaryBackupConditions(binding.Path, backupSecondaryState{}, now, []notificationpkg.Condition{previous}, nil)[1]
	if !restored.Active || !restored.Suspended || restored.Generation != stale.Generation {
		t.Fatalf("restoring a catalog before secondary setup should suspend the unresolved coverage episode: %+v", restored)
	}
	state.Binding.Directory = "replacement"
	if got := coverage(now, previous); got.Active {
		t.Fatalf("replacement mount carried over an earlier mount's episode: %+v", got)
	}
	if got := secondaryBackupConditions("", state, now, []notificationpkg.Condition{previous}, nil)[1]; got.Active {
		t.Fatalf("disabled destination kept a coverage alert: %+v", got)
	}
}

func TestSecondaryBackupAlertsWhenWorkerStopsObserving(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		lastResult bool
		alertDelay time.Duration
	}{
		{name: "first probe", alertDelay: 31 * time.Minute},
		{name: "after a successful probe", lastResult: true, alertDelay: 90 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binding := backupSecondaryBinding{Path: backupStorageBinding("/mounted"), Directory: "secondary", Owner: "installation"}
			state := backupSecondaryState{Approved: true, Binding: binding}
			state.Observation.start(start)
			if tc.lastResult {
				state.Observation.record(start, true)
			}
			alert := func(at time.Time) notificationpkg.ConditionFact {
				return secondaryBackupConditions(binding.Path, state, at, nil, nil)[0]
			}
			due := start.Add(tc.alertDelay)
			if got := alert(due.Add(-time.Second)); got.Active {
				t.Fatalf("overdue probe should respect the storage outage grace period: %+v", got)
			}
			outage := alert(due)
			if !outage.Active {
				t.Fatalf("overdue probe should open an outage after the grace period: %+v", outage)
			}
			state.Observation.record(due.Add(time.Minute), false)
			if got := alert(due.Add(time.Minute)); !got.Active || got.Generation != outage.Generation || got.CreatedAt != outage.CreatedAt {
				t.Fatalf("a late failure should continue the same outage: %+v, before: %+v", got, outage)
			}
			state.Observation.record(due.Add(2*time.Minute), true)
			if got := alert(due.Add(2 * time.Minute)); got.Active {
				t.Fatalf("successful observation should resolve the outage: %+v", got)
			}
		})
	}
}

func TestSecondaryBackupRestartsDuringMountOutage(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	binding := backupSecondaryBinding{Path: backupStorageBinding("/mounted"), Directory: "secondary", Owner: "installation"}
	state := backupSecondaryState{Approved: true, Binding: binding}
	state.Observation.record(start, true)
	firstFailure := start.Add(time.Minute)
	state.Observation.record(firstFailure, false)

	// A process restart happens just before the outage becomes alertable.
	state.Observation.Process = "previous process"
	due := firstFailure.Add(30 * time.Minute)
	state.Observation.start(due)
	if got := secondaryBackupConditions(binding.Path, state, due, nil, nil)[0]; got.Active {
		t.Fatalf("outage alert should await the new process's observation: %+v", got)
	}
	state.Observation.record(due, false)
	if got := secondaryBackupConditions(binding.Path, state, due, nil, nil)[0]; !got.Active {
		t.Fatalf("renewed failure should alert without restarting the grace period: %+v", got)
	}
}

func TestSecondaryBackupWriteFailureDoesNotHideMissingCoverage(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("requires filesystem write permissions to be enforced")
	}
	app, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secondary, 0500); err != nil {
		t.Fatal(err)
	}

	firstCheck := *now
	for _, elapsed := range []time.Duration{0, time.Hour} {
		*now = firstCheck.Add(elapsed)
		if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, os.ErrPermission) {
			t.Fatalf("expected write failure on readable secondary mount: %v", err)
		}
	}
	read := newNotificationConditionSource(app.Deps.DB, automation.deps.Now, nil, automation.deployment)
	batch, err := read(t.Context(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	coverage := lastConditionFact(batch.Facts, "backup/secondary/coverage")
	outage := lastConditionFact(batch.Facts, "backup/secondary/storage")
	if !coverage.Active || coverage.Suspended || outage.Active {
		t.Fatalf("expected an actionable coverage alert on the readable mount, coverage=%+v, storage=%+v", coverage, outage)
	}
}

func TestSecondaryBackupOutageNotifiesAcrossRestartAndRecovery(t *testing.T) {
	app, automation, fixtureNow := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	initial := *fixtureNow
	var clock atomic.Int64
	advance := func(at time.Time) { clock.Store(at.UnixNano()) }
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	advance(initial)
	automation.deps.Now = now
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	poll := func(at time.Time) {
		t.Helper()
		advance(at)
		if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	poll(initial)
	poll(initial.Add(time.Minute))
	state := backupAutomationStateForTest(t, automation)
	if len(state.Secondary.Copies) != 1 || state.Secondary.Copies[0].VerifiedAt == "" {
		t.Fatalf("expected one published secondary copy: %+v", state.Secondary)
	}
	archive := state.Secondary.Copies[0].Archive
	client := &archiveObservationDeliveryClient{archiveID: archive.ID, archiveDirectory: secondary}
	deps := NotificationServiceDeps{
		DB: app.Deps.DB, Now: now, HTTPClient: client, ConditionPollInterval: 10 * time.Millisecond,
		ReadConditions: newNotificationConditionSource(app.Deps.DB, now, nil, automation.deployment),
	}
	const event = notificationpkg.EventBackupSecondaryUnavailable
	closeWorker := func(worker *NotificationService) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := worker.Close(ctx); err != nil {
			t.Error(err)
		}
	}
	worker := NewNotificationService(deps)
	t.Cleanup(func() { closeWorker(worker) })
	if _, err := worker.SaveSettings(NotificationSettingsUpdate{
		Enabled: true, WebhookURL: "https://notifications.example.test/backups",
		WebhookURLIntent: notificationpkg.WebhookURLReplace, EventTypes: []string{event},
	}); err != nil {
		t.Fatal(err)
	}

	offline := secondary + ".offline"
	t.Cleanup(func() { _ = os.RemoveAll(offline) })
	move := func(from, to string) {
		t.Helper()
		if err := os.Rename(from, to); err != nil {
			t.Fatal(err)
		}
	}
	outageAt := initial.Add(2 * time.Minute)
	move(secondary, offline)
	poll(outageAt)
	poll(outageAt.Add(backupStorageAlertAfter - time.Second))
	batch, err := deps.ReadConditions(t.Context(), "", nil)
	if err != nil || lastConditionFact(batch.Facts, "backup/secondary/storage").Active {
		t.Fatalf("short outage opened an alert: %v, %+v", err, batch)
	}
	move(offline, secondary)
	poll(outageAt.Add(backupStorageAlertAfter))

	outageAt = now().Add(time.Minute)
	move(secondary, offline)
	poll(outageAt)
	poll(outageAt.Add(backupStorageAlertAfter))
	if err := automation.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	worker.StartConditionMonitoring()
	waitFor := func(deliveries int, active bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) {
			var sent int
			var storedActive bool
			if err := app.Deps.DB().QueryRow(`SELECT count(*) FROM notification_outbox WHERE event_type=? AND state='succeeded'`, event).Scan(&sent); err == nil && sent == deliveries {
				err := app.Deps.DB().QueryRow(`SELECT json_extract(state_json,'$.Conditions."backup/secondary/storage".Fact.Active') FROM notification_conditions WHERE id=1`).Scan(&storedActive)
				if err == nil && storedActive == active {
					return
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("expected %d completed deliveries and condition active=%v", deliveries, active)
	}
	waitFor(1, true)
	if got := backupAutomationStateForTest(t, automation).archiveCoverage(now()); got.RecoveryPoint.State != "recent" {
		t.Fatalf("local coverage should remain recent during the secondary outage: %+v", got)
	}
	closeWorker(worker)

	restarted := NewNotificationService(deps)
	t.Cleanup(func() { closeWorker(restarted) })
	restarted.StartConditionMonitoring()
	deadline := time.Now().Add(4 * time.Second)
	for {
		_, health, err := restarted.InstallationHealth(t.Context())
		if err == nil && health.LastConditionEvaluationAt != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted worker did not evaluate the ongoing outage: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(1, true)
	move(offline, secondary)
	poll(now().Add(time.Minute))
	waitFor(1, false)

	outageAt = now().Add(time.Minute)
	move(secondary, offline)
	poll(outageAt)
	poll(outageAt.Add(backupStorageAlertAfter))
	waitFor(2, true)
	closeWorker(restarted)
	if client.calls.Load() != 2 || client.disclosedArchive.Load() {
		t.Fatalf("outage webhook calls=%d, exposed archive details=%v", client.calls.Load(), client.disclosedArchive.Load())
	}
}
