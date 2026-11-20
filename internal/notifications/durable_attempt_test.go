package notifications

import (
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
			deps.Now = func() time.Time { return now }
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
				wantNext = now.Add(indeterminateRetryDelay).Format(time.RFC3339Nano)
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
	for _, remaining := range []int{0, 1} {
		t.Run(map[int]string{0: "exhausted", 1: "can retry"}[remaining], func(t *testing.T) {
			live, stage := openOutboxTestDB(t, "live.db"), openOutboxTestDB(t, "archive.db")
			endpoint := "https://hooks.example.test/restore"
			intent := DeliveryIntent{ID: "offline-attempt", Action: EventUpdateComplete}
			for _, db := range []*sql.DB{live, stage} {
				prepareOutboxSettings(t, db, endpoint, true)
				insertPendingOutboxIntent(t, db, intent, now)
			}
			id := outboxDeliveryID(intent.ID, DestinationWebhook)
			before := defaultAttempts - remaining - 1
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
			wantState := outboxStateRetrying
			if remaining == 0 {
				wantState = outboxStateFailed
			}
			if state != wantState || reason != outboxIndeterminateReason || attempts != before+1 ||
				lastIndeterminate != now.Format(time.RFC3339Nano) {
				t.Fatalf("restored result = %s %s after %d attempts at %s", state, reason, attempts, lastIndeterminate)
			}
		})
	}
}
