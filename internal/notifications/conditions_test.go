package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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

func newConditionHarness(t *testing.T) *conditionHarness {
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
	_, err := h.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: []string{EventRebootRequired}, RemindersEnabled: true, ReminderIntervalMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	other := rebootFact(true)
	other.ID = "reboot/second"
	other.TargetName = "second"
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
	if !strings.Contains(message, "2 unresolved") || strings.Contains(message, "backup") {
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
