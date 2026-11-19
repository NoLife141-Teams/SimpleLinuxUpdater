package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/events"

	"github.com/gin-gonic/gin"
)

func newDashboardEventsTCPServer(t *testing.T, writeTimeout time.Duration, handler gin.HandlerFunc) (*http.Client, string, <-chan struct{}) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlerDone := make(chan struct{}, 1)
	router.GET("/api/dashboard/events", func(c *gin.Context) {
		defer func() { handlerDone <- struct{}{} }()
		handler(c)
	})

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := &http.Server{
		Handler:      router,
		WriteTimeout: writeTimeout,
	}
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("shutdown dashboard events server: %v", err)
		}
		select {
		case err := <-serveDone:
			if err != nil && err != http.ErrServerClosed {
				t.Errorf("serve dashboard events: %v", err)
			}
		case <-time.After(time.Second):
			t.Error("dashboard events server did not stop")
		}
	})

	return &http.Client{}, "http://" + listener.Addr().String(), handlerDone
}

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

func readDashboardEventUntil(t *testing.T, reader *bufio.Reader, want string) string {
	t.Helper()
	deadline := time.After(time.Second)
	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for SSE line containing %q", want)
		default:
		}
		if line := readDashboardEventLine(t, reader); strings.Contains(line, want) {
			return line
		}
	}
}

func readDashboardPayload(t *testing.T, reader *bufio.Reader, reason string) events.Event {
	t.Helper()
	expected := `"reason":` + strconv.Quote(reason)
	line := readDashboardEventUntil(t, reader, expected)
	var event events.Event
	if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
		t.Fatalf("decode dashboard event: %v", err)
	}
	return event
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
	event := readDashboardPayload(t, reader, "injected")
	if event.Revision == 0 {
		t.Fatal("dashboard invalidation must carry a revision")
	}

	broker.PublishEvent(events.Event{
		Reason:     "job.log",
		ServerName: "demo-host",
		JobID:      "job-1",
		Sequence:   4,
		Stream:     "stdout",
		Data:       "Reading 40%\r",
	})
	readDashboardEventUntil(t, reader, "event: dashboard")
	jobLog := readDashboardPayload(t, reader, "job.log")
	if jobLog.ServerName != "demo-host" || jobLog.JobID != "job-1" || jobLog.Sequence != 4 || jobLog.Stream != "stdout" || jobLog.Data != "Reading 40%\r" {
		t.Fatalf("unexpected log event: %+v", jobLog)
	}
	broker.PublishEvent(events.Event{Reason: "job.update", ServerName: "demo-host"})
	scoped := readDashboardPayload(t, reader, "job.update")
	if scoped.Revision != event.Revision+1 || scoped.ServerName != "demo-host" {
		t.Fatalf("scoped invalidation must carry the next dashboard revision across log fragments: %+v", scoped)
	}
}

func TestDashboardEventsSurvivesServerWriteTimeout(t *testing.T) {
	const serverWriteTimeout = 100 * time.Millisecond

	broker := events.NewBroker()
	client, baseURL, _ := newDashboardEventsTCPServer(t, serverWriteTimeout, func(c *gin.Context) {
		handleDashboardEventsWithBroker(c, broker)
	})
	resp, err := client.Get(baseURL + "/api/dashboard/events")
	if err != nil {
		t.Fatalf("GET /api/dashboard/events error = %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readDashboardEventUntil(t, reader, `data: {"reason":"connected"}`)

	timeoutWindow := time.NewTimer(2 * serverWriteTimeout)
	defer timeoutWindow.Stop()
	<-timeoutWindow.C

	broker.Publish("after-write-timeout")
	readDashboardPayload(t, reader, "after-write-timeout")
}

func TestDashboardEventsHeartbeatsRenewStreamWriteDeadline(t *testing.T) {
	const heartbeatInterval = 20 * time.Millisecond
	broker := events.NewBroker()
	config := dashboardEventsStreamConfig{
		heartbeatInterval: heartbeatInterval,
		writeTimeout:      2 * heartbeatInterval,
	}
	client, baseURL, _ := newDashboardEventsTCPServer(t, 15*time.Millisecond, func(c *gin.Context) {
		handleDashboardEventsWithConfig(c, broker, config)
	})
	resp, err := client.Get(baseURL + "/api/dashboard/events")
	if err != nil {
		t.Fatalf("GET /api/dashboard/events error = %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readDashboardEventUntil(t, reader, `data: {"reason":"connected"}`)
	for range 4 {
		readDashboardEventUntil(t, reader, ": keepalive")
	}

	broker.Publish("after-heartbeats")
	readDashboardPayload(t, reader, "after-heartbeats")
}

func TestDashboardEventsClientDisconnectStopsHandler(t *testing.T) {
	broker := events.NewBroker()
	client, baseURL, handlerDone := newDashboardEventsTCPServer(t, time.Second, func(c *gin.Context) {
		handleDashboardEventsWithBroker(c, broker)
	})
	resp, err := client.Get(baseURL + "/api/dashboard/events")
	if err != nil {
		t.Fatalf("GET /api/dashboard/events error = %v", err)
	}

	reader := bufio.NewReader(resp.Body)
	readDashboardEventUntil(t, reader, `data: {"reason":"connected"}`)
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close SSE response: %v", err)
	}

	select {
	case <-handlerDone:
	case <-time.After(time.Second):
		t.Fatal("SSE handler did not stop after client disconnect")
	}
}

func TestDashboardEventsBlockedClientReachesStreamWriteDeadline(t *testing.T) {
	broker := events.NewBroker()
	config := dashboardEventsStreamConfig{
		heartbeatInterval: time.Second,
		writeTimeout:      100 * time.Millisecond,
	}
	client, baseURL, handlerDone := newDashboardEventsTCPServer(t, 0, func(c *gin.Context) {
		handleDashboardEventsWithConfig(c, broker, config)
	})
	resp, err := client.Get(baseURL + "/api/dashboard/events")
	if err != nil {
		t.Fatalf("GET /api/dashboard/events error = %v", err)
	}
	defer resp.Body.Close()

	reader := bufio.NewReader(resp.Body)
	readDashboardEventUntil(t, reader, `data: {"reason":"connected"}`)
	broker.PublishEvent(events.Event{Reason: "blocked-client", Data: strings.Repeat("x", 16<<20)})

	select {
	case <-handlerDone:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked SSE handler exceeded its stream write deadline")
	}
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
