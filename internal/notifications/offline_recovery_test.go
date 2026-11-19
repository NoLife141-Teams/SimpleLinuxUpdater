package notifications

import (
	"database/sql"
	"testing"
	"time"
)

func TestOfflineDeliveryEvidenceKeepsTerminalOutcomesFromEitherCopy(t *testing.T) {
	live := openOutboxTestDB(t, "interrupted.db")
	staged := openOutboxTestDB(t, "rollback.db")
	for _, db := range []*sql.DB{live, staged} {
		if err := EnsureSchema(db); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	liveCompleted := DeliveryIntent{ID: "live-completed", Action: EventUpdateComplete}
	selectedCompleted := DeliveryIntent{ID: "selected-completed", Action: EventUpdateComplete}
	for _, intent := range []DeliveryIntent{liveCompleted, selectedCompleted} {
		for _, db := range []*sql.DB{live, staged} {
			insertPendingOutboxIntent(t, db, intent, now)
		}
	}
	for db, intent := range map[*sql.DB]DeliveryIntent{live: liveCompleted, staged: selectedCompleted} {
		if _, err := db.Exec(`UPDATE notification_outbox SET state=?, attempts=1 WHERE id=?`,
			outboxStateSucceeded, outboxDeliveryID(intent.ID, DestinationWebhook)); err != nil {
			t.Fatal(err)
		}
	}
	if err := MergeOfflineDeliveryEvidence(t.Context(), live, staged, now); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []DeliveryIntent{liveCompleted, selectedCompleted} {
		var state string
		if err := staged.QueryRow(`SELECT state FROM notification_outbox WHERE id=?`,
			outboxDeliveryID(intent.ID, DestinationWebhook)).Scan(&state); err != nil || state != outboxStateSucceeded {
			t.Fatalf("%s state = %q, %v", intent.ID, state, err)
		}
	}
}

func TestOfflineDeliveryEvidencePreventsApprovalAndConditionReplay(t *testing.T) {
	for _, expiredSide := range []string{"interrupted", "selected"} {
		t.Run(expiredSide, func(t *testing.T) {
			live, staged := newConditionHarness(t), newConditionHarness(t)
			approval := ConditionFact{ID: "approval/job", EventType: EventApprovalPublished, TargetName: "test",
				ServerIdentity: "inventory", Generation: "1", Active: true, MetaJSON: `{"job_id":"job","approval_generation":1}`}
			apt := ConditionFact{ID: APTReconciliationConditionID("target", "job"), EventType: EventAPTReconciliationRequired,
				TargetName: "test", Active: true, MetaJSON: `{"job_id":"job"}`}
			facts := []ConditionFact{approval, rebootFact(true), apt}
			for _, copy := range []*conditionHarness{live, staged} {
				copy.facts = facts
				copy.reconcile(t)
			}
			expired := live
			if expiredSide == "selected" {
				expired = staged
			}
			expired.drain(t)
			if expired.client.calls != len(facts) {
				t.Fatalf("fixture produced %d deliveries, want %d", expired.client.calls, len(facts))
			}
			expired.client.calls = 0
			if _, err := expired.db.Exec(`DELETE FROM notification_outbox`); err != nil {
				t.Fatal(err)
			}
			if err := MergeOfflineDeliveryEvidence(t.Context(), live.db, staged.db, live.now); err != nil {
				t.Fatal(err)
			}
			state, err := loadConditionState(t.Context(), staged.db)
			if err != nil || state.Cursor != "restore" {
				t.Fatalf("condition cursor = %q, %v", state.Cursor, err)
			}
			staged.reconcile(t)
			staged.drain(t)
			if staged.client.calls != 0 {
				t.Fatalf("restored notifications replayed %d deliveries", staged.client.calls)
			}
		})
	}
}
