package notifications

import (
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
)

func TestAPTAdmissionMigrationPreservesExpiredDelivery(t *testing.T) {
	h := newConditionHarness(t)
	ctx := t.Context()
	fact := ConditionFact{
		ID: APTReconciliationConditionID("target", "old-job"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}
	h.facts = []ConditionFact{fact}
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	// Captured legacy key for target/old-job and the fixture webhook.
	const identity = "condition/apt-reconciliation/" +
		"380cf970c2e2471f434a3b9d81ac3e61cf124168346ca181ccb454d146c0f625/" +
		"bf53c189816312a216c4190f1992529351cf8d86a337212037a9abf8b72a79e0"
	otherCondition := Condition{Fact: rebootFact(false), Episode: 1}
	otherAdmission := episodeAdmissionID(otherCondition, fingerprint)
	state, err := loadConditionState(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	state.Admissions[identity] = true
	state.Admissions[otherAdmission] = true
	state.Conditions[otherCondition.Fact.ID] = otherCondition
	if err := saveConditionState(ctx, h.db, &state); err != nil {
		t.Fatal(err)
	}
	// A failure to remove the legacy key must also roll back the indexed copy.
	if _, err := h.db.Exec(`CREATE TRIGGER reject_migration BEFORE UPDATE ON notification_conditions
		BEGIN SELECT RAISE(ABORT, 'reject migration'); END`); err != nil {
		t.Fatal(err)
	}
	if err := h.service.reconcileConditions(ctx); err == nil || !strings.Contains(err.Error(), "reject migration") {
		t.Fatalf("migration result = %v, want rejected JSON update", err)
	}
	if exists, err := hasAPTAdmission(ctx, h.db, identity); err != nil || exists {
		t.Fatalf("partially committed admission = %v, %v", exists, err)
	}
	if _, err := h.db.Exec(`DROP TRIGGER reject_migration`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		h.reconcile(t)
	}
	got, err := loadAPTAdmissions(ctx, h.db)
	if err != nil || !slices.Equal(got, []string{identity}) {
		t.Fatalf("indexed APT admissions = %v, err = %v", got, err)
	}
	state, err = loadConditionState(ctx, h.db)
	if err != nil || len(state.Admissions) != 1 || !state.Admissions[otherAdmission] {
		t.Fatalf("legacy condition state = %+v, err = %v", state, err)
	}
	h.service = &Service{deps: h.service.deps, wake: make(chan struct{}, 1), monitoringStarted: true}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 0 || h.count(t, EventAPTReconciliationRequired) != 0 {
		t.Fatal("an incident with expired outbox history was readmitted after restart")
	}
	settings.WebhookURL = "https://hooks.example.test/new-destination"
	if err := h.service.saveSettings(settings); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 1 || h.count(t, EventAPTReconciliationRequired) != 1 {
		t.Fatal("a new destination for the incident did not get its own delivery")
	}
}

func TestUnreadableConditionsDoNotPreventOpeningDBAndAreRejectedAtStaging(t *testing.T) {
	live := newConditionHarness(t)
	staged := openOutboxTestDB(t, "staged.db")
	if err := EnsureSchema(staged); err != nil {
		t.Fatal(err)
	}
	if _, err := staged.Exec(`INSERT INTO notification_conditions(id,state_json) VALUES(1,?)`, `{"Admissions":`); err != nil {
		t.Fatal(err)
	}
	// The normal application open must succeed so the operator can still
	// inspect other installation state and recover notification persistence.
	if err := EnsureSchema(staged); err != nil {
		t.Fatalf("opening the database failed because condition data is unreadable: %v", err)
	}
	if err := live.service.PreparePersistenceReplacement(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Even if there is no live admission to copy, malformed archive state
	// must fail before the application can install the replacement file.
	err := live.service.StageAPTDeliveryEvidence(t.Context(), staged)
	var invalidJSON *json.SyntaxError
	if !errors.As(err, &invalidJSON) {
		t.Fatalf("staging result = %v, want unreadable condition state", err)
	}
}

func TestAPTAdmissionCommitsWithOutbox(t *testing.T) {
	h := newConditionHarness(t)
	ctx := t.Context()
	h.facts = []ConditionFact{{
		ID: APTReconciliationConditionID("target", "job"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}}
	if _, err := h.db.Exec(`CREATE TRIGGER reject_outbox BEFORE INSERT ON notification_outbox
		BEGIN SELECT RAISE(ABORT, 'reject outbox'); END`); err != nil {
		t.Fatal(err)
	}
	if err := h.service.reconcileConditions(ctx); err == nil || !strings.Contains(err.Error(), "reject outbox") {
		t.Fatalf("admission result = %v, want rejected outbox insert", err)
	}
	if got, err := loadAPTAdmissions(ctx, h.db); err != nil || len(got) != 0 {
		t.Fatalf("admission survived a rolled back delivery: %v, %v", got, err)
	}
	if _, err := h.db.Exec(`DROP TRIGGER reject_outbox`); err != nil {
		t.Fatal(err)
	}
	h.reconcile(t)
	h.drain(t)
	if h.client.calls != 1 {
		t.Fatalf("delivery attempts after retry = %d, want 1", h.client.calls)
	}
}

func TestAPTRestoreStagesUnionWithLegacyArchive(t *testing.T) {
	live := newConditionHarness(t)
	archived := newConditionHarness(t)
	ctx := t.Context()
	liveFact := ConditionFact{
		ID: APTReconciliationConditionID("target", "live-job"), EventType: EventAPTReconciliationRequired,
		TargetType: "server", TargetName: "node", Active: true,
	}
	live.facts = []ConditionFact{liveFact}
	live.reconcile(t)
	live.drain(t)
	if live.client.calls != 1 {
		t.Fatal("fixture live delivery was not recorded")
	}
	archivedFact := liveFact
	archivedFact.ID = APTReconciliationConditionID("target", "archived-job")
	settings, err := archived.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	legacyID := aptAdmissionID(archivedFact, fingerprint)
	legacy, err := loadConditionState(ctx, archived.db)
	if err != nil {
		t.Fatal(err)
	}
	legacy.Admissions[legacyID] = true
	if err := saveConditionState(ctx, archived.db, &legacy); err != nil {
		t.Fatal(err)
	}
	if _, err := archived.db.Exec(`DROP TABLE notification_apt_admissions`); err != nil {
		t.Fatal(err)
	}
	if err := live.service.PreparePersistenceReplacement(ctx); err != nil {
		t.Fatal(err)
	}
	if err := live.service.StageAPTDeliveryEvidence(ctx, archived.db); err != nil {
		t.Fatal(err)
	}
	want := []string{legacyID, aptAdmissionID(liveFact, fingerprint)}
	slices.Sort(want)
	if got, err := loadAPTAdmissions(ctx, archived.db); err != nil || !slices.Equal(got, want) {
		t.Fatalf("staged APT admissions = %v, err = %v, want %v", got, err, want)
	}
	// Start from the staged database without reloading the original runtime.
	live.service.deps.DB = func() *sql.DB { return archived.db }
	live.service = &Service{deps: live.service.deps, wake: make(chan struct{}, 1), monitoringStarted: true}
	live.db = archived.db
	live.facts = []ConditionFact{liveFact, archivedFact}
	live.reconcile(t)
	live.drain(t)
	if live.client.calls != 1 || live.count(t, EventAPTReconciliationRequired) != 1 {
		t.Fatal("an incident was readmitted after stage and restart")
	}
}
