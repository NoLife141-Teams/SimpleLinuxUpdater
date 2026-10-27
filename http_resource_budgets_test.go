package main

import (
	"bufio"
	"bytes"
	serverpkg "debian-updater/internal/servers"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRejectedBackupRequestsKeepOrdinaryDeadlines(t *testing.T) {
	for _, path := range []string{"/api/backup/export", "/api/backup/restore", "/api/backup/verify", "/api/backup/automation/run", "/api/servers/web/postchecks/test", "/api/backup/unknown"} {
		for _, method := range []string{http.MethodPost, http.MethodPut} {
			t.Run(method+path, func(t *testing.T) {
				done := make(chan struct{})
				app := newApplicationHTTPServer("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					defer close(done)
					w.WriteHeader(http.StatusUnauthorized)
				}))
				server := httptest.NewUnstartedServer(app.Handler)
				server.Config.ReadTimeout = 40 * time.Millisecond
				server.Config.WriteTimeout = 40 * time.Millisecond
				server.Start()
				defer server.Close()
				u, _ := url.Parse(server.URL)
				conn, err := net.DialTimeout("tcp", u.Host, time.Second)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(400 * time.Millisecond))
				fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: %s\r\nContent-Length: 20\r\n\r\n", method, path, u.Host)
				_, err = bufio.NewReader(conn).ReadString('\n')
				if e, ok := err.(net.Error); ok && e.Timeout() {
					t.Fatalf("refused request retained connection beyond ordinary deadline: %v", err)
				}
				select {
				case <-done:
				case <-time.After(time.Second):
					t.Fatal("handler leaked")
				}
			})
		}
	}
}

func TestInventoryJSONResourceBoundary(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, tc := range []struct {
		name, suffix string
		size, want   int
	}{
		{"below", "", (1 << 20) - 1, http.StatusCreated},
		{"at", "", 1 << 20, http.StatusCreated},
		{"over", "", (1 << 20) + 1, http.StatusRequestEntityTooLarge},
		{"audit-reproduction", "", 4 << 20, http.StatusRequestEntityTooLarge},
		{"additional-object", " {}", 0, http.StatusBadRequest},
		{"trailing-garbage", " !", 0, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := fmt.Sprintf(`{"name":%q,"host":%q,"port":22,"user":"root","ignored_padding":""}`, tc.name, tc.name+".example.test")
			if tc.size > len(raw) {
				raw = strings.TrimSuffix(raw, `"}`) + strings.Repeat("x", tc.size-len(raw)) + `"}`
			}
			raw += tc.suffix
			rec := performContractRequest(app.Handler, http.MethodPost, "/api/servers", bytes.NewBufferString(raw), cookie, true)
			if rec.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", rec.Code, tc.want, rec.Body.String())
			}
			_, exists := app.Deps.ServerState.FindByName(tc.name)
			if exists != (tc.want == http.StatusCreated) {
				t.Fatalf("persisted=%v after status=%d", exists, rec.Code)
			}
		})
	}
}

func TestRouterRefusalsKeepOrdinaryHTTPDeadlines(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, tc := range []struct {
		name, method, path    string
		authenticated, origin bool
	}{
		{"export-auth", "POST", "/api/backup/export", false, true},
		{"restore-auth", "POST", "/api/backup/restore", false, true},
		{"verify-auth", "POST", "/api/backup/verify", false, true},
		{"automation-auth", "POST", "/api/backup/automation/run", false, true},
		{"probe-auth", "POST", "/api/servers/web/postchecks/test", false, true},
		{"export-origin", "POST", "/api/backup/export", true, false},
		{"restore-origin", "POST", "/api/backup/restore", true, false},
		{"verify-origin", "POST", "/api/backup/verify", true, false},
		{"automation-origin", "POST", "/api/backup/automation/run", true, false},
		{"probe-origin", "POST", "/api/servers/web/postchecks/test", true, false},
		{"wrong-method", "PUT", "/api/backup/verify", true, true},
		{"unknown-route", "POST", "/api/backup/missing", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ended := make(chan struct{})
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { defer close(ended); app.Handler.ServeHTTP(w, r) })
			server := newResourceBudgetNetworkServer(t, handler, 40*time.Millisecond, 300*time.Millisecond)
			u, _ := url.Parse(server.URL)
			conn, err := net.DialTimeout("tcp", u.Host, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(time.Second))
			headers := ""
			if tc.authenticated {
				headers += "Cookie: " + cookie.String() + "\r\n"
			}
			if tc.origin {
				headers += "Origin: http://localhost\r\nSec-Fetch-Site: same-origin\r\n"
			}
			fmt.Fprintf(conn, "%s %s HTTP/1.1\r\nHost: localhost\r\n%sContent-Length: 20\r\n\r\n", tc.method, tc.path, headers)
			_, err = io.ReadAll(conn)
			if e, ok := err.(net.Error); ok && e.Timeout() {
				t.Fatal("refused request kept connection alive")
			}
			select {
			case <-ended:
			case <-time.After(time.Second):
				t.Fatal("refused handler leaked")
			}
		})
	}
}

func newResourceBudgetNetworkServer(t *testing.T, handler http.Handler, ordinary, special time.Duration) *httptest.Server {
	t.Helper()
	application := newApplicationHTTPServer("", handler)
	application.Handler = applicationHTTPBudgetHandler(handler, httpResourceBudgets{backup: special, postcheck: special})
	server := httptest.NewUnstartedServer(application.Handler)
	server.Config = application
	server.Config.ReadTimeout = ordinary
	server.Config.WriteTimeout = ordinary
	server.Start()
	t.Cleanup(server.Close)
	server.Client().Timeout = 3 * time.Second
	return server
}

func TestInventoryChunkedJSONBudgetHasNoMutationOrSuccessAudit(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	server := newResourceBudgetNetworkServer(t, app.Handler, time.Second, 2*time.Second)
	for _, size := range []int{(1 << 20) - 1, 1 << 20, (1 << 20) + 1} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			name := fmt.Sprintf("chunked-%d", size)
			raw := fmt.Sprintf(`{"name":%q,"host":%q,"user":"root","ignored_padding":""}`, name, name+".example.test")
			raw = strings.TrimSuffix(raw, `"}`) + strings.Repeat("x", size-len(raw)) + `"}`
			req, _ := http.NewRequest("POST", server.URL+"/api/servers", io.NopCloser(strings.NewReader(raw)))
			req.ContentLength = -1
			req.AddCookie(cookie)
			markSameOriginAuthRequest(req)
			req.Header.Set("Content-Type", "application/json")
			resp, err := server.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			data, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			want := http.StatusCreated
			if size > 1<<20 {
				want = http.StatusRequestEntityTooLarge
			}
			if resp.StatusCode != want {
				t.Fatalf("status=%d body=%s", resp.StatusCode, data)
			}
			if !json.Valid(data) {
				t.Fatalf("response contains multiple or invalid JSON documents: %s", data)
			}
			if want == http.StatusRequestEntityTooLarge {
				var count int
				if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM servers WHERE name = ?", name).Scan(&count); err != nil || count != 0 {
					t.Fatalf("server count=%d err=%v", count, err)
				}
				if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='server.create' AND status='success' AND target_name=?", name).Scan(&count); err != nil || count != 0 {
					t.Fatalf("success audit count=%d err=%v", count, err)
				}
			}
		})
	}
}

func TestHTTPTransportControllerErrorsAreNotIgnored(t *testing.T) {
	reached := false
	handler := applicationHTTPBudgetHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, _ := gin.CreateTestContext(w)
		c.Request = r
		finish, ok := beginHTTPTransferBudget(c, true, false)
		if ok {
			defer finish()
			reached = true
		}
	}), defaultHTTPResourceBudgets)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("POST", "/api/backup/verify", nil))
	if reached || rec.Code != 500 || !json.Valid(rec.Body.Bytes()) {
		t.Fatalf("controller failure ignored: reached=%v status=%d body=%s", reached, rec.Code, rec.Body.String())
	}
}

func TestAuthorizedBackupNetworkBudgetsAndCleanup(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	temp := t.TempDir()
	t.Setenv("TMPDIR", temp)
	var finishedMu sync.Mutex
	var verifyFinished chan struct{}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if r.URL.Path == "/api/backup/verify" {
				finishedMu.Lock()
				ch := verifyFinished
				finishedMu.Unlock()
				if ch != nil {
					close(ch)
				}
			}
		}()
		app.Handler.ServeHTTP(w, r)
	})
	server := newResourceBudgetNetworkServer(t, handler, 40*time.Millisecond, 2*time.Second)
	request := func(path, contentType string, body io.Reader, length int64) *http.Response {
		t.Helper()
		req, err := http.NewRequest("POST", server.URL+path, body)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = length
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
		req.Header.Set("Content-Type", contentType)
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	exportJSON := `{"passphrase":"disposable-backup-network-passphrase","include_known_hosts":false}`
	resp := request("/api/backup/export", "application/json", strings.NewReader(exportJSON), int64(len(exportJSON)))
	archive, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("export=%d err=%v body=%s", resp.StatusCode, err, archive)
	}
	var upload bytes.Buffer
	writer := multipart.NewWriter(&upload)
	part, err := writer.CreateFormFile("file", "network.slubkp")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive); err != nil {
		t.Fatal(err)
	}
	writer.WriteField("passphrase", "disposable-backup-network-passphrase")
	writer.Close()
	// The authenticated multipart transfer pauses longer than the ordinary read
	// and write budgets, then decrypts and verifies the real exported archive.
	reader, pipe := io.Pipe()
	writerDone := make(chan error, 1)
	go func() {
		_, err := pipe.Write(upload.Bytes()[:100])
		if err == nil {
			timer := time.NewTimer(120 * time.Millisecond)
			<-timer.C
			_, err = pipe.Write(upload.Bytes()[100:])
		}
		pipe.CloseWithError(err)
		writerDone <- err
	}()
	resp = request("/api/backup/verify", writer.FormDataContentType(), reader, int64(upload.Len()))
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("slow verify=%d err=%v body=%s", resp.StatusCode, err, data)
	}
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	// A peer cancellation after multipart has spilled to disk must end the
	// handler and remove temporary files, without a successful verify audit.
	var before int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='backup.verify' AND status='success'").Scan(&before); err != nil {
		t.Fatal(err)
	}
	for _, cancelPeer := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelPeer), func(t *testing.T) {
			ended := make(chan struct{})
			finishedMu.Lock()
			verifyFinished = ended
			finishedMu.Unlock()
			u, _ := url.Parse(server.URL)
			conn, err := net.DialTimeout("tcp", u.Host, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(3 * time.Second))
			boundary := "disposable-boundary"
			fmt.Fprintf(conn, "POST /api/backup/verify HTTP/1.1\r\nHost: localhost\r\nOrigin: http://localhost\r\nSec-Fetch-Site: same-origin\r\nCookie: %s\r\nContent-Type: multipart/form-data; boundary=%s\r\nContent-Length: %d\r\n\r\n--%s\r\nContent-Disposition: form-data; name=\"file\"; filename=\"partial.slubkp\"\r\n\r\n", cookie.String(), boundary, 4<<20, boundary)
			if _, err := io.CopyN(conn, strings.NewReader(strings.Repeat("x", (1<<20)+4096)), (1<<20)+4096); err != nil {
				t.Fatal(err)
			}
			if cancelPeer {
				conn.Close()
			}
			select {
			case <-ended:
			case <-time.After(2800 * time.Millisecond):
				t.Fatal("partial backup handler or cancellation callback leaked")
			}
			files, err := os.ReadDir(temp)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatalf("temporary files leaked: %v", files)
			}
			var after int
			if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM audit_events WHERE action='backup.verify' AND status='success'").Scan(&after); err != nil || after != before {
				t.Fatalf("rejected upload recorded success: before=%d after=%d err=%v", before, after, err)
			}
		})
	}
}

func TestCommandJSONRejectsMalformedOrAdditionalDocumentsBeforeMutation(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, tc := range []struct{ method, path, body, contentType string }{
		{"POST", "/api/metrics/token", `{"unterminated":`, "application/json"},
		{"POST", "/api/metrics/token", `{} {}`, "application/json"},
		{"POST", "/api/servers", `{"name":"form-bypass","host":"192.0.2.1","user":"root"} {}`, "application/x-www-form-urlencoded"},
		{"DELETE", "/api/keys/global", `{} {}`, "application/json"},
		{"DELETE", "/api/servers/missing/key", `{`, "application/json"},
	} {
		t.Run(tc.method+tc.path+tc.body, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			req.AddCookie(cookie)
			markSameOriginAuthRequest(req)
			req.Header.Set("Content-Type", tc.contentType)
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, req)
			if rec.Code != 400 || !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	var count int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM audit_events WHERE status='success' AND action != 'auth.setup'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("command recorded success: count=%d err=%v", count, err)
	}
	// No-body commands retain their supported empty request.
	rec := performContractRequest(app.Handler, "DELETE", "/api/keys/global", nil, cookie, true)
	if rec.Code != 200 {
		t.Fatalf("empty command status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestSpecializedRequestBodyBudgetsAndInventoryBOM(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, tc := range []struct {
		method, path string
		limit        int
	}{
		{"POST", "/api/auth/login", authMaxRequestBytes},
		{"POST", "/api/auth/mfa/challenge", authMaxRequestBytes},
		{"POST", "/api/auth/accounts", authMaxRequestBytes},
		{"POST", "/api/servers/onboarding/check", 128 * 1024},
		{"PUT", "/api/postcheck-profiles", 256 * 1024},
		{"PUT", "/api/backup/automation", 4096},
		{"POST", "/api/backup/export", backupMaxExportRequestBytes},
		{"POST", "/api/servers/import/preview?format=json", serverpkg.MaxImportBytes},
		{"POST", "/api/servers/import", serverpkg.MaxImportSelectionBytes},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rec := performContractRequest(app.Handler, tc.method, tc.path, bytes.NewBufferString(strings.Repeat(" ", tc.limit+1)), cookie, true)
			if rec.Code != 413 || !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
	for _, tc := range []struct {
		format, body string
		want         int
	}{
		{"json", `[{"name":"json-bom","host":"192.0.2.10","user":"root"}]`, 200},
		{"json", `[{"name":"json-bom","host":"192.0.2.10","user":"root"}] {}`, 400},
		{"csv", "name,host,port,user,tags\ncsv-bom,192.0.2.11,22,root,[]\n", 200},
	} {
		t.Run(tc.format+tc.body, func(t *testing.T) {
			rec := performContractRequest(app.Handler, "POST", "/api/servers/import/preview?format="+tc.format, bytes.NewBufferString("\xef\xbb\xbf"+tc.body), cookie, true)
			if rec.Code != tc.want {
				t.Fatalf("BOM import status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAdmittedBackupBlockedResponseHasFiniteBudget(t *testing.T) {
	result := make(chan error, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, finish, err := beginHTTPTransferBudgetContext(r.Context(), false, false)
		if err != nil {
			result <- err
			return
		}
		defer finish()
		// More than the socket buffer can accept when the client stops reading.
		chunk := make([]byte, 64*1024)
		for i := 0; i < 1024; i++ {
			if _, err := w.Write(chunk); err != nil {
				result <- err
				return
			}
		}
		result <- nil
	})
	server := newResourceBudgetNetworkServer(t, handler, 40*time.Millisecond, 150*time.Millisecond)
	u, _ := url.Parse(server.URL)
	conn, err := net.DialTimeout("tcp", u.Host, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprint(conn, "POST /api/backup/export HTTP/1.1\r\nHost: localhost\r\nContent-Length: 0\r\n\r\n")
	select {
	case err := <-result:
		var timeout net.Error
		if !errors.As(err, &timeout) || !timeout.Timeout() {
			t.Fatalf("blocked writer error=%v, want timeout", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked response writer leaked beyond admitted budget")
	}
}

func TestOversizedBackupFileUses413BeforeFileRead(t *testing.T) {
	app := newIsolatedTestApp(t)
	for _, restore := range []bool{false, true} {
		t.Run(fmt.Sprintf("restore=%v", restore), func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			part, err := writer.CreateFormFile("file", "oversized.slubkp")
			if err != nil {
				t.Fatal(err)
			}
			part.Write([]byte("x"))
			writer.WriteField("passphrase", "disposable-backup-passphrase")
			writer.Close()
			c.Request = httptest.NewRequest("POST", "/api/backup/verify", &body)
			c.Request.Header.Set("Content-Type", writer.FormDataContentType())
			if err := c.Request.ParseMultipartForm(backupMultipartMemoryBytes); err != nil {
				t.Fatal(err)
			}
			// Preserve an openable parsed file, injecting just its measured size
			// instead of allocating a multi-gigabyte archive in this boundary test.
			c.Request.MultipartForm.File["file"][0].Size = backupMaxUploadBytes + 1
			if restore {
				handleBackupRestoreWithLifecycle(c, newBackupOperationLifecycle(app.Deps), app.Deps.CurrentSessionManager)
			} else {
				handleBackupVerifyWithService(c, app.Deps.BackupService)
			}
			if rec.Code != 413 || !json.Valid(rec.Body.Bytes()) {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
