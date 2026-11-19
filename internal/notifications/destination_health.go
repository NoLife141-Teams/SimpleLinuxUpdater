package notifications

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

var healthDestinations = [...]string{DestinationWebhook, DestinationDiscord, DestinationTelegram}

const DeliveryHealthUnknown DeliveryOutcome = "unknown"

func destinationFingerprints(settings Settings) map[string]string {
	fingerprints := make(map[string]string, len(healthDestinations))
	for _, destination := range healthDestinations {
		if fingerprint := destinationConfigFingerprint(settings, destination); fingerprint != "" {
			fingerprints[destination] = fingerprint
		}
	}
	return fingerprints
}

func (s *Service) backfillDestinationFingerprints(ctx context.Context, db *sql.DB) {
	if db == nil {
		return
	}
	var needsBackfill bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM settings WHERE key=? AND json_type(value, '$.destination_fingerprints') IS NOT 'object'
	)`, SettingsKey).Scan(&needsBackfill); err != nil || !needsBackfill {
		return
	}
	if _, err := s.currentSettings(); err != nil {
		s.deps.Logf("notification destination bindings could not be loaded")
	}
}

type DestinationHealth struct {
	Configured               bool            `json:"configured"`
	Enabled                  bool            `json:"enabled"`
	LastSuccessfulDeliveryAt *time.Time      `json:"last_successful_delivery_at,omitempty"`
	HistoricalSuccessAt      *time.Time      `json:"historical_success_at,omitempty"`
	LastAttemptAt            *time.Time      `json:"last_attempt_at,omitempty"`
	LastAttemptState         DeliveryOutcome `json:"last_attempt_state"`
	LastManualTestAt         *time.Time      `json:"last_manual_test_at,omitempty"`
	LastManualTestState      DeliveryOutcome `json:"last_manual_test_state"`
	OldestPendingAgeSeconds  *float64        `json:"oldest_pending_age_seconds,omitempty"`
}

type destinationManualTest struct {
	Fingerprint string          `json:"fingerprint"`
	CompletedAt string          `json:"completed_at"`
	Outcome     DeliveryOutcome `json:"outcome"`
}

type destinationHealthEvidence struct {
	destination string
	fingerprint string
	successAt   string
	attemptAt   string
	state       string
}

func ensureDestinationHealthSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM sqlite_schema WHERE type='table' AND name='notification_destination_health'
	)`).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return nil
	}
	// Older completions were recorded to the second. When distinct outcomes
	// share a timestamp, keep any confirmed success and leave their order unknown.
	if _, err := tx.ExecContext(ctx, `
		CREATE TABLE notification_destination_health (
			destination TEXT NOT NULL CHECK(destination IN ('webhook', 'discord', 'telegram')),
			fingerprint TEXT NOT NULL CHECK(fingerprint <> ''),
			last_success_at TEXT NOT NULL,
			last_attempt_at TEXT NOT NULL,
			last_attempt_state TEXT NOT NULL CHECK(last_attempt_state IN ('succeeded', 'retrying', 'failed', 'unknown')),
			PRIMARY KEY (destination, fingerprint)
		);
		INSERT INTO notification_destination_health
			(destination, fingerprint, last_success_at, last_attempt_at, last_attempt_state)
		WITH attempts AS (
			SELECT destination, destination_fingerprint, rtrim(completed_at, 'Z') AS completed, state,
			       RANK() OVER (PARTITION BY destination, destination_fingerprint
			           ORDER BY rtrim(completed_at, 'Z') DESC) AS recency
			FROM notification_outbox
			WHERE destination IN ('webhook', 'discord', 'telegram') AND destination_fingerprint <> ''
			  AND attempted_at <> '' AND completed_at <> ''
			  AND ((state='succeeded' AND status_code BETWEEN 200 AND 299) OR state IN ('retrying', 'failed'))
		)
		SELECT destination, destination_fingerprint,
		       COALESCE(MAX(completed) FILTER (WHERE state='succeeded') || 'Z', ''),
		       MAX(completed) || 'Z',
		       CASE WHEN MIN(state) FILTER (WHERE recency=1) = MAX(state) FILTER (WHERE recency=1)
		            THEN MIN(state) FILTER (WHERE recency=1) ELSE 'unknown' END
		FROM attempts
		GROUP BY destination, destination_fingerprint;
	`); err != nil {
		return err
	}
	return tx.Commit()
}

func persistDestinationHealth(ctx context.Context, tx *sql.Tx, event destinationHealthEvidence) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO notification_destination_health
			(destination, fingerprint, last_success_at, last_attempt_at, last_attempt_state)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(destination, fingerprint) DO UPDATE SET
			last_success_at=CASE WHEN rtrim(excluded.last_success_at, 'Z') > rtrim(last_success_at, 'Z')
				THEN excluded.last_success_at ELSE last_success_at END,
			last_attempt_state=CASE WHEN rtrim(excluded.last_attempt_at, 'Z') >= rtrim(last_attempt_at, 'Z')
				THEN excluded.last_attempt_state ELSE last_attempt_state END,
			last_attempt_at=CASE WHEN rtrim(excluded.last_attempt_at, 'Z') >= rtrim(last_attempt_at, 'Z')
				THEN excluded.last_attempt_at ELSE last_attempt_at END
	`, event.destination, event.fingerprint, event.successAt, event.attemptAt, event.state)
	return err
}

func loadDestinationHealthEvidence(ctx context.Context, db *sql.DB) ([]destinationHealthEvidence, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT destination, fingerprint, last_success_at, last_attempt_at, last_attempt_state
		FROM notification_destination_health
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var observations []destinationHealthEvidence
	for rows.Next() {
		var event destinationHealthEvidence
		if err := rows.Scan(&event.destination, &event.fingerprint, &event.successAt, &event.attemptAt, &event.state); err != nil {
			return nil, err
		}
		observations = append(observations, event)
	}
	return observations, rows.Err()
}

func loadDestinationHealth(ctx context.Context, tx *sql.Tx, settings persistedSettings, now time.Time) (map[string]DestinationHealth, error) {
	result := make(map[string]DestinationHealth, len(healthDestinations))
	for _, destination := range healthDestinations {
		fingerprint := settings.DestinationFingerprints[destination]
		configured, enabled := settings.destinationConfiguration(destination)
		facts := DestinationHealth{
			Configured: configured, Enabled: enabled,
			LastAttemptState: DeliveryHealthUnknown, LastManualTestState: DeliveryHealthUnknown,
		}
		var success, historical, attempt, attemptState, oldest string
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(last_success_at, ''), COALESCE(last_attempt_at, ''), COALESCE(last_attempt_state, ''),
			       COALESCE((SELECT MAX(rtrim(last_success_at, 'Z')) FROM notification_destination_health
			                 WHERE destination=? AND fingerprint<>? AND last_success_at<>''), ''),
			       COALESCE((SELECT MIN(rtrim(created_at, 'Z')) FROM notification_outbox
			                 WHERE destination=? AND state IN ('pending', 'claimed', 'retrying') AND created_at<>''), '')
			FROM (SELECT 1)
			LEFT JOIN notification_destination_health ON destination=? AND fingerprint=?
		`, destination, fingerprint, destination, destination, fingerprint).
			Scan(&success, &attempt, &attemptState, &historical, &oldest); err != nil {
			return nil, fmt.Errorf("read notification destination health: %w", err)
		}
		if fingerprint != "" || !configured {
			// A configured legacy destination without binding metadata cannot
			// reliably separate its current success from a previous one.
			facts.HistoricalSuccessAt = deliveryHealthTime(historical + "Z")
		}
		facts.LastSuccessfulDeliveryAt = deliveryHealthTime(success)
		if state := deliveryHealthOutcome(DeliveryOutcome(attemptState)); state != DeliveryHealthUnknown {
			facts.LastAttemptAt = deliveryHealthTime(attempt)
			if facts.LastAttemptAt != nil {
				facts.LastAttemptState = state
			}
		}
		if manual := settings.LastManualTests[destination]; fingerprint != "" && manual.Fingerprint == fingerprint {
			facts.LastManualTestAt = deliveryHealthTime(manual.CompletedAt)
			if facts.LastManualTestAt != nil {
				facts.LastManualTestState = deliveryHealthOutcome(manual.Outcome)
			}
		}
		if at := deliveryHealthTime(oldest + "Z"); at != nil {
			age := max(0, now.Sub(*at).Seconds())
			facts.OldestPendingAgeSeconds = &age
		}
		result[destination] = facts
	}
	return result, nil
}

func deliveryHealthTime(value string) *time.Time {
	at, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || at.IsZero() {
		return nil
	}
	return &at
}

func deliveryHealthOutcome(outcome DeliveryOutcome) DeliveryOutcome {
	switch outcome {
	case DeliveryOutcomeSucceeded, DeliveryOutcomeRetrying, DeliveryOutcomeFailed:
		return outcome
	default:
		return DeliveryHealthUnknown
	}
}

func (s persistedSettings) destinationConfiguration(destination string) (configured, enabled bool) {
	switch destination {
	case DestinationWebhook:
		return s.EncryptedWebhookURL != "" || s.LegacyWebhookURL != "", s.Enabled
	case DestinationDiscord:
		return s.DiscordWebhookURL != "", s.DiscordEnabled
	case DestinationTelegram:
		return s.TelegramBotToken != "" && s.TelegramChatID != "", s.TelegramEnabled
	}
	return false, false
}
