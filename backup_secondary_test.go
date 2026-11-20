package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
)

func secondaryBackupTestDirectory(t *testing.T, automation *backupAutomation) string {
	t.Helper()
	primary, err := os.Stat(automation.deployment.Directory)
	if err != nil {
		t.Fatal(err)
	}
	system, err := os.Stat(string(filepath.Separator))
	if err != nil {
		t.Fatal(err)
	}
	primaryDevice := backupFilesystemDevice(backupCatalogFileIdentity(primary))
	systemDevice := backupFilesystemDevice(backupCatalogFileIdentity(system))
	for _, parent := range []string{"/dev/shm", "/tmp", "/var/tmp"} {
		info, err := os.Stat(parent)
		if err != nil {
			continue
		}
		device := backupFilesystemDevice(backupCatalogFileIdentity(info))
		if device == "" || device == primaryDevice || device == systemDevice {
			continue
		}
		directory, err := os.MkdirTemp(parent, "slu-secondary-test-")
		if err == nil {
			t.Cleanup(func() { _ = os.RemoveAll(directory) })
			return directory
		}
	}
	t.Skip("secondary backup test requires a writable filesystem separate from app data")
	return ""
}

func TestSecondaryBackupRejectsLocalFallback(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	secondary, err := os.MkdirTemp(filepath.Dir(automation.deployment.Directory), "unmounted-secondary-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(secondary) })
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "mounted separately") {
		t.Fatalf("validation accepted a local fallback on the primary filesystem: %v", err)
	}
	if state := backupAutomationStateForTest(t, automation); state.Secondary.Approved {
		t.Fatalf("local fallback approval was persisted: %+v", state.Secondary)
	}
}

func TestSecondaryValidationRejectsUnconfirmedHardLink(t *testing.T) {
	for _, linked := range []bool{false, true} {
		name := "unsupported hard link"
		if linked {
			name = "link confirmed by the server but reported as failed"
		}
		t.Run(name, func(t *testing.T) {
			_, automation, _ := newBackupAutomationTest(t)
			secondary := secondaryBackupTestDirectory(t, automation)
			automation.deployment.SecondaryDirectory = secondary
			automation.linkSecondaryArchive = func(root *os.Root, source, target string) error {
				if linked {
					if err := root.Link(source, target); err != nil {
						t.Fatal(err)
					}
				}
				return errors.New("secondary storage could not confirm the link")
			}
			if _, err := automation.validateSecondaryDestination(t.Context(), nil); err == nil || !strings.Contains(err.Error(), "hard links") {
				t.Fatalf("validation must refuse unconfirmed publication capability: %v", err)
			}
			if state := backupAutomationStateForTest(t, automation); state.Secondary.Approved {
				t.Fatalf("publication probe failure granted secondary write access: %+v", state.Secondary)
			}
			if entries, err := os.ReadDir(secondary); err != nil || len(entries) != 0 {
				t.Fatalf("validation left probe files on the mount: %v (%v)", entries, err)
			}
		})
	}
}

func TestSecondaryValidationReleasesAdmissionAndDoesNotApproveAfterRestore(t *testing.T) {
	app, automation, _ := newBackupAutomationTest(t)
	automation.deployment.SecondaryDirectory = secondaryBackupTestDirectory(t, automation)
	admin := app.authenticate(t)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	releaseSync := sync.OnceFunc(func() { close(release) })
	defer releaseSync()
	automation.syncSecondaryDirectory = func(*os.Root) error {
		close(entered)
		<-release
		return nil
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		finished <- identityRequest(app, http.MethodPost, backupSecondaryValidateRoute, admin, "", nil)
	}()
	select {
	case <-entered:
	case response := <-finished:
		t.Fatalf("validation did not reach the mounted directory sync: %d %s", response.Code, response.Body)
	case <-ctx.Done():
		t.Fatal("timed out waiting for secondary validation to sync")
	}
	exported := make(chan error, 1)
	go func() { exported <- automation.Tick(ctx, true) }()
	select {
	case err := <-exported:
		state := backupAutomationStateForTest(t, automation)
		if err != nil || state.Outcome != "succeeded" || len(state.Archives) != 1 {
			t.Fatalf("secondary validation blocked primary export: %s, %d archives (%v)", state.Outcome, len(state.Archives), err)
		}
	case <-ctx.Done():
		t.Fatal("primary export waited for secondary validation")
	}
	lease, decision := automation.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("secondary validation held maintenance admission during directory sync")
	}
	runtime := runtimeComposition{deps: AppDeps{BackupAutomation: automation}}
	prepareErr := runtime.PreparePersistenceReplacement(ctx)
	lease.Close()
	if prepareErr != nil {
		t.Fatal(prepareErr)
	}
	releaseSync()
	select {
	case response := <-finished:
		if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "context canceled") {
			t.Fatalf("stale validation survived persistence replacement: %d %s", response.Code, response.Body)
		}
	case <-ctx.Done():
		t.Fatal("stale validation did not finish after persistence replacement")
	}
	if state := backupAutomationStateForTest(t, automation); state.Secondary.Approved {
		t.Fatalf("canceled validation granted secondary write access: %+v", state.Secondary)
	}
}

func TestSecondaryQueueOverflowRemainsVisibleAfterSuccessfulCopy(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	automation.deployment.SecondaryDirectory = secondaryBackupTestDirectory(t, automation)
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, automation)
	for len(state.Secondary.Copies) < backupSecondaryQueueLimit {
		archive := state.Archives[0]
		id, err := newBackupArchiveID()
		if err != nil {
			t.Fatal(err)
		}
		archive.ID = id
		state.Secondary.Copies = append(state.Secondary.Copies, backupSecondaryCopy{Archive: archive, RetryAt: backupStamp(now.Add(time.Hour))})
	}
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state = backupAutomationStateForTest(t, automation)
	if len(state.Secondary.Copies) != backupSecondaryQueueLimit+1 {
		t.Fatalf("queue overflow was not recorded: %d records", len(state.Secondary.Copies))
	}
	skipped := state.Secondary.Copies[backupSecondaryQueueLimit]
	if skipped.Archive.ID != state.Archives[len(state.Archives)-1].ID || skipped.Failure != "queue_full" ||
		automation.secondaryProtectsSource(state.Secondary, skipped.Archive.ID) {
		t.Fatalf("overflow did not record an unpinned failure for the new archive: %+v", skipped)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := automation.Status(t.Context())
	if err != nil || status.State != "succeeded" || status.Secondary == nil || status.Secondary.State != "degraded" ||
		status.Secondary.VerifiedAt == "" || status.Secondary.Failure != "queue_full" || status.Secondary.Failed != 1 ||
		status.Secondary.Pending != backupSecondaryQueueLimit-1 {
		t.Fatalf("successful replication hid an uncovered archive: %+v (%v)", status.Secondary, err)
	}
}

func TestVerifiedSecondaryBackupSurvivesPrimaryLossAndRestoresInIsolation(t *testing.T) {
	app, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	admin := app.authenticate(t)
	if response := identityRequest(app, http.MethodPost, backupSecondaryValidateRoute, admin, "", nil); response.Code != http.StatusOK {
		t.Fatalf("validate secondary destination: %d %s", response.Code, response.Body)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, automation)
	if len(state.Archives) != 1 || len(state.Secondary.Copies) != 1 || !state.Secondary.Copies[0].pending() {
		t.Fatalf("verified publication was not queued: %+v", state.Secondary)
	}
	archive := state.Archives[0]
	original := secondary + ".unmounted"
	if err := os.Rename(secondary, original); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(original) })
	if err := os.Mkdir(secondary, 0700); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(secondary)
	if err != nil || len(entries) != 0 {
		t.Fatalf("replacement mount received writes: %v, %v", entries, err)
	}
	if status, err := automation.Status(t.Context()); err != nil || status.State != "succeeded" ||
		status.Secondary == nil || status.Secondary.Failure != "storage_unavailable" || status.Secondary.VerifiedAt != "" {
		t.Fatalf("mount failure affected primary success or looked verified: %+v (%v)", status, err)
	}
	if err := os.Remove(secondary); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(original, secondary); err != nil {
		t.Fatal(err)
	}

	*now = now.Add(backupSecondaryRetryBase)
	automation.copyArchive = func(dst io.Writer, src io.Reader) (int64, error) {
		n, err := io.Copy(dst, io.LimitReader(src, 32))
		if err == nil {
			err = io.ErrUnexpectedEOF
		}
		return n, err
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("interrupted transfer = %v", err)
	}
	if _, err := os.Stat(filepath.Join(secondary, archive.Name)); !os.IsNotExist(err) {
		t.Fatalf("partial archive became public: %v", err)
	}

	// The new worker loads the interrupted transfer from SQLite and replaces
	// only its recorded temporary file.
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	*now = now.Add(2 * backupSecondaryRetryBase)
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	copyPath := filepath.Join(secondary, archive.Name)
	copied, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatal(err)
	}
	primary, err := os.ReadFile(filepath.Join(automation.deployment.Directory, archive.Name))
	if err != nil || !bytes.Equal(primary, copied) {
		t.Fatalf("copy is not byte-for-byte identical to verified archive: %v", err)
	}
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || status.Secondary.State != "verified" ||
		status.Secondary.CapturedAt != archive.CapturedAt || status.Secondary.Pending != 0 {
		t.Fatalf("copy status = %+v (%v)", status.Secondary, err)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	entries, err = os.ReadDir(secondary)
	if err != nil || len(entries) != 1 || entries[0].Name() != archive.Name {
		t.Fatalf("secondary directory after retry = %v, %v", entries, err)
	}
	detached := automation.deployment.Directory + ".lost"
	if err := os.Rename(automation.deployment.Directory, detached); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(detached) })

	fresh := newIsolatedTestApp(t)
	cookie := fresh.authenticate(t)
	passphrase, err := os.ReadFile(automation.deployment.SecretFile)
	if err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", archive.Name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(copied); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("passphrase", strings.TrimSpace(string(passphrase))); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("isolate", "true"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/restore", &body)
	request.Header.Set("Content-Type", form.FormDataContentType())
	request.AddCookie(cookie)
	markSameOriginAuthRequest(request)
	response := httptest.NewRecorder()
	fresh.Handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `"restore_isolated":true`) {
		t.Fatalf("restore of secondary copy: %d %s", response.Code, response.Body)
	}
}

func TestSecondaryPublicationReconcilesLinkErrorAfterLastAttempt(t *testing.T) {
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
	state.Secondary.Copies[0].Attempts = backupSecondaryMaxAttempts - 1
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	linkReplyLost := errors.New("secondary storage returned an error after committing the link")
	automation.linkSecondaryArchive = func(root *os.Root, staged, named string) error {
		if err := root.Link(staged, named); err != nil {
			return err
		}
		return linkReplyLost
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, linkReplyLost) {
		t.Fatalf("secondary publication did not report the ambiguous link result: %v", err)
	}
	linked := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
	if !linked.pending() || linked.VerifiedAt != "" || linked.Attempts != backupSecondaryMaxAttempts {
		t.Fatalf("last linked attempt was not kept for reconciliation: %+v", linked)
	}
	*now = now.Add(time.Duration(backupSecondaryMaxAttempts) * backupSecondaryRetryBase)
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	copy := backupAutomationStateForTest(t, restarted).Secondary.Copies[0]
	if copy.VerifiedAt == "" || copy.Attempts != linked.Attempts || copy.FileIdentity != linked.FileIdentity {
		t.Fatalf("restart did not verify the already published link: %+v", copy)
	}
	entries, err := os.ReadDir(secondary)
	if err != nil || len(entries) != 1 || entries[0].Name() != copy.Archive.Name {
		t.Fatalf("reconciling the link left extra files: %v (%v)", entries, err)
	}
}

func TestSecondaryBackupResumesAfterAtomicLinkAndPreservesForeignFiles(t *testing.T) {
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
	state.Secondary.Copies[0].Attempts = backupSecondaryMaxAttempts - 1
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	copy := state.Secondary.Copies[0]
	root, binding, err := automation.openSecondaryDestination()
	if err != nil {
		t.Fatal(err)
	}
	if err := automation.stageSecondaryArchive(t.Context(), root, binding, &copy); err != nil {
		t.Fatal(err)
	}
	if err := root.Link(copy.Temporary, copy.Archive.Name); err != nil {
		t.Fatal(err)
	}
	if err := root.Close(); err != nil {
		t.Fatal(err)
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if copies := backupAutomationStateForTest(t, restarted).Secondary.Copies; len(copies) != 1 ||
		copies[0].VerifiedAt == "" || copies[0].FileIdentity != copy.FileIdentity ||
		copies[0].Attempts != backupSecondaryMaxAttempts {
		t.Fatalf("interrupted publication was not registered once: %+v", copies)
	}

	config := state.Config
	config.Retain = 1
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := restarted.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	foreign := filepath.Join(secondary, "unregistered.slubkp")
	if err := os.WriteFile(foreign, []byte("never owned by this installation"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(secondary, copy.Archive.Name)); !os.IsNotExist(err) {
		t.Fatalf("older owned secondary copy was not retained correctly: %v", err)
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Fatalf("retention changed a foreign file: %v", err)
	}
}

func TestSecondaryTransferReleasesExclusiveAdmissionAndPinsPrimarySource(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	state := backupAutomationStateForTest(t, automation)
	state.Config.Retain = 1
	if _, err := automation.Configure(t.Context(), state.Config); err != nil {
		t.Fatal(err)
	}
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	first := backupAutomationStateForTest(t, automation).Archives[0]
	entered := make(chan struct{})
	release := make(chan struct{})
	automation.copyArchive = func(dst io.Writer, src io.Reader) (int64, error) {
		if limited, ok := src.(*io.LimitedReader); ok {
			if _, copyingSecondary := limited.R.(secondaryArchiveReader); copyingSecondary {
				close(entered)
				select {
				case <-release:
				case <-t.Context().Done():
					return 0, t.Context().Err()
				}
			}
		}
		return io.Copy(dst, src)
	}
	finished := make(chan error, 1)
	go func() { finished <- automation.copyNextSecondaryBackup(t.Context()) }()
	defer func() {
		close(release)
		select {
		case err := <-finished:
			if err != nil {
				t.Errorf("secondary transfer failed: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("secondary transfer did not finish")
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("secondary transfer did not reach its file copy")
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Errorf("primary backup could not export while secondary copy was transferring: %v", err)
	}
	state = backupAutomationStateForTest(t, automation)
	if len(state.Archives) != 2 || state.Outcome != "succeeded" {
		t.Errorf("primary occurrence did not publish while secondary was copying: %+v", state)
	}
	if _, err := os.Stat(filepath.Join(automation.deployment.Directory, first.Name)); err != nil {
		t.Errorf("primary retention removed the active secondary source: %v", err)
	}
}

func TestSecondaryBackupInterruptedAttemptsRemainBounded(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	root, binding, err := automation.openSecondaryDestination()
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	for attempt := 1; attempt <= backupSecondaryMaxAttempts; attempt++ {
		copy := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
		if err := automation.stageSecondaryArchive(t.Context(), root, binding, &copy); err != nil {
			t.Fatalf("stage attempt %d: %v", attempt, err)
		}
		// Restart before the file is linked and the transfer is committed.
		restarted := newBackupAutomation(automation.deps)
		restarted.deployment = automation.deployment
		automation = restarted
		current := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
		if current.Attempts != attempt || !current.Transferring {
			t.Fatalf("attempt %d was not durably charged: %+v", attempt, current)
		}
	}
	automation.copyArchive = func(io.Writer, io.Reader) (int64, error) {
		t.Error("exhausted retry budget started another transfer")
		return 0, io.ErrUnexpectedEOF
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err == nil {
		t.Fatal("exhausted retry budget did not mark the uncommitted copy as failed")
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	copy := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
	if copy.pending() || copy.Attempts != backupSecondaryMaxAttempts || copy.Temporary != "" {
		t.Fatalf("exhausted copy did not settle: %+v", copy)
	}
	if entries, err := os.ReadDir(secondary); err != nil || len(entries) != 0 {
		t.Fatalf("exhausted attempt left a staging or published file: %v, %v", entries, err)
	}
}

func TestSecondaryRetentionCountsOnlyCopiesStillPresentAndUnchanged(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	config := backupAutomationStateForTest(t, automation).Config
	config.Retain = 3
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		*now = now.Add(time.Minute)
		if err := automation.Tick(t.Context(), true); err != nil {
			t.Fatal(err)
		}
		if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	copies := backupAutomationStateForTest(t, automation).Secondary.Copies
	if len(copies) != 3 {
		t.Fatalf("expected three registered secondary copies: %+v", copies)
	}
	if err := os.Remove(filepath.Join(secondary, copies[1].Archive.Name)); err != nil {
		t.Fatal(err)
	}
	config.Retain = 2
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	oldest := filepath.Join(secondary, copies[0].Archive.Name)
	if _, err := os.Stat(oldest); err != nil {
		t.Fatalf("retention removed the oldest of two available archives: %v", err)
	}
	if err := os.Chmod(filepath.Join(secondary, copies[2].Archive.Name), 0000); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err := automation.Status(t.Context())
	if err != nil || status.Secondary.State != "degraded" || status.Secondary.Failed != 2 ||
		status.Secondary.Failure != "copy_unavailable" || status.Secondary.CapturedAt != copies[0].Archive.CapturedAt {
		t.Fatalf("status relied on an unavailable copy: %+v (%v)", status.Secondary, err)
	}
	if err := os.Remove(oldest); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	status, err = automation.Status(t.Context())
	if err != nil || status.Secondary.State != "degraded" || status.Secondary.VerifiedAt != "" || status.Secondary.CapturedAt != "" {
		t.Fatalf("status still advertised a verified archive: %+v (%v)", status.Secondary, err)
	}
}

func TestSecondaryRetentionWaitsForConfirmedReplacementIntegrity(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	config := backupAutomationStateForTest(t, automation).Config
	config.Retain = 2
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
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
	older, newer := state.Secondary.Copies[0], state.Secondary.Copies[1]
	oldPath := filepath.Join(secondary, older.Archive.Name)
	newPath := filepath.Join(secondary, newer.Archive.Name)
	original, err := os.ReadFile(newPath)
	if err != nil {
		t.Fatal(err)
	}
	damaged := append([]byte(nil), original...)
	damaged[len(damaged)/2] ^= 1
	if err := os.WriteFile(newPath, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	modTime := parseBackupStamp(newer.ModifiedAt)
	if err := os.Chtimes(newPath, modTime, modTime); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(newPath)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate a filesystem whose metadata observation does not reveal
	// the changed bytes, including the ctime available on this test host.
	state.Secondary.Copies[1].ChangedAt = backupStamp(backupIntegrityChangeTime(info))
	if !secondaryCopyMatches(info, state.Secondary.Copies[1]) {
		t.Fatal("fixture changed the observed metadata")
	}
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	config.Retain = 1
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("retention removed the last healthy copy: %v", err)
	}
	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary == nil || status.Secondary.Failure != "copy_corrupt" ||
		status.Secondary.Integrity.State != backupIntegrityMismatch || status.Secondary.CapturedAt != older.Archive.CapturedAt {
		t.Fatalf("fallback status after restarting = %+v, %v", status.Secondary, err)
	}
	if err := os.WriteFile(newPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(backupIntegrityNewestAfter)
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("retention did not resume after confirming a healthy copy: %v", err)
	}
	state = backupAutomationStateForTest(t, restarted)
	if len(state.Secondary.Copies) != 1 || state.Secondary.Copies[0].VerifiedAt != newer.VerifiedAt ||
		state.Secondary.Copies[0].Integrity.State != backupIntegrityMatched ||
		state.Secondary.Copies[0].Integrity.CheckedAt == newer.VerifiedAt {
		t.Fatalf("publication and integrity evidence = %+v", state.Secondary.Copies)
	}
}

func TestSecondaryRetentionLeavesArchiveWithChangedDigestOnDisk(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	config := backupAutomationStateForTest(t, automation).Config
	config.Retain = 2
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
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
	copies := backupAutomationStateForTest(t, automation).Secondary.Copies
	path := filepath.Join(secondary, copies[0].Archive.Name)
	damaged, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	damaged[len(damaged)/2] ^= 1
	if err := os.WriteFile(path, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	config.Retain = 1
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, damaged) {
		t.Fatalf("retention changed a file that failed ownership-by-digest checks: %v", err)
	}
}

func TestSecondaryTransferStopsWritingDuringRestore(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	buffered := make(chan struct{})
	resume := make(chan struct{})
	automation.copyArchive = func(dst io.Writer, src io.Reader) (int64, error) {
		chunk := make([]byte, 32)
		if _, err := io.ReadFull(src, chunk); err != nil {
			return 0, err
		}
		close(buffered)
		select {
		case <-resume:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		n, err := dst.Write(chunk)
		return int64(n), err
	}
	finished := make(chan error, 1)
	go func() { finished <- automation.copyNextSecondaryBackup(ctx) }()
	select {
	case <-buffered:
	case <-ctx.Done():
		t.Fatal("secondary transfer could not buffer the first archive chunk")
	}
	runtime := runtimeComposition{deps: AppDeps{BackupAutomation: automation}}
	if err := runtime.PreparePersistenceReplacement(ctx); err != nil {
		t.Fatal(err)
	}
	close(resume)
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("buffered transfer writes were not canceled before replacement: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("secondary transfer did not stop")
	}
	copy := backupAutomationStateForTest(t, automation).Secondary.Copies[0]
	staged, err := os.Stat(filepath.Join(secondary, copy.Temporary))
	if err != nil || staged.Size() != 0 {
		t.Fatalf("buffered archive bytes were written during isolation: %v (%v)", staged, err)
	}
	if _, err := os.Stat(filepath.Join(secondary, copy.Archive.Name)); !os.IsNotExist(err) {
		t.Fatalf("canceled secondary copy became public: %v", err)
	}
}

func TestSecondaryPublicationReleasesAdmissionAndRecoversAfterLastSyncFailure(t *testing.T) {
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
	first := state.Archives[0]
	state.Secondary.Copies[0].Attempts = backupSecondaryMaxAttempts - 1
	if err := automation.save(t.Context(), state); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	syncFailed := errors.New("secondary directory sync temporarily failed")
	automation.syncSecondaryDirectory = func(*os.Root) error {
		close(entered)
		<-release
		return syncFailed
	}
	finished := make(chan error, 1)
	go func() { finished <- automation.copyNextSecondaryBackup(t.Context()) }()
	select {
	case <-entered:
	case err := <-finished:
		close(release)
		t.Fatalf("secondary backup did not reach directory sync: %v", err)
	case <-time.After(3 * time.Second):
		close(release)
		t.Fatal("secondary backup did not reach directory sync")
	}
	// The secondary has finished transferring bytes and is suspended in a
	// mounted filesystem operation, which must leave exclusive admission free.
	exportDone := make(chan error, 1)
	go func() { exportDone <- automation.Tick(t.Context(), true) }()
	var exportErr error
	select {
	case exportErr = <-exportDone:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("secondary directory sync blocked primary export")
	}
	close(release)
	select {
	case err := <-finished:
		if !errors.Is(err, syncFailed) {
			t.Fatalf("secondary sync error = %v, want %v", err, syncFailed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("secondary backup did not report the sync failure")
	}
	state = backupAutomationStateForTest(t, automation)
	if exportErr != nil || state.Outcome != "succeeded" || len(state.Archives) != 2 {
		t.Fatalf("secondary directory sync blocked primary export: %s, %d archives (%v)", state.Outcome, len(state.Archives), exportErr)
	}
	if info, err := os.Stat(filepath.Join(secondary, first.Name)); err != nil || info.Size() != *first.SizeBytes {
		t.Fatalf("last attempt did not retain its linked copy: %v (%v)", info, err)
	}

	restarted := newBackupAutomation(automation.deps)
	restarted.deployment = automation.deployment
	status, err := restarted.Status(t.Context())
	if err != nil || status.Secondary.State != "degraded" || status.Secondary.Failure != "publication_pending" ||
		status.Secondary.VerifiedAt != "" {
		t.Fatalf("unconfirmed publication not visible independently: %+v (%v)", status.Secondary, err)
	}
	restarted.copyArchive = func(io.Writer, io.Reader) (int64, error) {
		t.Error("registration of the linked archive started another transfer")
		return 0, io.ErrUnexpectedEOF
	}
	*now = now.Add(time.Duration(backupSecondaryMaxAttempts) * backupSecondaryRetryBase)
	if err := restarted.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	state = backupAutomationStateForTest(t, restarted)
	if copy := state.Secondary.Copies[0]; copy.VerifiedAt == "" || copy.Attempts != backupSecondaryMaxAttempts {
		t.Fatalf("last linked attempt was not finalized after restart: %+v", copy)
	}
}

func TestSecondaryRetentionRetriesUnreadableOwnedCopy(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("this test requires filesystem permissions that also restrict reads")
	}
	_, automation, now := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	config := backupAutomationStateForTest(t, automation).Config
	config.Retain = 2
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
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
	copies := backupAutomationStateForTest(t, automation).Secondary.Copies
	oldest := filepath.Join(secondary, copies[0].Archive.Name)
	if err := os.Chmod(oldest, 0000); err != nil {
		t.Fatal(err)
	}
	config.Retain = 1
	if _, err := automation.Configure(t.Context(), config); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("retention did not preserve unreadable file registration for retry: %v", err)
	}
	if err := os.Chmod(oldest, 0600); err != nil {
		t.Fatal(err)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Fatalf("retention never retried after restoring file access: %v", err)
	}
}

func TestSecondaryPublicationRejectsChangedFinalContentWithOriginalTimeAndSize(t *testing.T) {
	_, automation, _ := newBackupAutomationTest(t)
	secondary := secondaryBackupTestDirectory(t, automation)
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.validateSecondaryDestination(t.Context(), nil); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(secondary, backupAutomationStateForTest(t, automation).Archives[0].Name)
	automation.syncSecondaryDirectory = func(root *os.Root) error {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		blob, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		blob[len(blob)/2] ^= 0xff
		if err := os.WriteFile(path, blob, 0600); err != nil {
			return err
		}
		if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
			return err
		}
		return syncSecondaryDirectory(root)
	}
	if err := automation.copyNextSecondaryBackup(t.Context()); !errors.Is(err, errBackupSecondaryMismatch) {
		t.Fatalf("last digest did not detect changed final content: %v", err)
	}
	status, err := automation.Status(t.Context())
	if err != nil || status.Secondary.State != "degraded" || status.Secondary.VerifiedAt != "" {
		t.Fatalf("changed file was reported as verified: %+v (%v)", status.Secondary, err)
	}
}
