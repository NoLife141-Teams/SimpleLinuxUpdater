package main

import (
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestApplicationHTTPServerProtectsOrdinaryRequests(t *testing.T) {
	handler := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	server := newApplicationHTTPServer("127.0.0.1:0", handler)

	if server.ReadHeaderTimeout != serverReadHeaderTimeout {
		t.Fatalf("ReadHeaderTimeout = %s, want %s", server.ReadHeaderTimeout, serverReadHeaderTimeout)
	}
	if server.ReadTimeout != serverReadHeaderTimeout {
		t.Fatalf("ReadTimeout = %s, want %s", server.ReadTimeout, serverReadHeaderTimeout)
	}
	if server.WriteTimeout != serverWriteTimeout {
		t.Fatalf("WriteTimeout = %s, want %s", server.WriteTimeout, serverWriteTimeout)
	}
	if server.IdleTimeout != serverIdleTimeout {
		t.Fatalf("IdleTimeout = %s, want %s", server.IdleTimeout, serverIdleTimeout)
	}
	if server.Handler == nil {
		t.Fatal("Handler = nil")
	}
}

func TestBackupTransferDeadlinesAreRelaxedWithoutRelaxingOtherBodies(t *testing.T) {
	const requestDeadline = 75 * time.Millisecond

	t.Run("slow backup upload and response complete", func(t *testing.T) {
		started := make(chan struct{})
		readResult := make(chan error, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			_, err := io.ReadAll(r.Body)
			readResult <- err
			if err == nil {
				_, _ = io.WriteString(w, "ok")
			}
		})
		server := newDeadlineTestServer(t, requestDeadline, handler)

		bodyReader, bodyWriter := io.Pipe()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/backup/restore", bodyReader)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = 2
		response := make(chan struct {
			resp *http.Response
			err  error
		}, 1)
		go func() {
			resp, err := server.Client().Do(req)
			response <- struct {
				resp *http.Response
				err  error
			}{resp: resp, err: err}
		}()
		if _, err := bodyWriter.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		<-started
		time.Sleep(2 * requestDeadline)
		if _, err := bodyWriter.Write([]byte("b")); err != nil {
			t.Fatal(err)
		}
		if err := bodyWriter.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-readResult; err != nil {
			t.Fatalf("backup body read failed after server deadline: %v", err)
		}
		result := <-response
		if result.err != nil {
			t.Fatalf("slow backup response failed after server write deadline: %v", result.err)
		}
		defer result.resp.Body.Close()
		payload, err := io.ReadAll(result.resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(payload) != "ok" {
			t.Fatalf("response body = %q, want ok", payload)
		}
	})

	t.Run("ordinary body retains deadline", func(t *testing.T) {
		started := make(chan struct{})
		readResult := make(chan error, 1)
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			_, err := io.ReadAll(r.Body)
			readResult <- err
		})
		server := newDeadlineTestServer(t, requestDeadline, handler)

		bodyReader, bodyWriter := io.Pipe()
		req, err := http.NewRequest(http.MethodPost, server.URL+"/api/ordinary", bodyReader)
		if err != nil {
			t.Fatal(err)
		}
		req.ContentLength = 2
		requestDone := make(chan error, 1)
		go func() {
			resp, err := server.Client().Do(req)
			if resp != nil {
				_ = resp.Body.Close()
			}
			requestDone <- err
		}()
		if _, err := bodyWriter.Write([]byte("a")); err != nil {
			t.Fatal(err)
		}
		<-started
		select {
		case err := <-readResult:
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Fatalf("ordinary body read error = %v, want timeout", err)
			}
		case <-time.After(10 * requestDeadline):
			t.Fatal("ordinary request body did not time out")
		}
		_ = bodyWriter.Close()
		<-requestDone
	})
}

func newDeadlineTestServer(t *testing.T, deadline time.Duration, handler http.Handler) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(backupTransferDeadlineHandler(handler))
	server.Config.ReadHeaderTimeout = deadline
	server.Config.ReadTimeout = deadline
	server.Config.WriteTimeout = deadline
	server.Start()
	t.Cleanup(server.Close)
	server.Client().Timeout = 5 * time.Second
	return server
}

func TestBackupTransferDeadlineHandlerLeavesOtherPathsWrapped(t *testing.T) {
	called := false
	handler := backupTransferDeadlineHandler(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		called = true
		_, _ = io.WriteString(w, "ok")
	}))
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, req)
	if !called || !strings.Contains(recorder.Body.String(), "ok") {
		t.Fatal("ordinary request did not reach wrapped handler")
	}
}
