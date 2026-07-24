package main

import (
	"bufio"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/events"

	"github.com/gin-gonic/gin"
)

func readDashboardEventLine(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	lines := make(chan string, 1)
	errs := make(chan error, 1)
	go func() {
		line, err := reader.ReadString('\n')
		if err != nil {
			errs <- err
			return
		}
		lines <- line
	}()

	select {
	case line := <-lines:
		return line
	case err := <-errs:
		t.Fatalf("read SSE line: %v", err)
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for SSE line")
	}
	return ""
}

func readDashboardEventUntil(t *testing.T, reader *bufio.Reader, want string) {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for SSE line containing %q", want)
		default:
		}
		if line := readDashboardEventLine(t, reader); strings.Contains(line, want) {
			return
		}
	}
}

func TestDashboardEventsRouteUsesInjectedBroker(t *testing.T) {
	broker := events.NewBroker()
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "dashboard-events.db"), AppDeps{
		DashboardEventBroker: broker,
	})
	sessionCookie := app.authenticate(t)

	server := httptest.NewServer(app.Handler)
	defer server.Close()

	req, err := http.NewRequest(http.MethodGet, server.URL+"/api/dashboard/events", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.AddCookie(sessionCookie)

	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/dashboard/events error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/dashboard/events status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	reader := bufio.NewReader(resp.Body)
	readDashboardEventUntil(t, reader, "event: dashboard")
	readDashboardEventUntil(t, reader, `data: {"reason":"connected"}`)

	broker.Publish("injected")
	readDashboardEventUntil(t, reader, "event: dashboard")
	readDashboardEventUntil(t, reader, `data: {"reason":"injected"}`)

	broker.PublishEvent(events.Event{
		Reason:     "job.log",
		ServerName: "demo-host",
		JobID:      "job-1",
		Sequence:   4,
		Stream:     "stdout",
		Data:       "Reading 40%\r",
	})
	readDashboardEventUntil(t, reader, "event: dashboard")
	readDashboardEventUntil(t, reader, `"reason":"job.log","server_name":"demo-host","job_id":"job-1","sequence":4,"stream":"stdout","data":"Reading 40%\\r"`)
}

func TestDashboardEventsNilBrokerReturnsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.GET("/api/dashboard/events", func(c *gin.Context) {
		handleDashboardEventsWithBroker(c, nil)
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/dashboard/events", nil)
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("nil broker status = %d, want %d (body=%s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"error":"streaming unavailable"`) {
		t.Fatalf("nil broker body = %s, want streaming unavailable error", rec.Body.String())
	}
}
