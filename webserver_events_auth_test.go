package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/events"
)

func TestRevokedSessionClosesDashboardStream(t *testing.T) {
	for _, cause := range []string{"revoked", "expired"} {
		t.Run(cause, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			cookie := app.authenticate(t)
			server := httptest.NewServer(app.Handler)
			defer server.Close()
			req, _ := http.NewRequest(http.MethodGet, server.URL+"/api/dashboard/events", nil)
			req.AddCookie(cookie)
			client := server.Client()
			client.Timeout = 5 * time.Second
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("stream status %d", resp.StatusCode)
			}
			reader := bufio.NewReader(resp.Body)
			readDashboardEventUntil(t, reader, `"connected"`)
			if cause == "expired" {
				if _, err := app.Deps.DB().Exec("UPDATE sessions SET expiry=julianday('2000-01-01')"); err != nil {
					t.Fatal(err)
				}
			} else {
				revoke := httptest.NewRequest(http.MethodDelete, "/api/auth/sessions", nil)
				revoke.AddCookie(cookie)
				markSameOriginAuthRequest(revoke)
				rec := httptest.NewRecorder()
				app.Handler.ServeHTTP(rec, revoke)
				if rec.Code != 200 {
					t.Fatalf("clear sessions status %d: %s", rec.Code, rec.Body.String())
				}

			}
			check := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
			check.AddCookie(cookie)
			checked := httptest.NewRecorder()
			app.Handler.ServeHTTP(checked, check)
			if checked.Code != 401 {
				t.Fatalf("revoked cookie still accepted: %d", checked.Code)
			}
			app.Deps.DashboardEventBroker.PublishEvent(events.Event{Reason: "job.log", ServerName: "private-host", JobID: "future-job", Data: "REVIEW_AFTER_REVOCATION"})
			data, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "REVIEW_AFTER_REVOCATION") {
				t.Fatal("revoked stream received private data")
			}

		})
	}
}
