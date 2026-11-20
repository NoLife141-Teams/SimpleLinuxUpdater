package main

import (
	"bytes"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVerifiedSecondaryBackupSurvivesPrimaryLossAndRestoresInIsolation(t *testing.T) {
	app, automation, now := newBackupAutomationTest(t)
	secondary := t.TempDir()
	if err := os.Chmod(secondary, 0700); err != nil {
		t.Fatal(err)
	}
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

func TestSecondaryBackupResumesAfterAtomicLinkAndPreservesForeignFiles(t *testing.T) {
	_, automation, now := newBackupAutomationTest(t)
	secondary := t.TempDir()
	if err := os.Chmod(secondary, 0700); err != nil {
		t.Fatal(err)
	}
	automation.deployment.SecondaryDirectory = secondary
	if _, err := automation.ValidateSecondaryDestination(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := automation.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	state := backupAutomationStateForTest(t, automation)
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
		copies[0].VerifiedAt == "" || copies[0].FileIdentity != copy.FileIdentity {
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
	secondary := t.TempDir()
	if err := os.Chmod(secondary, 0700); err != nil {
		t.Fatal(err)
	}
	automation.deployment.SecondaryDirectory = secondary
	state := backupAutomationStateForTest(t, automation)
	state.Config.Retain = 1
	if _, err := automation.Configure(t.Context(), state.Config); err != nil {
		t.Fatal(err)
	}
	if _, err := automation.ValidateSecondaryDestination(t.Context()); err != nil {
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
