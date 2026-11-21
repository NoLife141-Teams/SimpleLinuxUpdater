package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestRestoreCompletesWhileAdminWaitsForPrimaryMount(t *testing.T) {
	for _, operation := range []string{"validate", "verify", "download"} {
		t.Run(operation, func(t *testing.T) {
			app, a, cookie, archive := publishedCatalogFixture(t)
			data, err := os.ReadFile(filepath.Join(a.deployment.Directory, archive.Name))
			if err != nil {
				t.Fatal(err)
			}
			restore := primaryAdminRestoreRequest(t, cookie, data)
			entered, unblock := make(chan struct{}), make(chan struct{})
			release := sync.OnceFunc(func() { close(unblock) })
			defer release()
			open := a.openPrimaryAdminDestination
			a.openPrimaryAdminDestination = func(validate bool) (*os.Root, string, string, string, error) {
				close(entered)
				<-unblock // Model a filesystem syscall that ignores cancellation.
				return open(validate)
			}

			method, path := http.MethodPost, "/api/backup/archives/"+archive.ID+"/"+operation
			var body []byte
			if operation == "validate" {
				method, path = http.MethodPut, "/api/backup/automation"
				body, err = json.Marshal(backupAutomationStateForTest(t, a).Config)
				if err != nil {
					t.Fatal(err)
				}
			}
			admin := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				admin <- performContractRequest(app.Handler, method, path, bytes.NewBuffer(body), cookie, true)
			}()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				t.Fatal("Admin request did not reach the mount")
			}

			restored := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				rec := httptest.NewRecorder()
				app.Handler.ServeHTTP(rec, restore)
				restored <- rec
			}()
			select {
			case rec := <-restored:
				if rec.Code != http.StatusOK {
					t.Fatalf("restore could not complete during blocked %s: %d %s", operation, rec.Code, rec.Body)
				}
			case <-time.After(8 * time.Second):
				t.Fatal("restore waited for the Admin mount operation")
			}
			select {
			case rec := <-admin:
				t.Fatalf("Admin operation was not still blocked: %d %s", rec.Code, rec.Body)
			default:
			}
			before := backupAutomationStateForTest(t, a)
			release()
			select {
			case rec := <-admin:
				if rec.Code < 400 {
					t.Fatalf("old Admin request succeeded after restore: %d", rec.Code)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("Admin operation did not finish once the mount returned")
			}
			after := backupAutomationStateForTest(t, a)
			if before.Approval != "" || after.Approval != "" || after.Outcome != "suspended" ||
				len(before.Archives) != len(after.Archives) {
				t.Fatalf("late Admin operation changed restored automation: %+v", after)
			}
		})
	}
}

func primaryAdminRestoreRequest(t *testing.T, cookie *http.Cookie, data []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	part, err := form.CreateFormFile("file", "restore.slubkp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("passphrase", "a separate automated backup secret"); err != nil {
		t.Fatal(err)
	}
	if err := form.WriteField("isolate", "true"); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/backup/restore", &body)
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.AddCookie(cookie)
	markSameOriginAuthRequest(req)
	return req
}

func TestRestoreStopsAnAuthorizedArchiveDownload(t *testing.T) {
	app, a, cookie, _ := publishedCatalogFixture(t)
	if _, err := app.Deps.DB().Exec("CREATE TABLE download_fixture (payload BLOB)"); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 128<<10)
	rand.Read(payload)
	if _, err := app.Deps.DB().Exec("INSERT INTO download_fixture VALUES (?)", payload); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	archives := backupAutomationStateForTest(t, a).Archives
	archive := archives[len(archives)-1]
	data, err := os.ReadFile(filepath.Join(a.deployment.Directory, archive.Name))
	if err != nil {
		t.Fatal(err)
	}
	if len(data) <= 64<<10 {
		t.Fatalf("expected a multi-chunk download, got %d bytes", len(data))
	}
	restore := primaryAdminRestoreRequest(t, cookie, data)
	entered, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	writer := &catalogMutatingWriter{ResponseRecorder: httptest.NewRecorder(), mutate: func() {
		close(entered)
		<-unblock
	}}
	download := httptest.NewRequest(http.MethodPost, "/api/backup/archives/"+archive.ID+"/download", nil)
	download.AddCookie(cookie)
	markSameOriginAuthRequest(download)
	downloadDone := make(chan struct{})
	go func() {
		defer close(downloadDone)
		app.Handler.ServeHTTP(writer, download)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not begin streaming")
	}
	restored := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, restore)
		restored <- rec
	}()
	select {
	case rec := <-restored:
		if rec.Code != http.StatusOK {
			t.Fatalf("restore while streaming = %d %s", rec.Code, rec.Body)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("restore waited for the HTTP writer")
	}
	release()
	select {
	case <-downloadDone:
		if writer.Body.Len() == len(data) {
			t.Fatal("superseded download continued after restore")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("superseded download did not stop")
	}
}

func TestAdminCatalogueCancellationKeepsStorageSlotUntilBlockedCallReturns(t *testing.T) {
	app, a, cookie, archive := publishedCatalogFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	unblock, entered := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	a.openPrimaryAdminDestination = func(bool) (*os.Root, string, string, string, error) {
		close(entered)
		<-unblock
		return a.destination(false)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/backup/archives/"+archive.ID+"/download", nil).WithContext(ctx)
	request.AddCookie(cookie)
	markSameOriginAuthRequest(request)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		app.Handler.ServeHTTP(response, request)
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not reach the mount")
	}
	cancel()
	select {
	case <-done:
		if response.Code != http.StatusRequestTimeout {
			t.Fatalf("cancelled download=%d %s", response.Code, response.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled download waited for the mount")
	}
	if retry := catalogRequest(app, cookie, archive.ID, "download"); retry.Code != http.StatusConflict {
		t.Fatalf("retry started another mount call: %d %s", retry.Code, retry.Body)
	}
	release()
	waitForPrimaryBackupWorker(t, a)
}

func TestPrimaryAdminViewsUseLastStorageObservation(t *testing.T) {
	app, a, cookie, _ := publishedCatalogFixture(t)
	directory := a.deployment.Directory
	withdrawn := directory + ".withdrawn"
	if err := os.Rename(directory, withdrawn); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(withdrawn) })
	for _, route := range []string{"/api/backup/status", "/api/backup/automation", "/api/backup/archives"} {
		response := performContractRequest(app.Handler, http.MethodGet, route, nil, cookie, false)
		if response.Code != http.StatusOK || !bytes.Contains(response.Body.Bytes(), []byte(`"availability":"present"`)) {
			t.Fatalf("%s = %d %s", route, response.Code, response.Body)
		}
	}
	if next, err := a.nextScheduledOccurrence(t.Context()); err != nil || next.IsZero() {
		t.Fatalf("projected backup occurrence = %v, %v", next, err)
	}
	if err := a.observeArchiveAvailability(t.Context()); err != nil {
		t.Fatal(err)
	}
	if next, err := a.nextScheduledOccurrence(t.Context()); err != nil || !next.IsZero() {
		t.Fatalf("confirmed primary outage still projects an occurrence: %v, %v", next, err)
	}
	if archive := catalogFacts(t, app, cookie).Archives[0]; archive.Availability != "unavailable" {
		t.Fatalf("catalogue hid the observed outage: %+v", archive)
	}
}

func TestPrimaryValidationDoesNotOverwriteNewerMountOutage(t *testing.T) {
	app, a, cookie, _ := publishedCatalogFixture(t)
	facts := backupAutomationStateForTest(t, a)
	payload, err := json.Marshal(facts.Config)
	if err != nil {
		t.Fatal(err)
	}
	// Model a result sampled before a newer observer transaction, both for
	// the same deployment and process. The late proof must not reset the alert.
	outageAt := a.now().Add(time.Minute)
	a.recordArchiveObservation(&facts, newBackupArchiveObservation(outageAt, "unavailable", "", ""))
	if err := a.save(t.Context(), facts); err != nil {
		t.Fatal(err)
	}
	response := performContractRequest(app.Handler, http.MethodPut, "/api/backup/automation", bytes.NewBuffer(payload), cookie, true)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("validation replaced the newer outage: %d %s", response.Code, response.Body)
	}
	if current := backupAutomationStateForTest(t, a).Observation; current != facts.Observation {
		t.Fatalf("validation changed the newer observation: %+v", current)
	}
}

func TestAdminValidationRejectsMountReplacedWhileWaiting(t *testing.T) {
	app, a, cookie, _ := publishedCatalogFixture(t)
	before := backupAutomationStateForTest(t, a)
	payload, err := json.Marshal(before.Config)
	if err != nil {
		t.Fatal(err)
	}
	open := a.openPrimaryAdminDestination
	validated, unblock := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(unblock) })
	defer release()
	a.openPrimaryAdminDestination = func(validate bool) (*os.Root, string, string, string, error) {
		root, pass, owner, binding, err := open(validate)
		close(validated)
		<-unblock
		return root, pass, owner, binding, err
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- performContractRequest(app.Handler, http.MethodPut, "/api/backup/automation", bytes.NewBuffer(payload), cookie, true)
	}()
	select {
	case <-validated:
	case <-time.After(5 * time.Second):
		t.Fatal("validation did not reach the mount")
	}
	directory := a.deployment.Directory
	displaced := directory + ".displaced"
	if err := os.Rename(directory, displaced); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(displaced) })
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case response := <-done:
		if response.Code != http.StatusBadRequest {
			t.Fatalf("replaced mount approval = %d %s", response.Code, response.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("validation did not finish")
	}
	after := backupAutomationStateForTest(t, a)
	if after.Approval != before.Approval || after.ApprovedDirectory != before.ApprovedDirectory {
		t.Fatal("approval changed after validating a withdrawn mount")
	}
}
