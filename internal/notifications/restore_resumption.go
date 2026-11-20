package notifications

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type RestoreDeliveryQueue struct {
	Destination      string     `json:"destination"`
	Configured       bool       `json:"configured"`
	Pending          int        `json:"pending"`
	Retrying         int        `json:"retrying"`
	Claimed          int        `json:"claimed"`
	RateLimitedUntil *time.Time `json:"rate_limited_until,omitempty"`
}

// RestoreDeliveryResumption reads the paused queues and their configuration
// without decrypting secrets, migrating settings or waking the worker.
func (s *Service) RestoreDeliveryResumption(ctx context.Context, now time.Time) ([]RestoreDeliveryQueue, string, error) {
	db := s.database()
	if db == nil {
		return nil, "", errors.New("notification persistence unavailable")
	}
	// Read memory first so a completion committed during the read still
	// appears in either the in-memory or persisted view.
	runtimePauses := s.rateLimitCompletions.snapshot()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, "", err
	}
	defer tx.Rollback()
	var raw string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", SettingsKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, "", err
	}
	var settings persistedSettings
	if raw != "" {
		if err := json.Unmarshal([]byte(raw), &settings); err != nil {
			return nil, "", err
		}
	}
	revision := sha256.New()
	// Credential fingerprints identify the delivery target across writes
	// that encrypt the same settings again. Older files without fingerprints
	// still bind the stored credentials, without decrypting them.
	configuration := settings
	configuration.LastDelivery, configuration.LastDeliveries, configuration.LastManualTests = nil, nil, nil
	if settings.DestinationFingerprints[DestinationWebhook] != "" {
		configuration.LegacyWebhookURL, configuration.EncryptedWebhookURL = "", ""
	}
	if settings.DestinationFingerprints[DestinationDiscord] != "" {
		configuration.DiscordWebhookURL = ""
	}
	if settings.DestinationFingerprints[DestinationTelegram] != "" {
		configuration.TelegramBotToken, configuration.TelegramChatID = "", ""
	}
	if err := json.NewEncoder(revision).Encode(configuration); err != nil {
		return nil, "", err
	}
	queues := make([]RestoreDeliveryQueue, 0, len(healthDestinations))
	for _, destination := range healthDestinations {
		configured, enabled := settings.destinationConfiguration(destination)
		if !enabled {
			continue
		}
		fact := RestoreDeliveryQueue{Destination: destination, Configured: configured}
		fingerprint := settings.DestinationFingerprints[destination]
		var pause string
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE((SELECT rate_limited_until FROM notification_destination_health
				WHERE destination=? AND fingerprint=?), '')
		`, destination, fingerprint).Scan(&pause); err != nil {
			return nil, "", err
		}
		deadline := runtimePauses[rateLimitKey{destination: destination, fingerprint: fingerprint}].deadline
		if at := deliveryHealthTime(pause); at != nil && at.After(deadline) {
			deadline = *at
		}
		if deadline.After(now) {
			deadline = deadline.UTC()
			fact.RateLimitedUntil = &deadline
		}
		if configured {
			if err := appendRestoreDeliveryQueue(ctx, tx, &fact, fingerprint, json.NewEncoder(revision)); err != nil {
				return nil, "", err
			}
		}
		queues = append(queues, fact)
	}
	return queues, fmt.Sprintf("%x", revision.Sum(nil)), tx.Commit()
}

func appendRestoreDeliveryQueue(ctx context.Context, tx *sql.Tx, fact *RestoreDeliveryQueue, fingerprint string, revision *json.Encoder) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,state,next_attempt_at,claimed_at FROM notification_outbox
		WHERE state IN ('pending','retrying','claimed') AND destination=?
		  AND (?='' OR destination_fingerprint='' OR destination_fingerprint=?)
		ORDER BY id
	`, fact.Destination, fingerprint, fingerprint)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var row [4]string
		if err := rows.Scan(&row[0], &row[1], &row[2], &row[3]); err != nil {
			return err
		}
		switch row[1] {
		case outboxStatePending:
			fact.Pending++
		case outboxStateRetrying:
			fact.Retrying++
		case outboxStateClaimed:
			fact.Claimed++
		}
		if err := revision.Encode(row); err != nil {
			return err
		}
	}
	return rows.Err()
}
