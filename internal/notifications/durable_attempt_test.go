package notifications

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotificationAttemptSurvivesProcessLoss(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	const interrupted = 73
	if path := os.Getenv("SLU_ATTEMPT_TEST_DB"); path != "" {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		deps := outboxTestDeps(db, time.Hour)
		deps.Now = func() time.Time { return now }
		deps.DeliveryTimeout = 5 * time.Second
		deps.HTTPClient = notificationHTTPFunc(func(req *http.Request) (*http.Response, error) {
			var attempts int
			if err := db.QueryRow(`SELECT attempts FROM notification_outbox
				WHERE attempt_id <> '' AND completed_at = ''`).Scan(&attempts); err != nil || attempts == 0 {
				t.Fatalf("HTTP called without a committed attempt: %d, %v", attempts, err)
			}
			if os.Getenv("SLU_ATTEMPT_TEST_POINT") == "after response" {
				response, err := http.DefaultClient.Do(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := response.Body.Close(); err != nil {
					t.Fatal(err)
				}
			}
			os.Exit(interrupted)
			return nil, nil
		})
		service := &Service{deps: deps}
		row, err := claimNextOutboxRow(t.Context(), db, now, outboxDeliveries{})
		if err != nil || row == nil {
			t.Fatalf("fixture claim = %+v, %v", row, err)
		}
		service.processOutboxRow(t.Context(), *row)
		t.Fatal("fixture did not reach HTTP transport")
	}
	for _, tc := range []struct {
		point     string
		before    int
		requests  int32
		wantState string
	}{
		{"before request", 0, 0, outboxStateRetrying},
		{"after response", defaultAttempts - 1, 1, outboxStateFailed},
	} {
		t.Run(tc.point, func(t *testing.T) {
			var requests atomic.Int32
			receiver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Retry-After", "90")
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			defer receiver.Close()
			path := filepath.Join(t.TempDir(), "outbox.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			prepareOutboxSettings(t, db, receiver.URL, true)
			id := insertPendingOutboxIntent(t, db, DeliveryIntent{ID: "crash-attempt", Action: EventUpdateComplete}, now)
			if _, err := db.Exec("UPDATE notification_outbox SET attempts=? WHERE id=?", tc.before, id); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestNotificationAttemptSurvivesProcessLoss$")
			cmd.Env = append(os.Environ(), "SLU_ATTEMPT_TEST_DB="+path, "SLU_ATTEMPT_TEST_POINT="+tc.point)
			result, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != interrupted {
				t.Fatalf("fixture exit = %v\n%s", err, result)
			}
			if got := requests.Load(); got != tc.requests {
				t.Fatalf("received %d requests, want %d", got, tc.requests)
			}
			deps := outboxTestDeps(db, time.Hour)
			restartTime := now.Add(-time.Minute)
			deps.Now = func() time.Time { return restartTime }
			deps.RestoreDeliveryBlocked = func() bool { return true }
			restarted := NewService(deps)
			defer closeOutboxService(t, restarted)
			page, err := restarted.DeliveryHistory(t.Context(), DeliveryHistoryQuery{})
			if err != nil || len(page.Items) != 1 {
				t.Fatalf("history = %+v, %v", page, err)
			}
			item := page.Items[0]
			if item.Attempts != tc.before+1 || item.State != tc.wantState ||
				item.ReasonCode != outboxIndeterminateReason || item.LastIndeterminateAt != now.Format(time.RFC3339Nano) {
				t.Fatalf("recovered attempt = %+v", item)
			}
			wantNext := ""
			if tc.wantState == outboxStateRetrying {
				wantNext = restartTime.Add(indeterminateRetryDelay).Format(time.RFC3339Nano)
			}
			if item.NextAttemptAt != wantNext {
				t.Fatalf("next retry = %q, want %q", item.NextAttemptAt, wantNext)
			}
		})
	}
}

func TestNotificationAdmissionFailurePreventsRequest(t *testing.T) {
	h := newConditionHarness(t)
	insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "cannot-save-attempt", Action: EventUpdateComplete}, h.now)
	if _, err := h.db.Exec(`CREATE TRIGGER refuse_attempt BEFORE UPDATE OF attempt_id ON notification_outbox
		WHEN NEW.attempt_id <> '' BEGIN SELECT RAISE(ABORT, 'attempt storage unavailable'); END`); err != nil {
		t.Fatal(err)
	}
	row, err := claimNextOutboxRow(t.Context(), h.db, h.now, outboxDeliveries{})
	if err != nil || row == nil {
		t.Fatalf("fixture claim = %+v, %v", row, err)
	}
	if h.service.processOutboxRow(t.Context(), *row) || h.client.calls != 0 {
		t.Fatalf("request was admitted with a failed persistence write, HTTP calls = %d", h.client.calls)
	}
	var attempts int
	if err := h.db.QueryRow("SELECT attempts FROM notification_outbox WHERE id=?", row.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts after rejected admission = %d, %v", attempts, err)
	}
}

func TestOfflineRestoreKeepsSpentNotificationAttempts(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name       string
		remaining  int
		archiveWon bool
	}{
		{"exhausted", 0, false},
		{"can retry", 1, false},
		{"archive held a recorded success", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live, stage := openOutboxTestDB(t, "live.db"), openOutboxTestDB(t, "archive.db")
			endpoint := "https://hooks.example.test/restore"
			intent := DeliveryIntent{ID: "offline-attempt", Action: EventUpdateComplete}
			for _, db := range []*sql.DB{live, stage} {
				prepareOutboxSettings(t, db, endpoint, true)
				insertPendingOutboxIntent(t, db, intent, now)
			}
			id := outboxDeliveryID(intent.ID, DestinationWebhook)
			if tc.archiveWon {
				if _, err := stage.Exec(`UPDATE notification_outbox
					SET attempts=1, state='succeeded', status_code=200, completed_at=? WHERE id=?`,
					now.Format(time.RFC3339Nano), id); err != nil {
					t.Fatal(err)
				}
			}
			before := defaultAttempts - tc.remaining - 1
			if _, err := live.Exec("UPDATE notification_outbox SET attempts=? WHERE id=?", before, id); err != nil {
				t.Fatal(err)
			}
			row, err := claimNextOutboxRow(t.Context(), live, now, outboxDeliveries{})
			if err != nil || row == nil {
				t.Fatalf("fixture claim = %+v, %v", row, err)
			}
			fingerprint := destinationConfigFingerprint(Settings{WebhookURL: endpoint}, DestinationWebhook)
			if err := admitOutboxAttempt(t.Context(), live, row, fingerprint, now); err != nil {
				t.Fatal(err)
			}
			if err := MergeOfflineDeliveryEvidence(t.Context(), live, stage, now); err != nil {
				t.Fatal(err)
			}
			var attempts int
			var state, reason, lastIndeterminate string
			if err := stage.QueryRow(`SELECT attempts, state, error, last_indeterminate_at
				FROM notification_outbox WHERE id=?`, id).Scan(&attempts, &state, &reason, &lastIndeterminate); err != nil {
				t.Fatal(err)
			}
			wantState, wantReason := outboxStateRetrying, outboxIndeterminateReason
			if tc.remaining == 0 {
				wantState = outboxStateFailed
			}
			if tc.archiveWon {
				wantState, wantReason = outboxStateSucceeded, ""
			}
			if state != wantState || reason != wantReason || attempts != before+1 ||
				lastIndeterminate != now.Format(time.RFC3339Nano) {
				t.Fatalf("restored result = %s %s after %d attempts at %s", state, reason, attempts, lastIndeterminate)
			}
		})
	}
}

func TestRestoreKeepsRecordedOutcomeForArchivedFinalAttempt(t *testing.T) {
	for _, tc := range []struct {
		name       string
		statusCode int
		swapAPT    bool
	}{
		{"live success", http.StatusNoContent, false},
		{"live rate limit", http.StatusTooManyRequests, false},
		{"archived APT rate limit", http.StatusTooManyRequests, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConditionHarness(t)
			if tc.swapAPT {
				h.facts = []ConditionFact{{
					ID: APTReconciliationConditionID("target", "interrupted"), EventType: EventAPTReconciliationRequired,
					TargetType: "server", TargetName: "node", Active: true,
				}}
				h.reconcile(t)
			} else {
				insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "archived-final-attempt", Action: EventUpdateComplete}, h.now)
			}
			if _, err := h.db.Exec("UPDATE notification_outbox SET attempts=?", defaultAttempts-1); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "archive.db")
			h.service.deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
				// Capture the database after admission and before the HTTP result.
				if _, err := h.db.Exec("VACUUM INTO ?", path); err != nil {
					t.Fatal(err)
				}
				header := make(http.Header)
				header.Set("Retry-After", "90")
				return &http.Response{StatusCode: tc.statusCode, Header: header, Body: http.NoBody}, nil
			})
			h.drain(t)
			staged, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer staged.Close()
			live, restored := h.db, staged
			if tc.swapAPT {
				live, restored = staged, h.db
			}
			if err := MergeOfflineDeliveryEvidence(t.Context(), live, restored, h.now); err != nil {
				t.Fatal(err)
			}
			var state, reason string
			var attempts, recordedStatus int
			if err := restored.QueryRow(`SELECT state, attempts, status_code, error FROM notification_outbox`).
				Scan(&state, &attempts, &recordedStatus, &reason); err != nil {
				t.Fatal(err)
			}
			wantState := outboxStateSucceeded
			if tc.statusCode == http.StatusTooManyRequests {
				wantState = outboxStateFailed
			}
			if state != wantState || attempts != defaultAttempts || recordedStatus != tc.statusCode || reason == outboxIndeterminateReason {
				t.Fatalf("restored outcome = %s, %d attempts, HTTP %d, reason %q", state, attempts, recordedStatus, reason)
			}
		})
	}
}

func TestRestoreKeepsArchivedRateLimitForInterruptedRetry(t *testing.T) {
	h := newConditionHarness(t)
	insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "rate-limited-archive", Action: EventUpdateComplete}, h.now)
	path := filepath.Join(t.TempDir(), "interrupted.db")
	h.service.deps.HTTPClient = notificationHTTPFunc(func(*http.Request) (*http.Response, error) {
		if _, err := h.db.Exec("VACUUM INTO ?", path); err != nil {
			t.Fatal(err)
		}
		header := make(http.Header)
		header.Set("Retry-After", "90")
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: header, Body: http.NoBody}, nil
	})
	h.drain(t)
	interrupted, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer interrupted.Close()
	if err := MergeOfflineDeliveryEvidence(t.Context(), interrupted, h.db, h.now); err != nil {
		t.Fatal(err)
	}
	history, err := h.service.DeliveryHistory(t.Context(), DeliveryHistoryQuery{})
	if err != nil || len(history.Items) != 1 {
		t.Fatalf("restored history = %+v, %v", history, err)
	}
	item := history.Items[0]
	if item.State != outboxStateRetrying || item.Attempts != 1 || item.Reason != "Destination returned HTTP 429." ||
		item.NextAttemptAt != h.now.Add(90*time.Second).Format(time.RFC3339Nano) {
		t.Fatalf("restored rate limit = %+v", item)
	}
}

func TestQueuedConditionRotationBeforeAdmissionPreventsStaleRequest(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	row, err := claimNextOutboxRow(t.Context(), h.db, h.now, outboxDeliveries{})
	if err != nil || row == nil {
		t.Fatalf("fixture claim = %+v, %v", row, err)
	}
	h.service.deps.ReadConditions = func(_ context.Context, cursor string, _ []Condition) (ConditionBatch, error) {
		_, err := h.service.SaveSettings(SettingsUpdate{
			Enabled: true, WebhookURLIntent: WebhookURLReplace, WebhookURL: "https://rotated.example.test/hooks",
			EventTypes: SupportedEvents(),
		})
		return ConditionBatch{Cursor: cursor, Facts: h.facts}, err
	}
	h.service.processOutboxRow(t.Context(), *row)
	if h.client.calls != 0 {
		t.Fatalf("sent %d requests to credentials rotated before admission", h.client.calls)
	}
	var attempts int
	if err := h.db.QueryRow("SELECT attempts FROM notification_outbox WHERE id=?", row.ID).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("attempts for stale credentials = %d, %v", attempts, err)
	}
}

func TestIndeterminateRetryDelaySurvivesDestinationRotation(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	ctx := t.Context()
	obsolete, err := claimNextOutboxRow(ctx, h.db, h.now, outboxDeliveries{})
	if err != nil || obsolete == nil {
		t.Fatalf("initial claim = %+v, %v", obsolete, err)
	}
	if err := admitOutboxAttempt(ctx, h.db, obsolete, obsolete.DestinationFingerprint, h.now); err != nil {
		t.Fatal(err)
	}
	if err := recoverExpiredOutboxClaims(ctx, h.db, h.now, 0, outboxDeliveries{}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.SaveSettings(SettingsUpdate{
		Enabled: true, WebhookURLIntent: WebhookURLReplace, WebhookURL: "https://rotated.example.test/hooks",
		EventTypes: SupportedEvents(),
	}); err != nil {
		t.Fatal(err)
	}
	newIntent := DeliveryIntent{ID: "rotated-destination", Action: EventUpdateComplete}
	if got := h.service.Accept(newIntent); got.State != AdmissionAdmitted {
		t.Fatalf("queue for rotated destination: %+v", got)
	}
	current, err := claimNextOutboxRow(ctx, h.db, h.now, outboxDeliveries{})
	if err != nil || current == nil || current.ID != outboxDeliveryID(newIntent.ID, DestinationWebhook) {
		t.Fatalf("current destination claim = %+v, %v", current, err)
	}
	h.service.processOutboxRow(ctx, *current)
	if h.client.calls != 1 {
		t.Fatalf("requests to current destination = %d, want one", h.client.calls)
	}

	h.now = h.now.Add(indeterminateRetryDelay)
	old, err := claimNextOutboxRow(ctx, h.db, h.now, outboxDeliveries{})
	if err != nil || old == nil || old.ID != obsolete.ID {
		t.Fatalf("obsolete row claim after fallback = %+v, %v", old, err)
	}
	h.sourceErr = errors.New("condition observations unavailable")
	h.service.processOutboxRow(ctx, *old)
	if row, err := claimNextOutboxRow(ctx, h.db, h.now, outboxDeliveries{}); err != nil || row != nil {
		t.Fatalf("obsolete row became eligible during revalidation backoff: %+v, %v", row, err)
	}
}

func TestStagedRestoreKeepsInterruptionFromArchivedAPTAttempt(t *testing.T) {
	live, archived := newConditionHarness(t), newConditionHarness(t)
	fact := ConditionFact{
		ID: APTReconciliationConditionID("target", "interrupted"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}
	for _, h := range []*conditionHarness{live, archived} {
		h.facts = []ConditionFact{fact}
		h.reconcile(t)
	}
	live.client.fail = true
	ctx := t.Context()
	row, err := claimNextOutboxRow(ctx, live.db, live.now, outboxDeliveries{})
	if err != nil || row == nil {
		t.Fatalf("live claim = %+v, %v", row, err)
	}
	live.service.processOutboxRow(ctx, *row)
	staged, err := claimNextOutboxRow(ctx, archived.db, archived.now, outboxDeliveries{})
	if err != nil || staged == nil {
		t.Fatalf("archive claim = %+v, %v", staged, err)
	}
	if err := admitOutboxAttempt(ctx, archived.db, staged, staged.DestinationFingerprint, archived.now); err != nil {
		t.Fatal(err)
	}
	if err := live.service.PreparePersistenceReplacement(ctx); err != nil {
		t.Fatal(err)
	}
	if err := live.service.StageAPTDeliveryEvidence(ctx, archived.db); err != nil {
		t.Fatal(err)
	}
	history, err := archived.service.DeliveryHistory(ctx, DeliveryHistoryQuery{})
	if err != nil || len(history.Items) != 1 {
		t.Fatalf("staged history = %+v, %v", history, err)
	}
	if item := history.Items[0]; item.Attempts != 1 || item.LastIndeterminateAt != archived.now.Format(time.RFC3339Nano) ||
		item.NextAttemptAt != archived.now.Add(indeterminateRetryDelay).Format(time.RFC3339Nano) {
		t.Fatalf("staged attempt = %+v", item)
	}
}
