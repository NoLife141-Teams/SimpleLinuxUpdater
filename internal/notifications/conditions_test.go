package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type conditionTestClient struct {
	calls  int
	onCall func()
	fail   bool
}

func (c *conditionTestClient) Do(*http.Request) (*http.Response, error) {
	c.calls++
	if c.onCall != nil {
		c.onCall()
	}
	if c.fail {
		return nil, errors.New("transport unavailable")
	}
	return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: http.Header{}}, nil
}

type conditionHarness struct {
	service   *Service
	db        *sql.DB
	now       time.Time
	facts     []ConditionFact
	sourceErr error
	client    *conditionTestClient
}

func newConditionHarness(t testing.TB) *conditionHarness {
	t.Helper()
	db := openOutboxTestDB(t, "conditions.db")
	prepareOutboxSettings(t, db, "https://hooks.example.test/test", true)
	h := &conditionHarness{db: db, now: time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC), client: &conditionTestClient{}}
	deps := outboxTestDeps(db, time.Hour)
	deps.Now = func() time.Time { return h.now }
	deps.HTTPClient = h.client
	deps.ReadConditions = func(_ context.Context, cursor string, _ []Condition) (ConditionBatch, error) {
		return ConditionBatch{Facts: h.facts, Cursor: cursor}, h.sourceErr
	}
	h.service = &Service{deps: deps, wake: make(chan struct{}, 1), monitoringStarted: true}
	_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: SupportedEvents(), ReminderIntervalMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
func (h *conditionHarness) reconcile(t *testing.T) {
	t.Helper()
	if err := h.service.reconcileConditions(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func (h *conditionHarness) count(t *testing.T, event string) int {
	t.Helper()
	var n int
	if err := h.db.QueryRow(`SELECT COUNT(*) FROM notification_outbox WHERE event_type=?`, event).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
func (h *conditionHarness) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 30; i++ {
		row, err := claimNextOutboxRow(context.Background(), h.db, h.now)
		if err != nil {
			t.Fatal(err)
		}
		if row == nil {
			return
		}
		h.service.processOutboxRow(context.Background(), *row)
	}
	t.Fatal("outbox did not drain")
}
func rebootFact(active bool) ConditionFact {
	return ConditionFact{ID: "reboot/test", EventType: EventRebootRequired, TargetType: "server", TargetName: "test", Active: active, Message: "Reboot required"}
}

func TestConditionEpisodesRestartResolutionUnknownAndGeneration(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	h.reconcile(t)
	if h.count(t, EventRebootRequired) != 1 || h.client.calls != 1 {
		t.Fatal("repeated observation duplicated admission")
	}
	h.service = &Service{deps: h.service.deps, wake: make(chan struct{}, 1), monitoringStarted: true} // Same durable DB, no cached condition state.
	h.facts = nil
	h.reconcile(t)
	h.drain(t)
	if h.count(t, EventRebootRequired) != 1 {
		t.Fatal("restart or unavailable observation duplicated admission")
	}
	h.facts = []ConditionFact{rebootFact(false)}
	h.reconcile(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	if h.count(t, EventRebootRequired) != 2 || h.client.calls != 2 {
		t.Fatal("healthy then recurrence did not start new episode")
	}
	fact := ConditionFact{ID: "approval/job", EventType: EventApprovalPublished, TargetName: "test", Generation: "1", Active: true, Message: "job generation 1", MetaJSON: `{"job_id":"job","approval_generation":1}`}
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
	fact.Generation = "2"
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
	h.drain(t)
	if h.count(t, EventApprovalPublished) != 2 || h.client.calls != 3 {
		t.Fatalf("obsolete generation delivered: calls=%d", h.client.calls)
	}
	fact.Active = false
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
}

func TestApprovalAdmissionSurvivesOlderObservationAndRestore(t *testing.T) {
	h := newConditionHarness(t)
	old := ConditionFact{ID: "approval/job", EventType: EventApprovalPublished, TargetType: "server",
		TargetName: "original", ServerIdentity: "inventory", Generation: "1", Active: true, Message: "Generation 1",
		MetaJSON: `{"job_id":"job","approval_generation":1}`}
	current := old
	current.Generation, current.Message = "2", "Generation 2"
	current.MetaJSON = `{"job_id":"job","approval_generation":2}`
	h.facts = []ConditionFact{current}
	h.reconcile(t)
	h.drain(t)

	// The admission must survive an episode rollover even if a condition source
	// reports an older observation before restoring the current generation.
	h.facts = []ConditionFact{old, current}
	h.reconcile(t)
	h.facts = []ConditionFact{current}
	h.drain(t)
	if h.client.calls != 1 || h.count(t, EventApprovalPublished) != 1 {
		t.Fatalf("older observation re-admitted the current plan: deliveries=%d", h.client.calls)
	}

	current.Active = false
	h.facts = []ConditionFact{current}
	h.reconcile(t)
	state, err := loadConditionState(t.Context(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	h.service = &Service{deps: h.service.deps, wake: make(chan struct{}, 1), monitoringStarted: true, replacementConditions: &state}
	if err := h.service.restoreConditions(t.Context(), h.db); err != nil {
		t.Fatal(err)
	}
	// A legacy row from a restored archive must not be sent if the live
	// ledger already admitted a different row for this exact plan.
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	legacy := Condition{Fact: current, Episode: state.Conditions[current.ID].Episode + 1}
	insertLegacyApprovalRow(t, h, legacy, fingerprint, outboxStatePending)
	current.Active, current.TargetName = true, "renamed"
	h.facts = []ConditionFact{current}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 1 || h.count(t, EventApprovalPublished) != 2 {
		t.Fatalf("restored live ledger re-admitted the same plan: deliveries=%d", h.client.calls)
	}

	_, err = h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: settings.EventTypes,
		WebhookURLIntent: WebhookURLReplace, WebhookURL: "https://hooks.example.test/changed"})
	if err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 2 || h.count(t, EventApprovalPublished) != 3 {
		t.Fatalf("new destination did not get exactly one current alert: deliveries=%d", h.client.calls)
	}
}

func TestApprovalMigratesAndDeliversExistingPendingEpisode(t *testing.T) {
	for _, previouslySent := range []bool{false, true} {
		name, deliveries := "pending", 1
		if previouslySent {
			name, deliveries = "already delivered in an earlier episode", 0
		}
		t.Run(name, func(t *testing.T) {
			h := newConditionHarness(t)
			settings, err := h.service.currentSettings()
			if err != nil {
				t.Fatal(err)
			}
			fact := ConditionFact{ID: "approval/old-job", EventType: EventApprovalPublished, TargetType: "server",
				TargetName: "original", Generation: "2", Active: true, Message: "Review the plan in Status",
				MetaJSON: `{"job_id":"old-job","approval_generation":2}`}
			legacy := Condition{Fact: fact, Episode: 4, UpdatedAt: h.now.Format(time.RFC3339Nano)}
			fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
			wantRows := 1
			if previouslySent {
				earlier := legacy
				earlier.Episode = 2
				insertLegacyApprovalRow(t, h, earlier, fingerprint, outboxStateSucceeded)
				wantRows++
			}
			id := insertLegacyApprovalRow(t, h, legacy, fingerprint, outboxStatePending)
			raw, err := json.Marshal(conditionState{
				Conditions: map[string]Condition{fact.ID: legacy},
				Admissions: map[string]bool{id: true},
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := h.db.Exec(`INSERT INTO notification_conditions(id,state_json) VALUES(1,?)`, string(raw)); err != nil {
				t.Fatal(err)
			}
			fact.ServerIdentity = "stable-inventory"
			h.facts = []ConditionFact{fact}
			h.reconcile(t)
			h.drain(t)
			if h.client.calls != deliveries || h.count(t, EventApprovalPublished) != wantRows {
				t.Fatalf("legacy queue sent %d notifications, want %d", h.client.calls, deliveries)
			}
		})
	}
}

func TestApprovalMigrationRemembersPreviousDestination(t *testing.T) {
	h := newConditionHarness(t)
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	originalURL := settings.WebhookURL
	fact := ConditionFact{ID: "approval/old-job", EventType: EventApprovalPublished, TargetType: "server",
		TargetName: "original", Generation: "2", Active: true, Message: "Review the plan in Status",
		MetaJSON: `{"job_id":"old-job","approval_generation":2}`}
	legacy := Condition{Fact: fact, Episode: 4, UpdatedAt: h.now.Format(time.RFC3339Nano)}
	insertLegacyApprovalRow(t, h, legacy, destinationConfigFingerprint(settings, DestinationWebhook), outboxStateSucceeded)

	replaceDestination := func(url string) {
		t.Helper()
		_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: settings.EventTypes,
			WebhookURLIntent: WebhookURLReplace, WebhookURL: url})
		if err != nil {
			t.Fatal(err)
		}
	}
	const replacementURL = "https://hooks.example.test/replacement"
	replaceDestination(replacementURL)
	settings.WebhookURL = replacementURL
	currentAdmission := insertLegacyApprovalRow(t, h, legacy, destinationConfigFingerprint(settings, DestinationWebhook), outboxStateSucceeded)
	// The pre-upgrade ledger retained only the configured destination, while
	// the outbox still records the delivery to the original configuration.
	raw, err := json.Marshal(conditionState{
		Conditions: map[string]Condition{fact.ID: legacy},
		Admissions: map[string]bool{currentAdmission: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`INSERT INTO notification_conditions(id,state_json) VALUES(1,?)`, string(raw)); err != nil {
		t.Fatal(err)
	}
	fact.ServerIdentity = "stable-inventory"
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
	replaceDestination(originalURL)
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 0 || h.count(t, EventApprovalPublished) != 2 {
		t.Fatalf("switching back re-admitted a delivered legacy plan: deliveries=%d", h.client.calls)
	}
}

func insertLegacyApprovalRow(t *testing.T, h *conditionHarness, legacy Condition, fingerprint, state string) string {
	t.Helper()
	intent := conditionIntent(legacy)
	intent.ID = fmt.Sprintf("condition/%s/%d/%s", legacy.Fact.ID, legacy.Episode, fingerprint)
	rows, err := buildOutboxRows(intent, EventApprovalPublished, []string{DestinationWebhook}, h.now)
	if err != nil {
		t.Fatal(err)
	}
	rows[0].DestinationFingerprint = fingerprint
	if err := insertOutboxRows(t.Context(), h.db, rows, h.now.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := h.db.Exec(`UPDATE notification_outbox SET state = ? WHERE id = ?`, state, rows[0].ID); err != nil {
		t.Fatal(err)
	}
	return intent.ID
}

func TestConditionRemindersSelectedGroupedAndResolvedBeforeDelivery(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	h.now = h.now.Add(24 * time.Hour)
	h.reconcile(t)
	if h.count(t, EventReminder) != 0 {
		t.Fatal("reminders enabled by default")
	}
	_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: []string{EventRebootRequired, EventApprovalPublished}, RemindersEnabled: true, ReminderIntervalMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	const approvalURL = "https://updates.example/?approval_generation=2&job_id=job&server_identity=stable"
	other := ConditionFact{ID: "approval/job", EventType: EventApprovalPublished, TargetName: "second", Generation: "2", Active: true, Message: "Review the exact plan in Status: " + approvalURL}
	hidden := ConditionFact{ID: "backup/missing", EventType: EventBackupCoverageMissing, TargetName: "backup", Active: true, Message: "Do not include unselected backup"}
	h.facts = []ConditionFact{rebootFact(true), other, hidden}
	h.reconcile(t)
	h.drain(t)
	h.now = h.now.Add(5 * time.Minute)
	h.reconcile(t)
	h.reconcile(t)
	if h.count(t, EventReminder) != 1 {
		t.Fatal("expected one grouped reminder")
	}
	var message string
	if err := h.db.QueryRow(`SELECT message FROM notification_outbox WHERE event_type=?`, EventReminder).Scan(&message); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(message, "2 unresolved") || !strings.Contains(message, approvalURL) || strings.Contains(message, "backup") {
		t.Fatal(message)
	}
	h.facts = []ConditionFact{rebootFact(false)}
	other.Active = false
	h.facts = append(h.facts, other)
	before := h.client.calls
	h.drain(t)
	if h.client.calls != before {
		t.Fatal("resolved reminder was delivered")
	}
	h.now = h.now.Add(time.Hour)
	h.reconcile(t)
	if h.count(t, EventReminder) != 1 {
		t.Fatal("resolved conditions kept reminders")
	}
}

func TestConditionDeliveryRevalidatesEachDestinationAndUnavailableFacts(t *testing.T) {
	h := newConditionHarness(t)
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	settings.DiscordEnabled = true
	settings.DiscordWebhookURL = "https://discord.com/api/webhooks/123/test-token"
	if err := h.service.saveSettings(settings); err != nil {
		t.Fatal(err)
	}
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.client.onCall = func() { h.facts = []ConditionFact{rebootFact(false)} }
	h.drain(t)
	if h.client.calls != 1 {
		t.Fatalf("second destination sent stale alert: %d", h.client.calls)
	}
	assertPersistedSkipReason(t, h.db, "condition_ineligible", "The notification condition was no longer eligible.")
	h.client.onCall = nil
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.sourceErr = errors.New("facts unavailable")
	row, err := claimNextOutboxRow(context.Background(), h.db, h.now)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	h.service.processOutboxRow(context.Background(), *row)
	if h.client.calls != 1 {
		t.Fatal("unavailable revalidation delivered from stale state")
	}
	state, err := loadConditionState(context.Background(), h.db)
	if err != nil || !state.Conditions["reboot/test"].Fact.Active {
		t.Fatal("unavailable observation healed condition", err)
	}
}

func TestGroupedReminderDistinguishesDestinationRotationFromResolution(t *testing.T) {
	for _, tc := range []struct {
		name         string
		active       bool
		code, reason string
	}{
		{"unresolved", true, "destination_configuration_changed", "Destination configuration changed after admission."},
		{"resolved", false, "condition_ineligible", "The notification condition was no longer eligible."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newConditionHarness(t)
			settings := SettingsUpdate{Enabled: true, EventTypes: []string{EventRebootRequired}, RemindersEnabled: true, ReminderIntervalMinutes: 5}
			if _, err := h.service.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			h.facts = []ConditionFact{rebootFact(true)}
			h.reconcile(t)
			h.drain(t)
			h.now = h.now.Add(5 * time.Minute)
			h.reconcile(t)
			if h.count(t, EventReminder) != 1 {
				t.Fatal("expected a queued reminder for the original destination")
			}

			settings.WebhookURLIntent = WebhookURLReplace
			settings.WebhookURL = "https://hooks.example.test/replacement"
			if _, err := h.service.SaveSettings(settings); err != nil {
				t.Fatal(err)
			}
			h.facts = []ConditionFact{rebootFact(tc.active)}
			h.drain(t)
			assertPersistedSkipReason(t, h.db, tc.code, tc.reason)
		})
	}
}

func TestConditionDestinationChangeDisableAndOneShotOptIn(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, WebhookURLIntent: WebhookURLReplace, WebhookURL: "https://hooks.example.test/new", EventTypes: SupportedEvents()})
	if err != nil {
		t.Fatal(err)
	}
	h.drain(t)
	if h.client.calls != 1 || h.count(t, EventRebootRequired) != 2 {
		t.Fatal("destination replacement did not skip old binding and admit one current alert")
	}
	_, err = h.service.SaveSettings(SettingsUpdate{Enabled: false, EventTypes: SupportedEvents()})
	if err != nil {
		t.Fatal(err)
	}
	expired := ConditionFact{ID: "expired/job/1", EventType: EventApprovalExpired, Active: true, OneShot: true, TargetName: "test", Generation: "1", Message: "Expired job"}
	h.facts = []ConditionFact{expired}
	h.reconcile(t)
	_, err = h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: SupportedEvents()})
	if err != nil {
		t.Fatal(err)
	}
	h.facts = nil
	h.reconcile(t)
	h.drain(t)
	if h.count(t, EventApprovalExpired) != 0 {
		t.Fatal("activation replayed old expiry")
	}
}

func TestConditionRestorePreservesAdmissionAndLedgerAtomicity(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	state, err := loadConditionState(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	h.service.replacementConditions = &state
	if _, err := h.db.Exec(`DELETE FROM notification_conditions`); err != nil {
		t.Fatal(err)
	}
	if err := h.service.restoreConditions(context.Background(), h.db); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 1 {
		t.Fatal("restore duplicated unchanged episode")
	}
	if _, err := h.db.Exec(`CREATE TRIGGER reject_condition_write BEFORE UPDATE ON notification_conditions BEGIN SELECT RAISE(ABORT,'test failure'); END`); err != nil {
		t.Fatal(err)
	}
	h.facts = []ConditionFact{rebootFact(false), rebootFact(true)}
	if err := h.service.reconcileConditions(context.Background()); err == nil {
		t.Fatal("expected failure")
	}
	if h.count(t, EventRebootRequired) != 1 {
		t.Fatal("outbox committed without condition ledger")
	}
	if _, err := h.db.Exec(`DROP TRIGGER reject_condition_write`); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	if h.count(t, EventRebootRequired) != 2 {
		t.Fatal("failed transaction lost admission")
	}
}

func TestLegacyNotificationSettingsKeepSelectionAndRemindersOff(t *testing.T) {
	h := newConditionHarness(t)
	for _, raw := range []string{`{"enabled":false}`, `{"enabled":false,"event_types":["update.complete"]}`, `{"enabled":false,"event_types":[]}`} {
		if _, err := h.db.Exec(`UPDATE settings SET value=? WHERE key=?`, raw, SettingsKey); err != nil {
			t.Fatal(err)
		}
		settings, err := h.service.Settings()
		if err != nil {
			t.Fatal(err)
		}
		if settings.RemindersEnabled {
			t.Fatal("legacy reminders enabled")
		}
		for _, event := range settings.EventTypes {
			if isConditionEvent(event) {
				t.Fatal("new event automatically selected")
			}
		}
	}
	h.client.fail = true
	h.facts = []ConditionFact{rebootFact(true)}
	_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, WebhookURL: "https://hooks.example.test/test", WebhookURLIntent: WebhookURLReplace, EventTypes: []string{EventRebootRequired}})
	if err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.drain(t)
	var outcome string
	if err := h.db.QueryRow(`SELECT state FROM notification_outbox WHERE event_type=?`, EventRebootRequired).Scan(&outcome); err != nil {
		t.Fatal(err)
	}
	if outcome != outboxStateFailed {
		t.Fatal(outcome)
	}
	state, err := loadConditionState(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(state)
	if strings.Contains(string(encoded), "hooks.example") {
		t.Fatal("condition state leaked destination")
	}
}

func TestConditionMonitoringStartGatesRestoredPendingDelivery(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.service.monitoringStarted = false
	row, err := claimNextOutboxRow(context.Background(), h.db, h.now)
	if err != nil || row == nil {
		t.Fatal(err)
	}
	h.service.processOutboxRow(context.Background(), *row)
	if h.client.calls != 0 {
		t.Fatal("delivery preceded runtime initialization")
	}
	h.service.StartConditionMonitoring()
	h.now = h.now.Add(time.Hour)
	h.drain(t)
	if h.client.calls != 1 {
		t.Fatal("monitoring did not resume pending delivery")
	}
}

func TestConditionRestoreAcceptsOlderHealthyEvidence(t *testing.T) {
	h := newConditionHarness(t)
	fact := rebootFact(true)
	fact.CreatedAt = h.now.Format(time.RFC3339Nano)
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
	h.drain(t)
	fact.Active = false
	fact.CreatedAt = h.now.Add(-time.Hour).Format(time.RFC3339Nano)
	h.facts = []ConditionFact{fact}
	h.reconcile(t)
	state, err := loadConditionState(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if !state.Conditions[fact.ID].Fact.Active {
		t.Fatal("late older observation superseded current health")
	}
	h.service.deps.ReadConditions = func(context.Context, string, []Condition) (ConditionBatch, error) {
		return ConditionBatch{Facts: []ConditionFact{fact}, Rebased: true}, nil
	}
	h.reconcile(t)
	state, err = loadConditionState(context.Background(), h.db)
	if err != nil {
		t.Fatal(err)
	}
	if state.Conditions[fact.ID].Fact.Active {
		t.Fatal("restored healthy evidence did not resolve live reboot condition")
	}
}
