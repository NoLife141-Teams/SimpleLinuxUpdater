package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	notificationpkg "debian-updater/internal/notifications"
)

func TestBackupArchiveObservationsDistinguishLossFromUnavailableStorage(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	archives := backupAutomationStateForTest(t, a).Archives
	if len(archives) != 2 {
		t.Fatalf("published %d archives", len(archives))
	}
	older, latest := archives[0], archives[1]
	directory := a.deployment.Directory
	latestPath := filepath.Join(directory, latest.Name)
	body, err := os.ReadFile(latestPath)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)/2] ^= 1
	if err := os.WriteFile(latestPath, body, 0600); err != nil {
		t.Fatal(err)
	}
	changedAt := now.Add(30 * time.Minute)
	if err := os.Chtimes(latestPath, changedAt, changedAt); err != nil {
		t.Fatal(err)
	}
	check := func(state, storage string, verified, lost int) {
		t.Helper()
		if err := a.observeArchiveAvailability(t.Context()); err != nil {
			t.Fatal(err)
		}
		snapshot := installationSnapshot(t.Context(), app.Deps)
		facts := snapshot.BackupArchives
		if facts.State != state || facts.Storage != storage || facts.Verified != verified || facts.Lost != lost {
			t.Fatalf("installation archive facts = %+v", facts)
		}
		body := installationMetrics(snapshot)
		if strings.Contains(body, directory) || strings.Contains(body, latest.ID) || strings.Contains(body, latest.Owner) {
			t.Fatal("archive facts disclosed storage identity")
		}
	}
	check("covered", "available", 1, 1)
	snapshot := installationSnapshot(t.Context(), app.Deps)
	if snapshot.BackupArchives.LastVerifiedAt != older.LastVerifiedAt ||
		!strings.Contains(installationMetrics(snapshot), "slu_installation_backup_local_archive_coverage 1") {
		t.Fatal("older verified archive did not provide local coverage")
	}
	if err := os.Chmod(directory, 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(directory, 0700) })
	check("unknown", "unavailable", 0, 0)
	cookie := app.authenticate(t)
	for _, item := range catalogFacts(t, app, cookie).Archives {
		if item.Availability != "unavailable" || item.ReverifyPossible {
			t.Fatalf("catalog reported an archive as accessible on unavailable storage: %+v", item)
		}
	}
	if err := os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory, directory+".unmounted"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory + ".unmounted") })
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	check("unknown", "unavailable", 0, 0)
	var failures int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&failures); err != nil || failures != 1 {
		t.Fatalf("mount outage was recorded as archive loss: %d %v", failures, err)
	}
	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(directory+".unmounted", directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(directory, older.Name)); err != nil {
		t.Fatal(err)
	}
	check("missing", "available", 0, 2)
}

func TestBackupArchiveObservationKeepsStalledStorageUnavailable(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if facts := installationSnapshot(t.Context(), app.Deps).BackupArchives; facts.State != "covered" {
		t.Fatalf("expected initial coverage: %+v", facts)
	}

	release := make(chan struct{})
	defer close(release)
	var scans atomic.Int32
	stall := func(context.Context, []ownedBackupArchive) backupArchiveScan {
		scans.Add(1)
		<-release
		return backupArchiveScan{}
	}
	for _, advance := range []time.Duration{backupArchiveObservationInterval, 3 * backupArchiveObservationInterval} {
		*now = now.Add(advance)
		if err := a.observeArchiveAvailabilityWithScan(t.Context(), stall); err != nil {
			t.Fatal(err)
		}
		facts := installationSnapshot(t.Context(), app.Deps).BackupArchives
		if facts.Storage != "unavailable" || facts.State != "unknown" || facts.CheckedAt != backupStamp(*now) {
			t.Fatalf("stalled storage observation = %+v", facts)
		}
	}
	if calls := scans.Load(); calls != 1 {
		t.Fatalf("stalled storage opened %d filesystem scans", calls)
	}
	var losses int
	if err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&losses); err != nil || losses != 0 {
		t.Fatalf("stalled storage opened loss episodes: %d, %v", losses, err)
	}
}

type archiveObservationDeliveryClient struct {
	calls atomic.Int32
}

func (c *archiveObservationDeliveryClient) Do(*http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestBackupArchiveLossNotifiesOnceAcrossWorkerRestartAndReverification(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	path := filepath.Join(a.deployment.Directory, archive.Name)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	client := &archiveObservationDeliveryClient{}
	deps := NotificationServiceDeps{
		DB: app.Deps.DB, Now: a.deps.Now, HTTPClient: client, ConditionPollInterval: 10 * time.Millisecond,
		ReadConditions: newNotificationConditionSource(app.Deps.DB, a.deps.Now, nil),
	}
	var runs []*NotificationService
	startWorker := func() *NotificationService {
		service := NewNotificationService(deps)
		runs = append(runs, service)
		service.StartConditionMonitoring()
		return service
	}
	t.Cleanup(func() {
		for _, service := range runs {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := service.Close(ctx)
			cancel()
			if err != nil {
				t.Error(err)
			}
		}
	})
	first := NewNotificationService(deps)
	runs = append(runs, first)
	if _, err := first.SaveSettings(NotificationSettingsUpdate{
		Enabled: true, WebhookURL: "https://notifications.example.test/backups",
		WebhookURLIntent: notificationpkg.WebhookURLReplace,
		EventTypes:       []string{notificationpkg.EventBackupVerifyFailed},
	}); err != nil {
		t.Fatal(err)
	}
	first.StartConditionMonitoring()
	// Restart the automation worker to exercise its startup observation even
	// when no export is due and nobody requests the catalog.
	ctx, cancel := context.WithCancel(t.Context())
	a.Start(ctx)
	var observed int
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_ = app.Deps.DB().QueryRow("SELECT count(*) FROM notification_outbox WHERE event_type=? AND state='succeeded'", notificationpkg.EventBackupVerifyFailed).Scan(&observed)
		if observed == 1 {
			break
		}
	}
	cancel()
	a.Wait()
	if observed != 1 || client.calls.Load() != 1 {
		state := backupAutomationStateForTest(t, a)
		t.Fatalf("initial loss receipts = %d, durable = %d, loss active = %v", client.calls.Load(), observed, state.Archives[0].LossActive)
	}
	stopCtx, stop := context.WithTimeout(t.Context(), 2*time.Second)
	if err := first.Close(stopCtx); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.State != "missing" || got.Lost != 1 {
		t.Fatalf("mere presence reset verification: %+v", got)
	}
	modified := a.now().Add(time.Hour)
	if err := os.Chtimes(path, modified, modified); err != nil {
		t.Fatal(err)
	}
	if item := catalogFacts(t, app, cookie).Archives[0]; item.Availability != "changed" || !item.ReverifyPossible {
		t.Fatalf("catalog did not allow reverification after an mtime change: %+v", item)
	}
	if rec := catalogRequest(app, cookie, archive.ID, "verify"); rec.Code != 200 {
		t.Fatalf("reverification = %d %s", rec.Code, rec.Body.String())
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.State != "covered" || got.Lost != 0 {
		t.Fatalf("verification did not restore local coverage: %+v", got)
	}
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	if rec := catalogRequest(app, cookie, archive.ID, "verify"); rec.Code != 200 {
		t.Fatalf("reverification before notification polling = %d %s", rec.Code, rec.Body.String())
	}
	startWorker()
	var second int
	secondCondition := backupArchiveLossConditionPrefix + archive.ID + "/2"
	for deadline := time.Now().Add(4 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		_ = app.Deps.DB().QueryRow(`SELECT count(*) FROM notification_outbox
		  WHERE event_type=? AND state='succeeded' AND json_extract(meta_json,'$.condition_id')=?`,
			notificationpkg.EventBackupVerifyFailed, secondCondition).Scan(&second)
		if second == 1 {
			break
		}
	}
	var audits int
	err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&audits)
	if err != nil || audits != 2 || second != 1 || client.calls.Load() != 2 {
		t.Fatalf("loss episodes: audits=%d calls=%d second=%d error=%v", audits, client.calls.Load(), second, err)
	}
}

func TestBackupArchiveLossRemainsObservableAcrossPublication(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	path := filepath.Join(a.deployment.Directory, archive.Name)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Deps.DB().Exec(`CREATE TRIGGER reject_archive_observation BEFORE UPDATE ON settings
		WHEN OLD.key='backup_automation_v1' BEGIN SELECT RAISE(ABORT,'controlled observation write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err == nil {
		t.Fatal("observation unexpectedly committed")
	}
	var audits int
	if err := app.Deps.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'`).Scan(&audits); err != nil || audits != 0 {
		t.Fatalf("failed observation published a loss event: %d, %v", audits, err)
	}
	if _, err := app.Deps.DB().Exec("DROP TRIGGER reject_archive_observation"); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.Verified != 1 || got.Lost != 1 {
		t.Fatalf("publication hid a registered loss: %+v", got)
	}
	var registered bool
	for _, item := range catalogFacts(t, app, cookie).Archives {
		if item.ID == archive.ID && item.Availability == "missing" {
			registered = true
		}
	}
	if !registered {
		t.Fatal("catalog dropped the lost archive on publication")
	}
}

func TestBackupArchiveObserverDoesNotWaitOnPublication(t *testing.T) {
	_, a, _ := newBackupAutomationTest(t)
	verify := a.verify
	reached, release := make(chan struct{}), make(chan struct{})
	a.verify = func(ctx context.Context, file internalbackup.TemporaryFile, pass string) (internalbackup.VerifyResult, error) {
		close(reached)
		<-release
		return verify(ctx, file, pass)
	}
	done := make(chan error, 1)
	go func() { done <- a.Tick(t.Context(), true) }()
	defer func() {
		close(release)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-reached:
	case <-time.After(5 * time.Second):
		t.Fatal("publication did not reach verification")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := a.observeArchiveAvailability(ctx); !errors.Is(err, errBackupArchiveObservationDeferred) || ctx.Err() != nil {
		t.Fatalf("observer waited on publication: %v %v", err, ctx.Err())
	}
	state, err := a.readState(t.Context())
	if err != nil || state.Observation.CheckedAt != "" {
		t.Fatalf("observer persisted stale results during publication: %+v, %v", state.Observation, err)
	}
}
