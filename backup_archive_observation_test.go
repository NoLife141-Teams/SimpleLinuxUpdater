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
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	empty := installationSnapshot(t.Context(), app.Deps)
	if empty.BackupArchives.State != "no_registered" ||
		!strings.Contains(installationMetrics(empty), "slu_installation_backup_local_archive_coverage 0\n") {
		t.Fatalf("observed empty storage did not report zero coverage: %+v", empty.BackupArchives)
	}
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
	cookie := app.authenticate(t)
	if err := os.Chmod(latestPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.State != "covered" || got.Verified != 1 || got.Lost != 0 || got.Unknown != 1 {
		t.Fatalf("unreadable archive counted toward local coverage: %+v", got)
	}
	if item := catalogFacts(t, app, cookie).Archives[0]; item.Availability != "unavailable" || item.ReverifyPossible {
		t.Fatalf("catalog reported unreadable archive as accessible: %+v", item)
	}
	if err := os.Chmod(latestPath, 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.Verified != 2 || got.LastVerifiedAt != latest.LastVerifiedAt {
		t.Fatalf("permission recovery did not preserve verification evidence: %+v", got)
	}
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

func TestBackupRecoveryPointFallsBackToCaptureTimeAfterNewestArchiveLoss(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	initial := *now
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, a)
	if len(state.Archives) != 1 || state.Archives[0].CapturedAt != backupStamp(initial) {
		t.Fatalf("verified publication did not preserve capture time: %+v", state.Archives)
	}
	older := state.Archives[0]
	// Simulate an older registration that did not retain the verified stamp.
	state.Archives[0].CapturedAt = ""
	if err := a.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := installationSnapshot(t.Context(), app.Deps).BackupArchives; got.Verified != 1 ||
		got.RecoveryPoint.State != "unknown" || got.RecoveryPoint.AgeSeconds != nil {
		t.Fatalf("availability supplied an undocumented capture date: %+v", got)
	}
	*now = initial.Add(8 * 24 * time.Hour)
	cookie := app.authenticate(t)
	if rec := catalogRequest(app, cookie, older.ID, "verify"); rec.Code != 200 {
		t.Fatalf("reverification = %d %s", rec.Code, rec.Body.String())
	}
	check := func(wantState, wantCaptured string, wantAge float64) {
		t.Helper()
		if err := a.observeArchiveAvailability(t.Context()); err != nil {
			t.Fatal(err)
		}
		snapshot := installationSnapshot(t.Context(), app.Deps)
		point := snapshot.BackupArchives.RecoveryPoint
		if point.State != wantState || point.CapturedAt != wantCaptured ||
			(wantCaptured == "" && point.AgeSeconds != nil) ||
			(wantCaptured != "" && (point.AgeSeconds == nil || *point.AgeSeconds != wantAge)) {
			t.Fatalf("point = %+v, want state %q capture %q age %v", point, wantState, wantCaptured, wantAge)
		}
		metrics := installationMetrics(snapshot)
		if wantCaptured != "" && !strings.Contains(metrics, "\nslu_installation_backup_local_recovery_point_age_seconds ") {
			t.Fatal("confirmed point has no age metric")
		}
		if wantState == "unknown" && strings.Contains(metrics, "\nslu_installation_backup_local_recovery_point_available ") {
			t.Fatal("unobservable point exported a false availability sample")
		}
	}
	oldAge := now.Sub(initial).Seconds()
	check("stale", backupStamp(initial), oldAge)
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	check("recent", backupStamp(*now), 0)
	latest := backupAutomationStateForTest(t, a).Archives[1]
	if err := os.Remove(filepath.Join(a.deployment.Directory, latest.Name)); err != nil {
		t.Fatal(err)
	}
	check("stale", backupStamp(initial), oldAge)
	if err := os.Remove(filepath.Join(a.deployment.Directory, older.Name)); err != nil {
		t.Fatal(err)
	}
	check("missing", "", 0)
	dir := a.deployment.Directory
	if err := os.Rename(dir, dir+"-unmounted"); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Rename(dir+"-unmounted", dir); err != nil {
			t.Error(err)
		}
	}()
	check("unknown", "", 0)
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
	calls            atomic.Int32
	disclosedArchive atomic.Bool
	archiveID        string
	archiveDirectory string
}

func (c *archiveObservationDeliveryClient) Do(req *http.Request) (*http.Response, error) {
	defer req.Body.Close()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, err
	}
	if payload := string(body); strings.Contains(payload, c.archiveID) || strings.Contains(payload, c.archiveDirectory) {
		c.disclosedArchive.Store(true)
	}
	c.calls.Add(1)
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

func TestBackupArchiveLossNotifiesOnceAcrossWorkerRestartAndReverification(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	path := filepath.Join(a.deployment.Directory, archive.Name)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	client := &archiveObservationDeliveryClient{archiveID: archive.ID, archiveDirectory: a.deployment.Directory}
	deps := NotificationServiceDeps{
		DB: app.Deps.DB, Now: a.deps.Now, HTTPClient: client, ConditionPollInterval: 10 * time.Millisecond,
		ReadConditions: newNotificationConditionSource(app.Deps.DB, a.deps.Now, nil, ""),
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
		EventTypes:       []string{notificationpkg.EventBackupVerifyFailed, notificationpkg.EventBackupCoverageMissing},
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
		_ = app.Deps.DB().QueryRow("SELECT count(*) FROM notification_outbox WHERE event_type IN (?,?) AND state='succeeded'",
			notificationpkg.EventBackupVerifyFailed, notificationpkg.EventBackupCoverageMissing).Scan(&observed)
		if observed == 2 {
			break
		}
	}
	cancel()
	a.Wait()
	if observed != 2 || client.calls.Load() != 2 {
		state := backupAutomationStateForTest(t, a)
		t.Fatalf("initial loss receipts = %d, durable = %d, loss active = %v", client.calls.Load(), observed, state.Archives[0].LossActive)
	}
	stopCtx, stop := context.WithTimeout(t.Context(), 2*time.Second)
	if err := first.Close(stopCtx); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	restarted := startWorker()
	for deadline := time.Now().Add(4 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		worker, facts, err := restarted.InstallationHealth(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if !worker.LastProgress.IsZero() && facts.Pending == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restarted notification worker did not evaluate the existing loss")
		}
	}
	stopCtx, stop = context.WithTimeout(t.Context(), 2*time.Second)
	if err := restarted.Close(stopCtx); err != nil {
		stop()
		t.Fatal(err)
	}
	stop()
	if client.calls.Load() != 2 {
		t.Fatalf("restart repeated a delivery for the confirmed recovery point loss: %d", client.calls.Load())
	}
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
	secondCondition := notificationpkg.BackupArchiveLossConditionPrefix + archive.ID + "/2"
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
	if err != nil || audits != 2 || second != 1 || client.calls.Load() != 3 {
		t.Fatalf("loss episodes: audits=%d calls=%d second=%d error=%v", audits, client.calls.Load(), second, err)
	}
	if client.disclosedArchive.Load() {
		t.Fatal("an outbound backup alert disclosed the archive identifier or path")
	}
}

func TestBackupArchiveObservationPreservesLegacyLossWhenFileReappears(t *testing.T) {
	app, a, _, archive := publishedCatalogFixture(t)
	state := backupAutomationStateForTest(t, a)
	legacy := &state.Archives[0]
	legacy.Availability, legacy.Verification = "missing", "failed"
	legacy.DirectoryIdentity, legacy.FileModifiedAt, legacy.ObservedAt = "", "", ""
	if err := a.catalogAudit("backup-scheduler", "", "backup.verify", *legacy, false); err != nil {
		t.Fatal(err)
	}
	if err := a.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}

	// The previously missing file is back when the upgraded worker first runs.
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.deployment.Directory, archive.Name)); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if facts := installationSnapshot(t.Context(), app.Deps).BackupArchives; facts.State != "missing" || facts.Lost != 1 {
		t.Fatalf("loss awaiting verification was not reported: %+v", facts)
	}
	var audits int
	err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&audits)
	if err != nil || audits != 1 {
		t.Fatalf("one unresolved legacy loss produced %d failure audits: %v", audits, err)
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

func TestBackupArchiveReverificationCannotRestoreCoverageAfterConcurrentRemoval(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	path := filepath.Join(a.deployment.Directory, archive.Name)
	saved := path + ".saved"
	if err := os.Rename(path, saved); err != nil {
		t.Fatal(err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	verify := a.verify
	a.verify = func(ctx context.Context, file internalbackup.TemporaryFile, pass string) (internalbackup.VerifyResult, error) {
		result, err := verify(ctx, file, pass)
		if err != nil || !result.RestoreReady {
			t.Fatalf("expected restore-ready snapshot: %+v, %v", result, err)
		}
		if err := os.Rename(path, saved); err != nil {
			t.Fatal(err)
		}
		return result, nil
	}
	if rec := catalogRequest(app, cookie, archive.ID, "verify"); rec.Code != http.StatusConflict {
		t.Fatalf("reverification = %d %s", rec.Code, rec.Body.String())
	}
	if facts := installationSnapshot(t.Context(), app.Deps).BackupArchives; facts.State != "missing" || facts.Lost != 1 {
		t.Fatalf("pinned snapshot restored coverage for a missing archive: %+v", facts)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	var audits int
	err := app.Deps.DB().QueryRow("SELECT count(*) FROM audit_events WHERE action='backup.verify' AND status='failure'").Scan(&audits)
	if err != nil || audits != 1 {
		t.Fatalf("an unresolved loss produced %d failure audits: %v", audits, err)
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

func TestBackupStorageOutageAlertsOnceAcrossRestartAndRecovers(t *testing.T) {
	app, a, _, archive := publishedCatalogFixture(t)
	initial := a.now()
	directory := a.deployment.Directory
	binding := backupStorageBinding(directory)
	var clock atomic.Int64
	clock.Store(initial.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	a.deps.Now = now
	observeAt := func(at time.Time) backupArchiveObservation {
		t.Helper()
		clock.Store(at.UnixNano())
		if err := a.observeArchiveAvailability(t.Context()); err != nil {
			t.Fatal(err)
		}
		return backupAutomationStateForTest(t, a).Observation
	}
	first := observeAt(initial)
	unmounted := directory + ".unmounted"
	if err := os.Rename(directory, unmounted); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(unmounted) })
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	outageAt := initial.Add(time.Minute)
	outage := observeAt(outageAt)
	if first.Storage != "available" || outage.Storage != "unavailable" {
		t.Fatalf("fixture mount status = %s then %s", first.Storage, outage.Storage)
	}
	source := newNotificationConditionSource(app.Deps.DB, now, nil, directory)
	checkFact := func(source notificationpkg.ConditionSource, active bool) notificationpkg.ConditionFact {
		t.Helper()
		batch, err := source(t.Context(), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		fact := lastConditionFact(batch.Facts, backupStorageConditionID)
		if fact.Active != active || lastConditionFact(batch.Facts, "backup/coverage/missing").Active {
			t.Fatalf("unexpected outage or archive-loss fact: %+v", batch.Facts)
		}
		return fact
	}
	checkFact(source, false)
	outage = observeAt(outageAt.Add(backupStorageAlertAfter))
	want := checkFact(source, true)
	if health := outage.storageHealth(binding, now()); health.LastAvailableAt != backupStamp(initial) ||
		health.UnavailableAt != backupStamp(outageAt) || !health.AlertDue || health.State != "unavailable" {
		t.Fatalf("unexpected storage history: %+v", health)
	}
	checkFact(newNotificationConditionSource(app.Deps.DB, now, nil, ""), false)
	checkFact(newNotificationConditionSource(app.Deps.DB, now, nil, t.TempDir()), false)
	if state := backupAutomationStateForTest(t, a); state.Archives[0].LossActive ||
		state.Archives[0].Verification != "verified" || state.archiveCoverage(now()).RecoveryPoint.State != "unknown" {
		t.Fatalf("mount outage altered archive verification: %+v", state.Archives)
	}

	client := &archiveObservationDeliveryClient{archiveID: archive.ID, archiveDirectory: directory}
	deps := NotificationServiceDeps{
		DB: app.Deps.DB, Now: now, HTTPClient: client, ConditionPollInterval: 10 * time.Millisecond,
		ReadConditions: source,
	}
	firstWorker := NewNotificationService(deps)
	closeWorker := func(service *NotificationService) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := service.Close(ctx); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { closeWorker(firstWorker) })
	if _, err := firstWorker.SaveSettings(NotificationSettingsUpdate{
		Enabled: true, WebhookURL: "https://notifications.example.test/backups",
		WebhookURLIntent: notificationpkg.WebhookURLReplace,
		EventTypes:       []string{notificationpkg.EventBackupStorageUnavailable},
	}); err != nil {
		t.Fatal(err)
	}
	firstWorker.StartConditionMonitoring()
	waitFor := func(receipts int, active bool) {
		t.Helper()
		deadline := time.Now().Add(4 * time.Second)
		var sent int
		var factActive bool
		for time.Now().Before(deadline) {
			err := app.Deps.DB().QueryRow(`SELECT count(*) FROM notification_outbox WHERE event_type=? AND state='succeeded'`,
				notificationpkg.EventBackupStorageUnavailable).Scan(&sent)
			errState := app.Deps.DB().QueryRow(`SELECT json_extract(state_json,'$.Conditions."backup/storage/unavailable".Fact.Active')
				FROM notification_conditions WHERE id=1`).Scan(&factActive)
			if err == nil && errState == nil && sent == receipts && factActive == active {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("stored deliveries = %d, condition active = %v, want %d and %v", sent, factActive, receipts, active)
	}
	waitFor(1, true)
	closeWorker(firstWorker)
	// Reopen condition evaluation and re-observe the mount after restart.
	deps.ReadConditions = newNotificationConditionSource(app.Deps.DB, now, nil, directory)
	observeAt(now().Add(5 * time.Minute))
	if got := checkFact(deps.ReadConditions, true); got.Generation != want.Generation {
		t.Fatalf("restart changed ongoing outage generation: %q != %q", got.Generation, want.Generation)
	}
	restarted := NewNotificationService(deps)
	t.Cleanup(func() { closeWorker(restarted) })
	restarted.StartConditionMonitoring()
	evaluationDeadline := time.Now().Add(4 * time.Second)
	for {
		_, facts, err := restarted.InstallationHealth(t.Context())
		if err == nil && facts.LastConditionEvaluationAt != nil {
			break
		}
		if !time.Now().Before(evaluationDeadline) {
			t.Fatalf("restarted worker did not evaluate the ongoing outage: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitFor(1, true)

	if err := os.Remove(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(unmounted, directory); err != nil {
		t.Fatal(err)
	}
	recovered := observeAt(now().Add(time.Minute))
	waitFor(1, false)
	if health := recovered.storageHealth(binding, now()); health.State != "available" ||
		health.LastAvailableAt != backupStamp(now()) || health.UnavailableAt != "" || health.AlertDue {
		t.Fatalf("successful mount probe did not close its outage: %+v", health)
	}
	if coverage := backupAutomationStateForTest(t, a).archiveCoverage(now()); coverage.RecoveryPoint.State != "recent" {
		t.Fatalf("coverage did not recover: %+v", coverage)
	}
	closeWorker(restarted)
	if client.calls.Load() != 1 || client.disclosedArchive.Load() {
		t.Fatalf("outage deliveries=%d, disclosed archive path or ID=%v", client.calls.Load(), client.disclosedArchive.Load())
	}
}

func TestBackupStorageAlertWaitsForCurrentProcessObservation(t *testing.T) {
	binding := backupStorageBinding("/archives")
	checked := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	now := checked.Add(time.Hour)
	observation := newBackupArchiveObservation(checked, "available", "owner", "directory")
	observation.recordStorageHistory(backupArchiveObservation{}, binding)
	active := backupStorageCondition(binding, observation, now, nil, nil)[0]
	if !active.Active {
		t.Fatal("stale observation in the running process did not open its outage")
	}

	observation.Process = "previous-process"
	if fact := backupStorageCondition(binding, observation, now, nil, nil)[0]; fact.Active {
		t.Fatal("restart opened an outage from the prior process's observation alone")
	}
	if health := observation.storageHealth(binding, now); health.State != "not_observed" ||
		health.LastAvailableAt != backupStamp(checked) || health.AlertDue {
		t.Fatalf("startup projection lost history or confirmed current availability: %+v", health)
	}
	previous := []notificationpkg.Condition{{Fact: active, Episode: 1}}
	if fact := backupStorageCondition(binding, observation, now, previous, nil)[0]; !fact.Active ||
		!fact.Suspended || fact.Generation != active.Generation {
		t.Fatalf("restart retired the earlier episode before probing: %+v", fact)
	}

	failed := newBackupArchiveObservation(now, "unavailable", "owner", "")
	failed.recordStorageHistory(observation, binding)
	if fact := backupStorageCondition(binding, failed, now, previous, nil)[0]; !fact.Active ||
		fact.Suspended || fact.Generation != active.Generation {
		t.Fatalf("first confirmed failure lost the earlier episode: %+v", fact)
	}
}

func TestBackupStorageProbeDiscardsObservationsFromWithdrawnMount(t *testing.T) {
	app, a, _, _ := publishedCatalogFixture(t)
	now := a.now()
	a.deps.Now = func() time.Time { return now }
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	lastAvailable := backupStamp(now)
	directory := a.deployment.Directory
	withdrawn := directory + ".withdrawn"
	t.Cleanup(func() { _ = os.RemoveAll(withdrawn) })
	now = now.Add(time.Hour)
	scan := func(ctx context.Context, archives []ownedBackupArchive) backupArchiveScan {
		result := a.scanBackupArchives(ctx, archives)
		if result.observation.Storage != "available" {
			t.Errorf("fixture did not observe the original mount: %+v", result.observation)
		}
		if err := os.Rename(directory, withdrawn); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Error(err)
		}
		return result
	}
	if err := a.observeArchiveAvailabilityWithScan(t.Context(), scan); err != nil {
		t.Fatal(err)
	}
	deps := app.Deps
	deps.Now = a.deps.Now
	facts := installationSnapshot(t.Context(), deps).BackupArchives
	if facts.StorageHealth.State != "unavailable" || facts.StorageHealth.LastAvailableAt != lastAvailable ||
		facts.RecoveryPoint.State != "unknown" || facts.Lost != 0 {
		t.Fatalf("probe replaced storage evidence with a result from a withdrawn mount: %+v, %+v", facts, facts.StorageHealth)
	}
}

func TestExpiredArchiveObservationContinuesIntoFailedProbe(t *testing.T) {
	start := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	binding := backupStorageBinding("/archives")
	first := newBackupArchiveObservation(start, "available", "owner", "directory")
	first.recordStorageHistory(backupArchiveObservation{}, binding)
	staleAt := start.Add(backupStorageStaleAfter)
	alertAt := staleAt.Add(backupStorageAlertAfter)
	for _, at := range []time.Time{staleAt, alertAt.Add(-time.Second), alertAt} {
		got := first.storageHealth(binding, at)
		if got.State != "stale" || got.UnavailableAt != backupStamp(staleAt) || got.AlertDue != at.Equal(alertAt) {
			t.Fatalf("storage health at %s = %+v", at, got)
		}
	}
	next := newBackupArchiveObservation(alertAt, "unavailable", "", "")
	next.recordStorageHistory(first, binding)
	if got := next.storageHealth(binding, alertAt); got.State != "unavailable" ||
		got.UnavailableAt != backupStamp(staleAt) || !got.AlertDue {
		t.Fatalf("failed probe restarted a previously stale episode: %+v", got)
	}
}
