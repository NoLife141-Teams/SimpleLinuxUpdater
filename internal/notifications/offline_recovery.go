package notifications

import (
	"context"
	"database/sql"
	"slices"
	"time"
)

// MergeOfflineDeliveryEvidence reconciles the selected rollback database with
// deliveries and observations made before the app was stopped. Both databases
// are private copies. Pending conditions are revalidated on the next startup.
func MergeOfflineDeliveryEvidence(ctx context.Context, live, staged *sql.DB, now time.Time) error {
	if err := ensureSchema(ctx, live); err != nil {
		return err
	}
	if err := ensureSchema(ctx, staged); err != nil {
		return err
	}
	observed, incoming, err := loadPersistenceEvidence(ctx, live)
	if err != nil {
		return err
	}
	selected, saved, err := loadPersistenceEvidence(ctx, staged)
	if err != nil {
		return err
	}
	timestamp := now.UTC().Format(time.RFC3339Nano)
	selectedExpired := offlineExpiredDeliveries(selected, saved, incoming.RetainedRows)
	for id := range offlineExpiredDeliveries(observed, incoming, saved.RetainedRows) {
		incoming.TerminalOutcomes[id] = outboxOutcome{
			State: outboxStateSkipped, CompletedAt: timestamp, Error: outboxSkipConditionIneligible,
		}
	}
	selected.mergeOfflineObservations(observed)
	selected.mergeOfflineApprovalAdmissions(observed, incoming, saved)
	for id, snapshot := range incoming.RetainedRows {
		if _, completed := incoming.TerminalOutcomes[id]; selectedExpired[id] && !completed {
			delete(incoming.RetainedRows, id)
			continue
		}
		if snapshot.Row.EventType == EventAPTReconciliationRequired {
			continue
		}
		_, terminal := saved.TerminalOutcomes[id]
		pending, queued := saved.RetainedRows[id]
		revalidates := isConditionEvent(snapshot.Row.EventType) || snapshot.Row.EventType == EventReminder
		// Keep a terminal result or a more advanced retry from either copy.
		// Import only matching ordinary rows and work the runtime can revalidate.
		if terminal || (queued && pending.Outcome.Attempts > snapshot.Outcome.Attempts) || (!queued && !revalidates) {
			delete(incoming.RetainedRows, id)
		}
	}
	if err := restoreCurrentOutboxRows(ctx, staged, &incoming, timestamp); err != nil {
		return err
	}
	selected.Cursor = "restore"
	return saveConditionState(ctx, staged, &selected)
}

// An admission can outlive its row. A pending copy from the other database
// must not restart delivery after the original outcome was pruned.
func offlineExpiredDeliveries(state conditionState, evidence outboxReplacement, candidates map[string]outboxSnapshot) map[string]bool {
	if len(candidates) == 0 {
		return nil
	}
	expired := map[string]bool{}
	mark := func(id string) {
		if _, queued := candidates[id]; !queued {
			return
		}
		_, retained := evidence.RetainedRows[id]
		_, terminal := evidence.TerminalOutcomes[id]
		if !retained && !terminal {
			expired[id] = true
		}
	}
	markIntent := func(identity string) {
		for _, destination := range []string{DestinationWebhook, DestinationDiscord, DestinationTelegram} {
			mark(outboxDeliveryID(identity, destination))
		}
	}
	for _, identity := range evidence.APTAdmissions {
		markIntent(identity)
	}
	for identity, admitted := range state.Admissions {
		if admitted {
			markIntent(identity)
		}
	}
	for _, admissions := range state.ApprovalAdmissions {
		for _, admission := range admissions {
			mark(admission.OutboxID)
		}
	}
	return expired
}

func (selected *conditionState) mergeOfflineObservations(observed conditionState) {
	for id, condition := range observed.Conditions {
		previous, exists := selected.Conditions[id]
		if !exists || laterNotificationTime(condition.UpdatedAt, previous.UpdatedAt) ||
			(condition.UpdatedAt == previous.UpdatedAt && condition.Episode > previous.Episode) {
			selected.Conditions[id] = condition
		}
	}
	for id, admitted := range observed.Admissions {
		if admitted {
			selected.Admissions[id] = true
		}
	}
	for fingerprint, at := range observed.Reminders {
		if laterNotificationTime(at, selected.Reminders[fingerprint]) {
			selected.Reminders[fingerprint] = at
		}
	}
}

func (selected *conditionState) mergeOfflineApprovalAdmissions(observed conditionState, incoming, saved outboxReplacement) {
	for id, admissions := range observed.ApprovalAdmissions {
		for _, admission := range admissions {
			_, retained := incoming.RetainedRows[admission.OutboxID]
			_, terminal := incoming.TerminalOutcomes[admission.OutboxID]
			previous, exists := selected.findApprovalAdmission(id, admission)
			if !exists {
				selected.recordApprovalAdmission(id, admission)
			} else if admission.OutboxID != "" && admission.OutboxID != previous.OutboxID {
				// Legacy approval deliveries can have different row IDs for the
				// same plan. Prefer known delivery evidence over a pending copy.
				if _, completed := saved.TerminalOutcomes[previous.OutboxID]; !completed && (terminal || !retained) {
					entries := selected.ApprovalAdmissions[id]
					entries[slices.Index(entries, previous)] = admission
				}
			}
		}
	}
}

func laterNotificationTime(a, b string) bool {
	left, _ := time.Parse(time.RFC3339Nano, a)
	right, _ := time.Parse(time.RFC3339Nano, b)
	return left.After(right)
}
