package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfterDeadline(t *testing.T) {
	now := time.Date(2026, 11, 20, 5, 0, 0, 250_000_000, time.UTC)
	date := func(delay time.Duration) string { return now.Add(delay).Format(http.TimeFormat) }
	tests := []struct {
		name   string
		values []string
		delay  time.Duration
	}{
		{"seconds", []string{" 42 "}, 42 * time.Second},
		{"HTTP date", []string{date(31 * time.Second)}, 30*time.Second + 750*time.Millisecond},
		{"zero seconds", []string{"0"}, time.Second},
		{"near future date", []string{date(time.Second)}, time.Second},
		{"distant date", []string{date(3 * time.Hour)}, time.Hour},
		{"out of bounds seconds", []string{"3601"}, time.Hour},
		{"overflowing seconds", []string{"999999999999999999999999999999999999"}, time.Hour},
		{"expired date", []string{date(-time.Second)}, 0},
		{"absent", nil, 0},
		{"ambiguous", []string{"1", "2"}, 0},
		{"combined", []string{"1, 2"}, 0},
		{"fractional", []string{"1.25"}, 0},
		{"negative", []string{"-1"}, 0},
		{"invalid", []string{"https://example.test/secret"}, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var want time.Time
			if tc.delay != 0 {
				want = now.Add(tc.delay)
			}
			if got := retryAfterDeadline(tc.values, now); !got.Equal(want) {
				t.Fatalf("retry deadline = %v, want %v", got, want)
			}
		})
	}
}

func TestQueuedRetryAfterSurvivesRestartAndKeepsOtherDestinationsMoving(t *testing.T) {
	for _, name := range []string{"resume at deadline", "rotate while paused", "disable while paused"} {
		t.Run(name, func(t *testing.T) {
			db := openNativeIntegrationTestDB(t, "rate-limited.db")
			now := time.Date(2026, 11, 20, 5, 0, 0, 250_000_000, time.UTC)
			deadline := now.Add(90 * time.Second)
			var clock atomic.Int64
			clock.Store(now.UnixNano())
			var original, rotated, discord, conditions atomic.Int32
			var isolated atomic.Bool
			isolationObserved := make(chan struct{}, 1)
			deps := outboxTestDeps(db, 10*time.Millisecond)
			deps.ConditionPollInterval = 10 * time.Millisecond
			deps.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
			deps.ReadConditions = func(_ context.Context, cursor string, _ []Condition) (ConditionBatch, error) {
				conditions.Add(1)
				return ConditionBatch{Cursor: cursor}, nil
			}
			deps.RestoreDeliveryBlocked = func() bool {
				if !isolated.Load() {
					return false
				}
				select {
				case isolationObserved <- struct{}{}:
				default:
				}
				return true
			}
			deps.HTTPClient = notificationHTTPFunc(func(req *http.Request) (*http.Response, error) {
				resp := &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}
				switch req.URL.Host {
				case "rate-limited.example.test":
					if original.Add(1) == 1 {
						resp.StatusCode = http.StatusTooManyRequests
						resp.Header.Set("Retry-After", "90")
					}
				case "rotated.example.test":
					rotated.Add(1)
				case "discord.com":
					discord.Add(1)
				}
				return resp, nil
			})

			configure := func(svc *Service, endpoint string) {
				t.Helper()
				_, err := svc.SaveSettings(SettingsUpdate{
					Enabled: true, WebhookURL: endpoint, WebhookURLIntent: WebhookURLReplace,
					EventTypes: []string{EventUpdateComplete},
					Discord: &DiscordUpdate{Enabled: true, WebhookURL: "https://discord.com/api/webhooks/123456/private",
						WebhookURLIntent: WebhookURLReplace},
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			enqueue := func(svc *Service, id string) {
				t.Helper()
				if got := svc.Accept(DeliveryIntent{ID: id, Action: EventUpdateComplete, TargetName: "host"}); got.State != AdmissionAdmitted {
					t.Fatalf("enqueue %s: %+v", id, got)
				}
			}

			first := NewService(deps)
			t.Cleanup(func() { closeOutboxService(t, first) })
			first.StartConditionMonitoring()
			configure(first, "https://rate-limited.example.test/private-path")
			if name == "rotate while paused" {
				// An older database can have an unbound row. Its HTTP 429
				// must bind the retry to the credentials that were used.
				insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "first", Action: EventUpdateComplete, TargetName: "host"}, now)
			}
			enqueue(first, "first")
			waitForRateLimitRow(t, db, "first", DestinationWebhook, outboxStateRetrying)
			var due string
			if err := db.QueryRow("SELECT next_attempt_at FROM notification_outbox WHERE id=?",
				outboxDeliveryID("first", DestinationWebhook)).Scan(&due); err != nil || due != deadline.Format(time.RFC3339Nano) {
				t.Fatalf("persisted row deadline = %q, %v", due, err)
			}
			before := conditions.Load()
			enqueue(first, "second")
			waitForRateLimitRow(t, db, "second", DestinationDiscord, outboxStateSucceeded)
			evaluationDeadline := time.Now().Add(time.Second)
			for conditions.Load() <= before {
				if time.Now().After(evaluationDeadline) {
					t.Fatal("condition evaluation stalled during the pause")
				}
				time.Sleep(time.Millisecond)
			}
			if at, scheduled, err := nextOutboxWake(t.Context(), db, deps.ClaimTTL, outboxDeliveries{}); err != nil || !scheduled || !at.Equal(deadline) {
				t.Fatalf("next wake = %v, scheduled=%v, err=%v, want %v", at, scheduled, err, deadline)
			}
			closeOutboxService(t, first)
			if original.Load() != 1 || discord.Load() != 2 {
				t.Fatalf("during pause: webhook calls=%d, Discord calls=%d", original.Load(), discord.Load())
			}

			clock.Store(deadline.Add(-10 * time.Second).UnixNano())
			reopened := NewService(deps)
			t.Cleanup(func() { closeOutboxService(t, reopened) })
			diagnostics, err := reopened.DeliveryDiagnostics()
			if err != nil || !diagnostics.RateLimitedUntil[DestinationWebhook].Equal(deadline) || len(diagnostics.RateLimitedUntil) != 1 {
				t.Fatalf("restarted diagnostics = %+v, %v", diagnostics, err)
			}
			switch name {
			case "rotate while paused":
				configure(reopened, "https://rotated.example.test/private-path")
				waitForRateLimitRow(t, db, "first", DestinationWebhook, outboxStateSkipped)
				waitForRateLimitRow(t, db, "second", DestinationWebhook, outboxStateSkipped)
				enqueue(reopened, "after-rotation")
				waitForRateLimitRow(t, db, "after-rotation", DestinationWebhook, outboxStateSucceeded)
				if original.Load() != 1 || rotated.Load() != 1 {
					t.Fatalf("rotation: old calls=%d, new calls=%d", original.Load(), rotated.Load())
				}
			case "disable while paused":
				if _, err := reopened.SaveSettings(SettingsUpdate{Enabled: false, EventTypes: []string{EventUpdateComplete}}); err != nil {
					t.Fatal(err)
				}
				waitForRateLimitRow(t, db, "first", DestinationWebhook, outboxStateSkipped)
				waitForRateLimitRow(t, db, "second", DestinationWebhook, outboxStateSkipped)
				if original.Load() != 1 {
					t.Fatalf("disabled destination sent another request: %d calls", original.Load())
				}
			default:
				// Another destination's delivery gives the restarted worker a
				// full poll cycle before we release the isolated queue.
				enqueue(reopened, "after-restart")
				waitForRateLimitRow(t, db, "after-restart", DestinationDiscord, outboxStateSucceeded)
				if original.Load() != 1 {
					t.Fatalf("worker ignored the persisted deadline after restart: %d calls", original.Load())
				}
				isolated.Store(true)
				clock.Store(deadline.UnixNano())
				enqueue(reopened, "while-isolated")
				select {
				case <-isolationObserved:
				case <-time.After(time.Second):
					t.Fatal("worker did not observe restore isolation")
				}
				if original.Load() != 1 {
					t.Fatalf("worker delivered during isolation: %d calls", original.Load())
				}
				isolated.Store(false)
				reopened.signalWorker()
				waitForRateLimitRow(t, db, "first", DestinationWebhook, outboxStateSucceeded)
				waitForRateLimitRow(t, db, "while-isolated", DestinationWebhook, outboxStateSucceeded)
				if original.Load() != 5 {
					t.Fatalf("queue did not drain after the deadline: %d webhook calls", original.Load())
				}
			}
			cleared, err := reopened.DeliveryDiagnostics()
			if err != nil || len(cleared.RateLimitedUntil) != 0 {
				t.Fatalf("pause survived expiry or configuration rotation: %+v, %v", cleared, err)
			}
		})
	}
}

func TestTerminalRateLimitUsesBackoffAndSurvivesOfflineRecovery(t *testing.T) {
	h := newConditionHarness(t)
	h.service.deps.DeliveryTimeout = time.Second
	h.service.deps.Backoff = func(int) time.Duration { return 7 * time.Second }
	h.service.deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"https://response.example/private-header"}},
			Body:       io.NopCloser(strings.NewReader("private response body")),
		}, nil
	})
	last := insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "final-attempt", Action: EventUpdateComplete}, h.now)
	if _, err := h.db.Exec("UPDATE notification_outbox SET attempts=? WHERE id=?", defaultAttempts-1, last); err != nil {
		t.Fatal(err)
	}
	row, err := claimNextOutboxRow(t.Context(), h.db, h.now, outboxDeliveries{})
	if err != nil || row == nil || !h.service.processOutboxRow(t.Context(), *row) {
		t.Fatalf("process terminal attempt: %+v, %v", row, err)
	}
	diagnostics, err := h.service.DeliveryDiagnostics()
	deadline := h.now.Add(7 * time.Second)
	if err != nil || diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Outcome != DeliveryOutcomeFailed ||
		diagnostics.LastAttempt.Attempts != defaultAttempts || diagnostics.LastAttempt.NextRetryAt != "" ||
		diagnostics.LastAttempt.StatusCode != http.StatusTooManyRequests ||
		!diagnostics.RateLimitedUntil[DestinationWebhook].Equal(deadline) {
		t.Fatalf("terminal diagnostics = %+v (%v)", diagnostics, err)
	}
	raw, err := json.Marshal(diagnostics)
	if err != nil || strings.Contains(string(raw), "private") {
		t.Fatalf("diagnostics disclosed raw HTTP details: %s (%v)", raw, err)
	}

	archive := newConditionHarness(t)
	id := insertPendingOutboxIntent(t, archive.db, DeliveryIntent{ID: "other-webhook", Action: EventUpdateComplete}, h.now)
	if err := MergeOfflineDeliveryEvidence(t.Context(), h.db, archive.db, h.now); err != nil {
		t.Fatal(err)
	}
	if row, err := claimNextOutboxRow(t.Context(), archive.db, deadline.Add(-time.Nanosecond), outboxDeliveries{}); err != nil || row != nil {
		t.Fatalf("restored legacy row skipped active cooldown: %+v, %v", row, err)
	}
	if row, err := claimNextOutboxRow(t.Context(), archive.db, deadline, outboxDeliveries{}); err != nil || row == nil || row.ID != id || row.Attempts != 0 {
		t.Fatalf("restored row did not become eligible at deadline: %+v, %v", row, err)
	}
}

func TestRateLimitedLaneRepairsFailedCompletionBeforeNextDelivery(t *testing.T) {
	for _, name := range []string{"resume at deadline", "revalidate immediately after rotation", "reclaim original row after rotation", "flush after a released claim"} {
		t.Run(name, func(t *testing.T) {
			testRateLimitedLaneRepairsFailedCompletion(t, name)
		})
	}
}

func testRateLimitedLaneRepairsFailedCompletion(t *testing.T, scenario string) {
	t.Helper()
	const retryAfter = 5 * time.Minute
	h := newConditionHarness(t)
	h.service.deps.DeliveryTimeout = time.Second
	h.service.deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
		h.client.calls++
		resp := &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}
		if h.client.calls == 1 {
			resp.StatusCode = http.StatusTooManyRequests
			resp.Header.Set("Retry-After", "300")
		}
		return resp, nil
	})

	first := insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "first-rate-limited", Action: EventUpdateComplete}, h.now)
	const nextIntent = "next-while-paused"
	if result := h.service.Accept(DeliveryIntent{ID: nextIntent, Action: EventUpdateComplete}); result.State != AdmissionAdmitted {
		t.Fatalf("queue next notification: %+v", result)
	}
	next := outboxDeliveryID(nextIntent, DestinationWebhook)
	if _, err := h.db.Exec(`CREATE TRIGGER reject_rate_limit_completion BEFORE UPDATE OF status_code ON notification_outbox
		WHEN new.status_code=429 BEGIN SELECT RAISE(ABORT, 'temporary persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	claim := func(want string) outboxRow {
		t.Helper()
		row, err := claimNextOutboxRow(t.Context(), h.db, h.now, outboxDeliveries{})
		if err != nil || row == nil || row.ID != want {
			t.Fatalf("claim = %+v, %v, want %s", row, err, want)
		}
		return *row
	}
	if h.service.processOutboxRow(t.Context(), claim(first)) {
		t.Fatal("completion succeeded while persistence rejected it")
	}

	if scenario == "flush after a released claim" {
		h.now = h.now.Add(defaultOutboxClaimTTL)
		if err := recoverExpiredOutboxClaims(t.Context(), h.db, h.now, defaultOutboxClaimTTL, outboxDeliveries{}); err != nil {
			t.Fatal(err)
		}
		h.now = h.now.Add(indeterminateRetryDelay)
		h.service.processOutboxRow(t.Context(), claim(first))
	}
	if _, err := h.db.Exec("DROP TRIGGER reject_rate_limit_completion"); err != nil {
		t.Fatal(err)
	}
	if scenario == "flush after a released claim" {
		h.service.flushRateLimitCompletions(t.Context())
		diagnostics, err := h.service.DeliveryDiagnostics()
		want := h.now.Add(retryAfter - defaultOutboxClaimTTL - indeterminateRetryDelay)
		if err != nil || !diagnostics.RateLimitedUntil[DestinationWebhook].Equal(want) {
			t.Fatalf("flush after claim recovery lost the observed deadline: %+v, %v", diagnostics, err)
		}
		if row, err := claimNextOutboxRow(t.Context(), h.db, h.now, outboxDeliveries{}); err != nil || row != nil || h.client.calls != 1 {
			t.Fatalf("a delivery became eligible before the recovered deadline: %+v, %v with %d requests", row, err, h.client.calls)
		}
		return
	}

	rotate := func() {
		t.Helper()
		_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, WebhookURLIntent: WebhookURLReplace,
			WebhookURL: "https://rotated.example.test/private-path", EventTypes: []string{EventUpdateComplete}})
		if err != nil {
			t.Fatal(err)
		}
	}
	assertSkippedAfterRotation := func(id string) {
		t.Helper()
		var state string
		if err := h.db.QueryRow("SELECT state FROM notification_outbox WHERE id=?", id).Scan(&state); err != nil ||
			state != outboxStateSkipped || h.client.calls != 1 {
			t.Fatalf("notification after rotation = %q with %d deliveries, %v", state, h.client.calls, err)
		}
	}
	if scenario == "reclaim original row after rotation" {
		rotate()
		h.now = h.now.Add(defaultOutboxClaimTTL)
		if err := recoverExpiredOutboxClaims(t.Context(), h.db, h.now, defaultOutboxClaimTTL, outboxDeliveries{}); err != nil {
			t.Fatal(err)
		}
		h.drain(t)
		assertSkippedAfterRotation(first)
		return
	}

	h.service.processOutboxRow(t.Context(), claim(next))
	if h.client.calls != 1 {
		t.Fatalf("same destination sent %d requests before its deadline, want 1", h.client.calls)
	}
	var state string
	var attempts int
	if err := h.db.QueryRow("SELECT state, attempts FROM notification_outbox WHERE id=?", first).Scan(&state, &attempts); err != nil ||
		state != outboxStateRetrying || attempts != 1 {
		t.Fatalf("recovered response = %q with %d attempts, %v", state, attempts, err)
	}

	if scenario == "revalidate immediately after rotation" {
		rotate()
		h.drain(t)
		assertSkippedAfterRotation(next)
		return
	}
	h.now = h.now.Add(retryAfter)
	h.drain(t)
	if h.client.calls != 3 {
		t.Fatalf("queued deliveries did not resume at the persisted deadline, got %d calls", h.client.calls)
	}
}

func TestGracefulRestartPersistsRateLimitAfterTemporaryWriteFailure(t *testing.T) {
	db := openNativeIntegrationTestDB(t, "rate-limit-close.db")
	prepareOutboxSettings(t, db, "https://hooks.example.test/test", true)
	now := time.Date(2026, 11, 20, 5, 0, 0, 0, time.UTC)
	deps := outboxTestDeps(db, time.Second)
	deps.Now = func() time.Time { return now }
	deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusTooManyRequests,
			Header:     http.Header{"Retry-After": {"90"}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})
	failed := make(chan struct{}, 1)
	deps.Logf = func(format string, _ ...any) {
		if strings.HasPrefix(format, "notification outbox outcome persistence failed") {
			failed <- struct{}{}
		}
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_rate_limit_completion BEFORE UPDATE OF status_code ON notification_outbox
		WHEN new.status_code=429 BEGIN SELECT RAISE(ABORT, 'temporary persistence failure'); END`); err != nil {
		t.Fatal(err)
	}
	first := NewService(deps)
	t.Cleanup(func() { closeOutboxService(t, first) })
	if got := first.Accept(DeliveryIntent{ID: "failed-before-close", Action: EventUpdateComplete}); got.State != AdmissionAdmitted {
		t.Fatalf("enqueue notification: %+v", got)
	}
	select {
	case <-failed:
	case <-time.After(time.Second):
		t.Fatal("worker did not report the failed write")
	}
	diagnostics, err := first.DeliveryDiagnostics()
	if err != nil || !diagnostics.RateLimitedUntil[DestinationWebhook].Equal(now.Add(90*time.Second)) {
		t.Errorf("live diagnostics omitted the known pause during a write failure: %+v, %v", diagnostics, err)
	}
	if _, err := db.Exec("DROP TRIGGER reject_rate_limit_completion"); err != nil {
		t.Fatal(err)
	}
	closeOutboxService(t, first)

	reopened := NewService(deps)
	defer closeOutboxService(t, reopened)
	diagnostics, err = reopened.DeliveryDiagnostics()
	if err != nil || !diagnostics.RateLimitedUntil[DestinationWebhook].Equal(now.Add(90*time.Second)) {
		t.Fatalf("reopened delivery diagnostics lost observed pause: %+v, %v", diagnostics, err)
	}
}

func TestExpiredRateLimitRetainsFailedTerminalAttempt(t *testing.T) {
	for _, header := range []string{"300", ""} {
		name := "write failure outlasts retry delay"
		if header == "" {
			name = "retry deadline already reached"
		}
		t.Run(name, func(t *testing.T) {
			h := newConditionHarness(t)
			h.service.deps.DeliveryTimeout = time.Second
			h.service.deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
				h.client.calls++
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {header}},
					Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			id := insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "failed-final-attempt", Action: EventUpdateComplete}, h.now)
			if _, err := h.db.Exec("UPDATE notification_outbox SET attempts=? WHERE id=?", defaultAttempts-1, id); err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Exec(`CREATE TRIGGER reject_terminal_rate_limit BEFORE UPDATE OF status_code ON notification_outbox
				WHEN new.status_code=429 BEGIN SELECT RAISE(ABORT, 'temporary persistence failure'); END`); err != nil {
				t.Fatal(err)
			}
			h.drain(t)
			if h.client.calls != 1 {
				t.Fatalf("expected one initial HTTP attempt, got %d", h.client.calls)
			}

			h.now = h.now.Add(5 * time.Minute)
			if err := recoverExpiredOutboxClaims(t.Context(), h.db, h.now, defaultOutboxClaimTTL, outboxDeliveries{}); err != nil {
				t.Fatal(err)
			}
			h.drain(t)
			if h.client.calls != 1 {
				t.Errorf("sent %d HTTP requests despite exhausting the attempt budget before persistence recovered", h.client.calls)
			}

			if _, err := h.db.Exec("DROP TRIGGER reject_terminal_rate_limit"); err != nil {
				t.Fatal(err)
			}
			h.now = h.now.Add(time.Hour)
			if err := recoverExpiredOutboxClaims(t.Context(), h.db, h.now, defaultOutboxClaimTTL, outboxDeliveries{}); err != nil {
				t.Fatal(err)
			}
			h.drain(t)
			var state string
			var attempts int
			if err := h.db.QueryRow("SELECT state, attempts FROM notification_outbox WHERE id=?", id).Scan(&state, &attempts); err != nil ||
				state != outboxStateFailed || attempts != defaultAttempts || h.client.calls != 1 {
				t.Fatalf("terminal state=%q attempts=%d HTTP requests=%d after recovery, err=%v", state, attempts, h.client.calls, err)
			}
		})
	}
}

func waitForRateLimitRow(t *testing.T, db *sql.DB, intent, destination, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var state string
	var err error
	for time.Now().Before(deadline) {
		err = db.QueryRow("SELECT state FROM notification_outbox WHERE id=?", outboxDeliveryID(intent, destination)).Scan(&state)
		if err == nil && state == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s %s state = %q, %v, want %q", intent, destination, state, err, want)
}
