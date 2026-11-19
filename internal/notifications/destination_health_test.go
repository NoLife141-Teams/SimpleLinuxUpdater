package notifications

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDestinationHealthSeparatesQueueAttemptsRotationAndManualTests(t *testing.T) {
	h := newConditionHarness(t)
	const token = "123456789:ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghi"
	update := SettingsUpdate{Enabled: true, EventTypes: SupportedEvents(), Telegram: &TelegramUpdate{
		Enabled: true, BotToken: token, ChatID: "-1001234567890", CredentialsIntent: WebhookURLReplace,
	}}
	if _, err := h.service.SaveSettings(update); err != nil {
		t.Fatal(err)
	}
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	inspect := func() map[string]DestinationHealth {
		t.Helper()
		decrypt := h.service.deps.DecryptSecret
		h.service.deps.DecryptSecret = func(string) (string, error) {
			t.Fatal("installation health decrypted a destination")
			return "", nil
		}
		defer func() { h.service.deps.DecryptSecret = decrypt }()
		_, facts, err := h.service.InstallationHealth(t.Context())
		if err != nil || len(facts.Destinations) != 3 {
			t.Fatalf("destination health = %+v, %v", facts, err)
		}
		settings, err := json.Marshal(facts)
		if err != nil {
			t.Fatal(err)
		}
		for _, private := range []string{token, update.Telegram.ChatID, "example.test", "reboot/test", "fingerprint"} {
			if strings.Contains(string(settings), private) {
				t.Fatalf("diagnostics exposed %q", private)
			}
		}
		return facts.Destinations
	}
	initial := inspect()
	if age := initial[DestinationWebhook].OldestPendingAgeSeconds; age == nil || *age != 0 {
		t.Fatalf("pending Webhook age = %v", age)
	}
	queuedAt := h.now
	for range 2 {
		row, err := claimNextOutboxRow(t.Context(), h.db, h.now)
		if err != nil || row == nil {
			t.Fatalf("claim = %v, %v", row, err)
		}
		if age := inspect()[row.Destination].OldestPendingAgeSeconds; age == nil || *age != 0 {
			t.Fatalf("claimed %s age = %v", row.Destination, age)
		}
		h.client.fail = row.Destination == DestinationTelegram
		if !h.service.processOutboxRow(t.Context(), *row) {
			t.Fatalf("failed persisting attempt for %s", row.Destination)
		}
	}
	h.now = h.now.Add(15 * time.Second)
	mixed := inspect()
	webhook, telegram := mixed[DestinationWebhook], mixed[DestinationTelegram]
	if webhook.LastAttemptState != DeliveryOutcomeSucceeded || webhook.LastSuccessfulDeliveryAt == nil ||
		!webhook.LastSuccessfulDeliveryAt.Equal(queuedAt) || webhook.OldestPendingAgeSeconds != nil {
		t.Fatalf("Webhook facts = %+v", webhook)
	}
	if telegram.LastAttemptState != DeliveryOutcomeRetrying || telegram.LastAttemptAt == nil ||
		telegram.LastSuccessfulDeliveryAt != nil || telegram.OldestPendingAgeSeconds == nil || *telegram.OldestPendingAgeSeconds != 15 {
		t.Fatalf("Telegram facts = %+v", telegram)
	}
	h.client.fail = false
	if _, err := h.service.TestDestination(t.Context(), DestinationTelegram); err != nil {
		t.Fatal(err)
	}
	manual := inspect()[DestinationTelegram]
	if manual.LastManualTestState != DeliveryOutcomeSucceeded || manual.LastManualTestAt == nil ||
		!manual.LastManualTestAt.Equal(h.now) || manual.LastSuccessfulDeliveryAt != nil || manual.LastAttemptState != DeliveryOutcomeRetrying {
		t.Fatalf("manual test rewrote Telegram queue evidence: %+v", manual)
	}
	h.now = queuedAt.Add(time.Hour)
	h.drain(t)
	successAt := h.now
	succeeded := inspect()[DestinationTelegram]
	if succeeded.LastSuccessfulDeliveryAt == nil || !succeeded.LastSuccessfulDeliveryAt.Equal(successAt) ||
		succeeded.LastManualTestAt == nil || !succeeded.LastManualTestAt.Equal(*manual.LastManualTestAt) || succeeded.OldestPendingAgeSeconds != nil {
		t.Fatalf("Telegram after retry = %+v", succeeded)
	}
	h.now = h.now.Add(2 * time.Hour)
	if n, err := pruneOutbox(t.Context(), h.db, h.now, time.Hour); n != 2 || err != nil {
		t.Fatalf("pruned %d rows: %v", n, err)
	}
	restarted := NewService(h.service.deps)
	defer closeOutboxService(t, restarted)
	_, facts, err := restarted.InstallationHealth(t.Context())
	if at := facts.Destinations[DestinationTelegram].LastSuccessfulDeliveryAt; err != nil || at == nil || !at.Equal(successAt) {
		t.Fatalf("Telegram success after pruning and restart = %v, %v", at, err)
	}
	update.Telegram.ChatID = "-1009876543210"
	if _, err := restarted.SaveSettings(update); err != nil {
		t.Fatal(err)
	}
	_, facts, err = restarted.InstallationHealth(t.Context())
	rotated := facts.Destinations[DestinationTelegram]
	if err != nil || rotated.LastSuccessfulDeliveryAt != nil || rotated.LastAttemptState != DeliveryHealthUnknown ||
		rotated.LastManualTestAt != nil || rotated.HistoricalSuccessAt == nil || !rotated.HistoricalSuccessAt.Equal(successAt) {
		t.Fatalf("Telegram after rotation = %+v, %v", rotated, err)
	}
}

func TestDestinationHealthMigratesBoundEvidenceBeforeRetention(t *testing.T) {
	h := newConditionHarness(t)
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	saved := h.now.Add(-48 * time.Hour)
	for _, row := range []struct {
		id, dest, fingerprint, state string
		at                           time.Time
		code                         int
	}{
		{"bound", DestinationWebhook, fingerprint, outboxStateSucceeded, saved, 204},
		{"unbound", DestinationWebhook, "", outboxStateSucceeded, saved.Add(time.Hour), 204},
		{"latest", DestinationWebhook, fingerprint, outboxStateFailed, saved.Add(time.Minute), 500},
		{"no-success", DestinationTelegram, "legacy-failure", outboxStateFailed, saved, 500},
	} {
		at := row.at.Format(time.RFC3339Nano)
		_, err := h.db.Exec(`INSERT INTO notification_outbox
			(id,destination,destination_fingerprint,event_type,action,state,attempted_at,completed_at,status_code,created_at,updated_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)`, row.id, row.dest, row.fingerprint, EventUpdateComplete, EventUpdateComplete, row.state, at, at, row.code, at, at)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`DROP TABLE notification_destination_health;
		UPDATE settings SET value=json_remove(value,'$.destination_fingerprints') WHERE key=?`, SettingsKey); err != nil {
		t.Fatal(err)
	}
	deps := h.service.deps
	deps.Retention = time.Hour
	restarted := NewService(deps)
	defer closeOutboxService(t, restarted)
	_, facts, err := restarted.InstallationHealth(t.Context())
	webhook := facts.Destinations[DestinationWebhook]
	if err != nil || facts.Failed != 0 || webhook.LastAttemptState != DeliveryOutcomeFailed ||
		webhook.LastSuccessfulDeliveryAt == nil || !webhook.LastSuccessfulDeliveryAt.Equal(saved) ||
		webhook.HistoricalSuccessAt != nil || webhook.LastManualTestAt != nil {
		t.Fatalf("migrated destination health = %+v, all facts = %+v, error = %v", webhook, facts, err)
	}
}

func TestDestinationHealthMigrationLeavesConflictingLegacyAttemptsUnknown(t *testing.T) {
	h := newConditionHarness(t)
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	legacyAt := h.now.Format(time.RFC3339)
	for _, result := range []struct {
		id, state string
		code      int
	}{
		{"first", outboxStateSucceeded, 204},
		{"second", outboxStateFailed, 500},
	} {
		if _, err := h.db.Exec(`INSERT INTO notification_outbox
			(id,destination,destination_fingerprint,event_type,action,state,attempted_at,completed_at,status_code,created_at,updated_at)
			VALUES (?,?,?,?,?,?,?,?,?,?,?)`, result.id, DestinationWebhook, fingerprint, EventUpdateComplete, EventUpdateComplete,
			result.state, legacyAt, legacyAt, result.code, legacyAt, legacyAt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.db.Exec(`DROP TABLE notification_destination_health`); err != nil {
		t.Fatal(err)
	}
	if err := EnsureSchema(h.db); err != nil {
		t.Fatal(err)
	}
	_, facts, err := h.service.InstallationHealth(t.Context())
	webhook := facts.Destinations[DestinationWebhook]
	if err != nil || webhook.LastSuccessfulDeliveryAt == nil || !webhook.LastSuccessfulDeliveryAt.Equal(h.now) ||
		webhook.LastAttemptState != DeliveryHealthUnknown || webhook.LastAttemptAt != nil {
		t.Fatalf("tied legacy attempts = %+v, %v", webhook, err)
	}

	h.now = h.now.Add(time.Second)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	_, facts, err = h.service.InstallationHealth(t.Context())
	webhook = facts.Destinations[DestinationWebhook]
	if err != nil || webhook.LastAttemptState != DeliveryOutcomeSucceeded || webhook.LastAttemptAt == nil ||
		!webhook.LastAttemptAt.Equal(h.now) {
		t.Fatalf("new attempt following ambiguous legacy outcomes = %+v, %v", webhook, err)
	}
}

func TestDestinationHealthRewrapsLegacyArchiveBindingsBeforeReadingHealth(t *testing.T) {
	h := newConditionHarness(t)
	h.facts = []ConditionFact{rebootFact(true)}
	h.reconcile(t)
	h.drain(t)
	if _, err := h.db.Exec(`UPDATE settings SET value=json_remove(value, '$.destination_fingerprints') WHERE key=?`, SettingsKey); err != nil {
		t.Fatal(err)
	}
	tx, err := h.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := ReencryptStoredWebhookURL(t.Context(), tx, h.service.deps.DecryptSecret, h.service.deps.EncryptSecret); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ReloadPersistence(t.Context()); err != nil {
		t.Fatal(err)
	}
	_, facts, err := h.service.InstallationHealth(t.Context())
	webhook := facts.Destinations[DestinationWebhook]
	if err != nil || webhook.LastSuccessfulDeliveryAt == nil || !webhook.LastSuccessfulDeliveryAt.Equal(h.now) {
		t.Fatalf("Webhook success after rewrapping restored settings = %+v, %v", webhook, err)
	}
}

func TestDestinationHealthKeepsLatestManualCompletionWhenWritesArriveOutOfOrder(t *testing.T) {
	h := newConditionHarness(t)
	settings, err := h.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	latest := h.now.Add(110 * time.Millisecond)
	for _, status := range []DeliveryStatus{
		{Outcome: DeliveryOutcomeSucceeded, CompletedAt: latest.Format(time.RFC3339Nano)},
		{Outcome: DeliveryOutcomeFailed, CompletedAt: h.now.Add(100 * time.Millisecond).Format(time.RFC3339Nano)},
	} {
		if err := h.service.storeLastDelivery(DestinationWebhook, status, fingerprint); err != nil {
			t.Fatal(err)
		}
	}
	_, facts, err := h.service.InstallationHealth(t.Context())
	webhook := facts.Destinations[DestinationWebhook]
	if err != nil || webhook.LastManualTestAt == nil || !webhook.LastManualTestAt.Equal(latest) ||
		webhook.LastManualTestState != DeliveryOutcomeSucceeded {
		t.Fatalf("last Webhook test after out-of-order writes = %+v, %v", webhook, err)
	}
}

func TestNotificationDeliveryDiagnosticsSelectsLatestFractionalAttempt(t *testing.T) {
	h := newConditionHarness(t)
	older := h.now.Add(100 * time.Millisecond).Format(time.RFC3339Nano)
	newer := h.now.Add(110 * time.Millisecond).Format(time.RFC3339Nano)
	if err := h.service.storeLastDelivery(DestinationWebhook, DeliveryStatus{Destination: DestinationWebhook, AttemptedAt: older}, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.service.storeLastDelivery(DestinationDiscord, DeliveryStatus{Destination: DestinationDiscord, AttemptedAt: newer}, ""); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := h.service.DeliveryDiagnostics()
	if err != nil || diagnostics.LastAttempt == nil || diagnostics.LastAttempt.Destination != DestinationDiscord {
		t.Fatalf("latest attempt = %+v, %v", diagnostics.LastAttempt, err)
	}
}

func TestDestinationHealthPersistsAtomicallyWithCompletion(t *testing.T) {
	h := newConditionHarness(t)
	now := h.now.Format(time.RFC3339Nano)
	id := insertPendingOutboxIntent(t, h.db, DeliveryIntent{ID: "atomic-destination-success", Action: EventUpdateComplete}, h.now)
	if row, err := claimNextOutboxRow(t.Context(), h.db, h.now); err != nil || row == nil || row.ID != id {
		t.Fatalf("claim = %v, %v", row, err)
	}
	if _, err := h.db.Exec(`CREATE TRIGGER block_destination_health BEFORE INSERT ON notification_destination_health
		BEGIN SELECT RAISE(ABORT, 'destination health write failed'); END`); err != nil {
		t.Fatal(err)
	}
	outcome := outboxOutcome{State: outboxStateSucceeded, StatusCode: 204, AttemptedAt: now, CompletedAt: now, DestinationFingerprint: "configuration"}
	if err := completeOutboxRow(t.Context(), h.db, id, outcome, now); err == nil {
		t.Fatal("outcome completed without persisting the destination observation")
	}
	var state, lastSuccess string
	if err := h.db.QueryRow(`SELECT state, (SELECT last_success_at FROM notification_delivery_health WHERE id=1)
		 FROM notification_outbox WHERE id=?`, id).Scan(&state, &lastSuccess); err != nil || state != outboxStateClaimed || lastSuccess != "" {
		t.Fatalf("outbox state = %q, success = %q, error = %v", state, lastSuccess, err)
	}
}

func TestDestinationHealthSurvivesRestoreAfterHistoryPrune(t *testing.T) {
	for _, offline := range []bool{false, true} {
		name := "stage full restore"
		if offline {
			name = "offline recovery"
		}
		t.Run(name, func(t *testing.T) {
			live, staged := newConditionHarness(t), newConditionHarness(t)
			live.facts = []ConditionFact{rebootFact(true)}
			live.reconcile(t)
			live.drain(t)
			success := live.now
			live.now = live.now.Add(2 * time.Hour)
			if n, err := pruneOutbox(t.Context(), live.db, live.now, time.Hour); n != 1 || err != nil {
				t.Fatalf("pruned %d rows: %v", n, err)
			}
			updated, err := staged.service.SaveSettings(SettingsUpdate{Enabled: true, EventTypes: SupportedEvents(),
				WebhookURL: "https://replacement.example.test/hook", WebhookURLIntent: WebhookURLReplace})
			if err != nil || !updated.WebhookConfigured {
				t.Fatalf("replacement settings = %+v, %v", updated, err)
			}
			if offline {
				if err := MergeOfflineDeliveryEvidence(t.Context(), live.db, staged.db, live.now); err != nil {
					t.Fatal(err)
				}
			} else {
				_, evidence, err := loadPersistenceEvidence(t.Context(), live.db)
				if err != nil {
					t.Fatal(err)
				}
				if err := stageAPTDeliveryEvidence(t.Context(), staged.db, &evidence, live.now); err != nil {
					t.Fatal(err)
				}
			}
			_, facts, err := staged.service.InstallationHealth(t.Context())
			webhook := facts.Destinations[DestinationWebhook]
			if err != nil || webhook.LastSuccessfulDeliveryAt != nil || webhook.LastAttemptState != DeliveryHealthUnknown ||
				webhook.HistoricalSuccessAt == nil || !webhook.HistoricalSuccessAt.Equal(success) {
				t.Fatalf("restored Webhook health = %+v, %v", webhook, err)
			}
		})
	}
}

func TestDestinationHealthRestoreLeavesConflictingTiedOutcomesUnknown(t *testing.T) {
	live, restored := newConditionHarness(t), newConditionHarness(t)
	settings, err := live.service.currentSettings()
	if err != nil {
		t.Fatal(err)
	}
	fingerprint := destinationConfigFingerprint(settings, DestinationWebhook)
	legacyAt := live.now.Format(time.RFC3339)
	for _, outcome := range []struct {
		harness    *conditionHarness
		state      string
		statusCode int
	}{
		{live, outboxStateFailed, 500},
		{restored, outboxStateSucceeded, 204},
	} {
		if _, err := outcome.harness.db.Exec(`
			DROP TABLE notification_destination_health;
			INSERT INTO notification_outbox
				(id, destination, destination_fingerprint, event_type, action, state, attempted_at, completed_at, status_code, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			outcome.state, DestinationWebhook, fingerprint, EventUpdateComplete, EventUpdateComplete, outcome.state,
			legacyAt, legacyAt, outcome.statusCode, legacyAt, legacyAt); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if err := MergeOfflineDeliveryEvidence(t.Context(), live.db, restored.db, live.now); err != nil {
			t.Fatal(err)
		}
		_, facts, err := restored.service.InstallationHealth(t.Context())
		webhook := facts.Destinations[DestinationWebhook]
		if err != nil || webhook.LastSuccessfulDeliveryAt == nil || !webhook.LastSuccessfulDeliveryAt.Equal(live.now) ||
			webhook.LastAttemptState != DeliveryHealthUnknown || webhook.LastAttemptAt != nil {
			t.Fatalf("conflicting restored Webhook outcomes = %+v, %v", webhook, err)
		}
	}

	restored.facts = []ConditionFact{rebootFact(true)}
	restored.reconcile(t)
	restored.drain(t)
	_, facts, err := restored.service.InstallationHealth(t.Context())
	webhook := facts.Destinations[DestinationWebhook]
	if err != nil || webhook.LastAttemptState != DeliveryOutcomeSucceeded || webhook.LastAttemptAt == nil ||
		!webhook.LastAttemptAt.Equal(restored.now) {
		t.Fatalf("new attempt after reconciling restored evidence = %+v, %v", webhook, err)
	}
}
