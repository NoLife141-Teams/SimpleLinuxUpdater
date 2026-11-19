package notifications

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAPTAlertsAreOptInAndRevalidateBeforeRetry(t *testing.T) {
	h := newConditionHarness(t)
	original := ConditionFact{
		ID: APTReconciliationConditionID("target", "original"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}
	if _, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: legacyEvents}); err != nil {
		t.Fatal(err)
	}
	h.facts = []ConditionFact{original}
	h.reconcile(t)
	if h.count(t, EventAPTReconciliationRequired) != 0 {
		t.Fatal("APT notification was admitted without opting in")
	}
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.EventTypes, settings.RemindersEnabled = []string{EventAPTReconciliationRequired}, true
	settings.DiscordEnabled, settings.DiscordWebhookURL = true, "https://discord.com/api/webhooks/123/test-token"
	if err := h.service.saveSettings(settings); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if h.count(t, EventAPTReconciliationRequired) != 2 {
		t.Fatal("expected one APT notification per destination")
	}
	h.client.fail = true
	row, err := claimNextOutboxRow(t.Context(), h.db, h.now)
	if err != nil || row == nil {
		t.Fatal(row, err)
	}
	h.service.processOutboxRow(t.Context(), *row)
	if h.client.calls != 1 {
		t.Fatalf("expected one initial attempt, got %d", h.client.calls)
	}
	original.Active = false
	h.facts = []ConditionFact{original}
	h.client.fail = false
	h.now = h.now.Add(24 * time.Hour)
	h.drain(t)
	if h.client.calls != 1 {
		t.Fatalf("obsolete incident contacted a destination: %d", h.client.calls)
	}
	var skipped int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM notification_outbox WHERE event_type=? AND state=? AND error=?`,
		EventAPTReconciliationRequired, outboxStateSkipped, outboxSkipConditionIneligible).Scan(&skipped); err != nil || skipped != 2 {
		t.Fatalf("skipped APT notifications = %d, %v", skipped, err)
	}
	next := original
	next.ID, next.Active = APTReconciliationConditionID("target", "next"), true
	h.facts = []ConditionFact{next}
	h.reconcile(t)
	h.drain(t)
	h.now = h.now.Add(24 * time.Hour)
	h.reconcile(t)
	if h.client.calls != 3 || h.count(t, EventAPTReconciliationRequired) != 4 || h.count(t, EventReminder) != 0 {
		t.Fatalf("distinct incident or reminder count invalid, attempts: %d", h.client.calls)
	}
}

func TestAPTAdmissionFromRejectedArchiveDoesNotSuppressLiveDelivery(t *testing.T) {
	live := newConditionHarness(t)
	archive := newConditionHarness(t)
	fact := ConditionFact{
		ID: APTReconciliationConditionID("target", "original"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}
	archive.facts = []ConditionFact{fact}
	archive.reconcile(t)
	archive.drain(t)
	if archive.client.calls != 1 {
		t.Fatal("fixture archive is missing its delivered alert")
	}
	if _, err := archive.db.Exec(`CREATE TRIGGER reject_archive_cleanup
		BEFORE DELETE ON notification_outbox BEGIN SELECT RAISE(ABORT, 'reject archive cleanup'); END`); err != nil {
		t.Fatal(err)
	}
	live.now = archive.now.Add(defaultOutboxRetention + time.Hour)
	live.service.deps.Retention = defaultOutboxRetention
	ctx := t.Context()
	if err := live.service.PreparePersistenceReplacement(ctx); err != nil {
		t.Fatal(err)
	}
	if err := live.service.StageAPTDeliveryEvidence(ctx, archive.db); err != nil {
		t.Fatal(err)
	}
	liveDB := live.db
	live.service.deps.DB = func() *sql.DB { return archive.db }
	if err := live.service.ReloadPersistence(ctx); err == nil || !strings.Contains(err.Error(), "reject archive cleanup") {
		t.Fatalf("reload result = %v, want cleanup failure", err)
	}
	live.service.deps.DB = func() *sql.DB { return liveDB }
	if err := live.service.ReloadPersistence(ctx); err != nil {
		t.Fatal(err)
	}
	live.facts = []ConditionFact{fact}
	live.reconcile(t)
	live.drain(t)
	if live.client.calls != 1 {
		t.Fatalf("rejected archive suppressed the live alert, deliveries = %d", live.client.calls)
	}
}

func TestAPTRestorePreservesArchivedDeliveryProgress(t *testing.T) {
	for _, tc := range []struct {
		name              string
		archiveState      string
		archiveAttempts   int
		remainingAttempts int
	}{
		{name: "already delivered", archiveState: outboxStateSucceeded, archiveAttempts: 1},
		{name: "attempts exhausted", archiveState: outboxStateFailed, archiveAttempts: defaultAttempts},
		{name: "attempts remaining", archiveState: outboxStateRetrying, archiveAttempts: defaultAttempts - 1, remainingAttempts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := newConditionHarness(t)
			archive := newConditionHarness(t)
			fact := ConditionFact{
				ID: APTReconciliationConditionID("target", "original"), EventType: EventAPTReconciliationRequired,
				TargetType: "server", TargetName: "node", Active: true,
			}
			live.facts = []ConditionFact{fact}
			live.reconcile(t)

			ctx := t.Context()
			archive.facts = []ConditionFact{fact}
			archive.reconcile(t)
			archive.client.fail = tc.archiveState != outboxStateSucceeded
			for range tc.archiveAttempts {
				row, err := claimNextOutboxRow(ctx, archive.db, archive.now)
				if err != nil || row == nil {
					t.Fatal(row, err)
				}
				archive.service.processOutboxRow(ctx, *row)
			}

			if err := live.service.PreparePersistenceReplacement(ctx); err != nil {
				t.Fatal(err)
			}
			if err := live.service.StageAPTDeliveryEvidence(ctx, archive.db); err != nil {
				t.Fatal(err)
			}
			live.db = archive.db
			live.service.deps.DB = func() *sql.DB { return archive.db }
			if err := live.service.ReloadPersistence(ctx); err != nil {
				t.Fatal(err)
			}
			var state string
			var attempts int
			if err := live.db.QueryRow(`SELECT state, attempts FROM notification_outbox WHERE event_type=?`, EventAPTReconciliationRequired).Scan(&state, &attempts); err != nil ||
				state != tc.archiveState || attempts != tc.archiveAttempts {
				t.Fatalf("restored APT delivery = %q after %d attempts, %v, want %q after %d attempts", state, attempts, err, tc.archiveState, tc.archiveAttempts)
			}
			live.client.fail = true
			live.drain(t)
			if live.client.calls != tc.remainingAttempts {
				t.Fatalf("attempts after restore = %d, want %d", live.client.calls, tc.remainingAttempts)
			}
		})
	}
}

func TestAPTRestoreKeepsRetainedHistoryAndSuppressesExpiredAdmissions(t *testing.T) {
	for _, test := range []struct {
		name      string
		pruned    bool
		exited    bool
		wantState string
	}{
		{name: "retain terminal history missing in the archive", wantState: outboxStateSucceeded},
		{name: "skip old pending row after its live history expired", pruned: true, wantState: outboxStateSkipped},
		{name: "skip old pending row after an exit before runtime reload", pruned: true, exited: true, wantState: outboxStateSkipped},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newConditionHarness(t)
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "archived.db")
			h.facts = []ConditionFact{{
				ID: APTReconciliationConditionID("target", "original"), EventType: EventAPTReconciliationRequired,
				TargetType: "server", TargetName: "node", Active: true, MetaJSON: `{"job_id":"original"}`,
			}}
			if test.pruned {
				h.reconcile(t)
			}
			if _, err := h.db.Exec(`VACUUM INTO ?`, path); err != nil {
				t.Fatal(err)
			}
			h.reconcile(t)
			h.drain(t)
			if h.client.calls != 1 {
				t.Fatalf("original APT delivery attempts = %d", h.client.calls)
			}
			if test.pruned {
				h.now = h.now.Add(defaultOutboxRetention + time.Hour)
				if pruned, err := pruneOutbox(ctx, h.db, h.now, defaultOutboxRetention); err != nil || pruned != 1 {
					t.Fatalf("outbox prune = %d, %v", pruned, err)
				}
			}
			if err := h.service.PreparePersistenceReplacement(ctx); err != nil {
				t.Fatal(err)
			}
			archived, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = archived.Close() })
			if err := h.service.StageAPTDeliveryEvidence(ctx, archived); err != nil {
				t.Fatal(err)
			}
			h.db = archived
			h.service.deps.DB = func() *sql.DB { return archived }
			if !test.exited {
				if err := h.service.ReloadPersistence(ctx); err != nil {
					t.Fatal(err)
				}
			}
			h.service = &Service{deps: h.service.deps, wake: make(chan struct{}, 1), monitoringStarted: true}
			h.reconcile(t)
			h.drain(t)
			if h.client.calls != 1 || h.count(t, EventAPTReconciliationRequired) != 1 {
				t.Fatalf("restore or restart repeated an admission: %d", h.client.calls)
			}
			var state string
			if err := h.db.QueryRow(`SELECT state FROM notification_outbox WHERE event_type=?`, EventAPTReconciliationRequired).Scan(&state); err != nil || state != test.wantState {
				t.Fatalf("restored APT delivery state = %q, %v, want %q", state, err, test.wantState)
			}
		})
	}
}
