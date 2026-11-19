package notifications

import (
	"database/sql"
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
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	identity := aptAdmissionID(fact, fingerprint)
	otherAdmission := episodeAdmissionID(Condition{Fact: rebootFact(true), Episode: 1}, fingerprint)
	state, err := loadConditionState(ctx, h.db)
	if err != nil {
		t.Fatal(err)
	}
	state.Admissions[identity] = true
	state.Admissions[otherAdmission] = true
	if err := saveConditionState(ctx, h.db, &state); err != nil {
		t.Fatal(err)
	}
	// A failure to remove the legacy key must also roll back the indexed copy.
	if _, err := h.db.Exec(`CREATE TRIGGER reject_migration BEFORE UPDATE ON notification_conditions
		BEGIN SELECT RAISE(ABORT, 'reject migration'); END`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(h.db); err == nil || !strings.Contains(err.Error(), "reject migration") {
		t.Fatalf("migration result = %v, want rejected JSON update", err)
	}
	if exists, err := hasAPTAdmission(ctx, h.db, identity); err != nil || exists {
		t.Fatalf("partially committed admission = %v, %v", exists, err)
	}
	if _, err := h.db.Exec(`DROP TRIGGER reject_migration`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := EnsureSchema(h.db); err != nil {
			t.Fatal(err)
		}
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
	h.facts = []ConditionFact{fact}
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
