package notifications

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestNotificationWorkerReportsFailedEvaluationWithoutProgress(t *testing.T) {
	for _, tc := range []struct {
		name                string
		failsAtRevalidation bool
	}{
		{name: "initial sweep"},
		{name: "before sending admitted delivery", failsAtRevalidation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConditionHarness(t)
			h.db.SetMaxOpenConns(1)
			deps := h.service.deps
			deps.ConditionPollInterval = time.Hour
			start := h.now
			next := start.Add(time.Minute)
			var failCycle atomic.Bool
			deps.Now = func() time.Time {
				if failCycle.Load() {
					return next
				}
				return start
			}
			failureCycleReads := 0
			deps.ReadConditions = func(context.Context, string, []Condition) (ConditionBatch, error) {
				if !failCycle.Load() {
					return ConditionBatch{}, nil
				}
				failureCycleReads++
				if tc.failsAtRevalidation && failureCycleReads == 1 {
					return ConditionBatch{Facts: []ConditionFact{rebootFact(true)}}, nil
				}
				return ConditionBatch{}, errors.New("condition source unavailable")
			}
			service := NewService(deps)
			t.Cleanup(func() { closeOutboxService(t, service) })
			service.StartConditionMonitoring()
			for deadline := time.Now().Add(5 * time.Second); ; {
				worker, facts, err := service.InstallationHealth(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if worker.LastProgress.Equal(start) && facts.ConditionEvaluationState == ConditionEvaluationHealthy {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("first cycle = %+v, %+v", worker, facts)
				}
				time.Sleep(5 * time.Millisecond)
			}
			failCycle.Store(true)
			service.signalWorker()
			for deadline := time.Now().Add(5 * time.Second); ; {
				worker, facts, err := service.InstallationHealth(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				if worker.LastTick.Equal(next) && !worker.Running {
					if !worker.LastProgress.Equal(start) || worker.Errors != 1 || facts.ConditionEvaluationState != ConditionEvaluationDegraded {
						t.Fatalf("failed cycle = %+v, %+v", worker, facts)
					}
					wantLastSuccess := start
					if tc.failsAtRevalidation {
						wantLastSuccess = next
						if facts.Retrying != 1 || facts.LastSuccessfulDeliveryAt != nil {
							t.Fatalf("unvalidated delivery result = %+v", facts)
						}
					}
					if facts.LastConditionEvaluationAt == nil || !facts.LastConditionEvaluationAt.Equal(wantLastSuccess) {
						t.Fatalf("last successful evaluation = %v, want %v", facts.LastConditionEvaluationAt, wantLastSuccess)
					}
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("failed cycle did not complete = %+v, %+v", worker, facts)
				}
				time.Sleep(5 * time.Millisecond)
			}
		})
	}
}

func TestInstallationHealthTracksConditionEvaluationSeparately(t *testing.T) {
	h := newConditionHarness(t)
	ctx := t.Context()
	check := func(state string, lastSuccess time.Time) {
		t.Helper()
		_, facts, err := h.service.InstallationHealth(ctx)
		if err != nil || facts.ConditionEvaluationState != state || facts.LastSuccessfulDeliveryAt != nil {
			t.Fatalf("condition evaluation facts = %+v, error = %v", facts, err)
		}
		actual := facts.LastConditionEvaluationAt
		if lastSuccess.IsZero() && actual != nil || !lastSuccess.IsZero() && (actual == nil || !actual.Equal(lastSuccess)) {
			t.Fatalf("last condition success = %v, want %v", actual, lastSuccess)
		}
	}
	check("not_observed", time.Time{})
	first := h.now
	h.reconcile(t)
	check(ConditionEvaluationHealthy, first)

	h.now = h.now.Add(time.Minute)
	h.sourceErr = errors.New("private condition source error")
	if err := h.service.reconcileConditions(ctx); err == nil {
		t.Fatal("condition source failure was ignored")
	}
	check(ConditionEvaluationDegraded, first)

	h.now = h.now.Add(time.Minute)
	h.sourceErr = nil
	h.facts = []ConditionFact{rebootFact(true)}
	if _, err := h.db.Exec(`CREATE TRIGGER block_conditions BEFORE INSERT ON notification_conditions
		BEGIN SELECT RAISE(ABORT, 'condition write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := h.service.reconcileConditions(ctx); err == nil {
		t.Fatal("condition transaction failure was ignored")
	}
	check(ConditionEvaluationDegraded, first)
	if _, err := h.db.Exec("DROP TRIGGER block_conditions"); err != nil {
		t.Fatal(err)
	}
	h.now = h.now.Add(time.Minute)
	h.reconcile(t)
	check(ConditionEvaluationHealthy, h.now)
}

func TestInstallationHealthPersistsOnlySuccessfulHTTPDelivery(t *testing.T) {
	h := newConditionHarness(t)
	ctx := t.Context()
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.client.fail = true
	row, err := claimNextOutboxRow(ctx, h.db, h.now)
	if err != nil || row == nil {
		t.Fatalf("claim = %v, %v", row, err)
	}
	h.service.processOutboxRow(ctx, *row)
	h.now = h.now.Add(30 * time.Second)
	_, facts, err := h.service.InstallationHealth(ctx)
	if err != nil || facts.ConditionEvaluationState != ConditionEvaluationHealthy || facts.Retrying != 1 || facts.LastSuccessfulDeliveryAt != nil ||
		facts.OldestPendingAgeSeconds == nil || *facts.OldestPendingAgeSeconds != 30 {
		t.Fatalf("after failed HTTP delivery = %+v, %v", facts, err)
	}

	h.client.fail = false
	h.drain(t)
	success := h.now
	_, facts, err = h.service.InstallationHealth(ctx)
	if err != nil || facts.LastSuccessfulDeliveryAt == nil || !facts.LastSuccessfulDeliveryAt.Equal(success) ||
		facts.OldestPendingAgeSeconds != nil || facts.Retrying != 0 {
		t.Fatalf("after successful HTTP delivery = %+v, %v", facts, err)
	}
	h.now = h.now.Add(2 * time.Hour)
	if n, err := pruneOutbox(ctx, h.db, h.now, time.Hour); n == 0 || err != nil {
		t.Fatalf("pruned %d rows, error = %v", n, err)
	}

	restarted := &Service{deps: h.service.deps}
	if err := EnsureSchema(h.db); err != nil {
		t.Fatal(err)
	}
	_, facts, err = restarted.InstallationHealth(ctx)
	if err != nil || facts.LastSuccessfulDeliveryAt == nil || !facts.LastSuccessfulDeliveryAt.Equal(success) ||
		facts.OldestPendingAgeSeconds != nil || facts.ConditionEvaluationState != "not_observed" || facts.LastConditionEvaluationAt != nil {
		t.Fatalf("after restart = %+v, %v", facts, err)
	}
}

func TestInstallationHealthMeasuresOldestNonterminalDelivery(t *testing.T) {
	h := newConditionHarness(t)
	for _, f := range []struct {
		state string
		at    time.Time
	}{
		{outboxStateSucceeded, h.now.Add(-time.Hour)},
		{outboxStatePending, h.now.Add(-time.Second)},
		{outboxStateClaimed, h.now.Add(-2 * time.Second)},
		{outboxStateRetrying, h.now.Add(-3*time.Second + 200*time.Millisecond)},
	} {
		created := f.at.Format(time.RFC3339Nano)
		if _, err := h.db.Exec(`INSERT INTO notification_outbox(id,destination,event_type,action,state,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?)`, f.state, DestinationWebhook, EventRebootRequired, EventRebootRequired, f.state, created, created); err != nil {
			t.Fatal(err)
		}
	}
	_, facts, err := h.service.InstallationHealth(t.Context())
	if err != nil || facts.OldestPendingAgeSeconds == nil || *facts.OldestPendingAgeSeconds != 2.8 || facts.Pending != 2 || facts.Retrying != 1 {
		t.Fatalf("mixed states = %+v, %v", facts, err)
	}
}

func TestInstallationHealthBackfillsLegacyHTTPDelivery(t *testing.T) {
	h := newConditionHarness(t)
	now := h.now.Format(time.RFC3339Nano)
	latestSuccess := h.now.Add(250 * time.Millisecond)
	for _, f := range []struct {
		id, state, completed string
		status               int
	}{
		{"old-success", outboxStateSucceeded, now, 204},
		{"new-success", outboxStateSucceeded, latestSuccess.Format(time.RFC3339Nano), 201},
		{"latest-error", outboxStateFailed, h.now.Add(time.Second).Format(time.RFC3339Nano), 500},
		{"no-http-result", outboxStateSucceeded, h.now.Add(2 * time.Second).Format(time.RFC3339Nano), 0},
	} {
		if _, err := h.db.Exec(`INSERT INTO notification_outbox(id,destination,event_type,action,state,created_at,updated_at,completed_at,status_code)
			VALUES(?,?,?,?,?,?,?,?,?)`, f.id, DestinationWebhook, EventRebootRequired, EventRebootRequired, f.state, now, now, f.completed, f.status); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec("DROP TABLE notification_delivery_health"); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(h.db); err != nil {
		t.Fatal(err)
	}
	_, facts, err := h.service.InstallationHealth(t.Context())
	if err != nil || facts.LastSuccessfulDeliveryAt == nil || !facts.LastSuccessfulDeliveryAt.Equal(latestSuccess) ||
		facts.OldestPendingAgeSeconds != nil {
		t.Fatalf("legacy queued deliveries = %+v, %v", facts, err)
	}
}

func TestInstallationHealthDeliveryAndSuccessTimestampCommitTogether(t *testing.T) {
	h := newConditionHarness(t)
	now := h.now.Format(time.RFC3339Nano)
	id := insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "atomic-success", Action: EventUpdateComplete}, h.now)
	if row, err := claimNextOutboxRow(t.Context(), h.db, h.now); err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %v, %v", row, err)
	}
	if _, err := h.db.Exec(`CREATE TRIGGER block_success BEFORE UPDATE ON notification_delivery_health
		BEGIN SELECT RAISE(ABORT, 'success write failed'); END`); err != nil {
		t.Fatal(err)
	}
	outcome := outboxOutcome{State: outboxStateSucceeded, StatusCode: 204, CompletedAt: now}
	if err := completeOutboxRow(t.Context(), h.db, id, outcome, now); err == nil {
		t.Fatal("delivery completed without persisting the success timestamp")
	}
	var persisted string
	if err := h.db.QueryRow("SELECT state FROM notification_outbox WHERE id=?", id).Scan(&persisted); err != nil || persisted != outboxStateClaimed {
		t.Fatalf("outbox state = %v, %v", persisted, err)
	}
}
