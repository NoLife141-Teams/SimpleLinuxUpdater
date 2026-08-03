package notifications

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	outboxStatePending   = "pending"
	outboxStateClaimed   = "claimed"
	outboxStateRetrying  = "retrying"
	outboxStateSucceeded = "succeeded"
	outboxStateFailed    = "failed"
	outboxStateSkipped   = "skipped"

	defaultOutboxClaimTTL  = 2 * time.Minute
	defaultOutboxRetention = 30 * 24 * time.Hour
)

type outboxRow struct {
	ID            string
	Destination   string
	EventType     string
	Intent        DeliveryIntent
	State         string
	Attempts      int
	NextAttemptAt string
	ClaimedAt     string
}

type outboxOutcome struct {
	State         string
	Attempts      int
	NextAttemptAt string
	AttemptedAt   string
	CompletedAt   string
	StatusCode    int
	Error         string
	DurationMS    int64
}

type outboxCounts struct {
	Pending  int
	Retrying int
}

func newOutboxID() string {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err == nil {
		return hex.EncodeToString(value)
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func EnsureSchema(db *sql.DB) error {
	if db == nil {
		return nil
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS notification_outbox (
			id TEXT PRIMARY KEY,
			destination TEXT NOT NULL,
			event_type TEXT NOT NULL,
			action TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			event_created_at TEXT NOT NULL DEFAULT '',
			actor TEXT NOT NULL DEFAULT '',
			client_ip TEXT NOT NULL DEFAULT '',
			meta_json TEXT NOT NULL DEFAULT '{}',
			state TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0,
			next_attempt_at TEXT NOT NULL DEFAULT '',
			claimed_at TEXT NOT NULL DEFAULT '',
			attempted_at TEXT NOT NULL DEFAULT '',
			completed_at TEXT NOT NULL DEFAULT '',
			status_code INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			duration_ms INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)
	`); err != nil {
		return fmt.Errorf("create notification outbox: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_notification_outbox_due
		ON notification_outbox (state, next_attempt_at, created_at, id)`); err != nil {
		return fmt.Errorf("index notification outbox due rows: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_notification_outbox_terminal
		ON notification_outbox (state, completed_at)`); err != nil {
		return fmt.Errorf("index notification outbox terminal rows: %w", err)
	}
	return nil
}

func buildOutboxRows(intent DeliveryIntent, eventType string, destinations []string, now time.Time) ([]outboxRow, error) {
	if strings.TrimSpace(intent.ID) == "" {
		return nil, errors.New("notification delivery identity is required")
	}
	if strings.TrimSpace(intent.CreatedAt) == "" {
		intent.CreatedAt = now.UTC().Format(time.RFC3339Nano)
	}
	payload, err := buildPayload(eventType, intent)
	if err != nil {
		return nil, err
	}
	metaJSON, err := json.Marshal(payload.Meta)
	if err != nil {
		return nil, err
	}
	intent.MetaJSON = string(metaJSON)
	rows := make([]outboxRow, 0, len(destinations))
	for _, destination := range destinations {
		destination = strings.TrimSpace(destination)
		digest := sha256.Sum256([]byte(intent.ID + "\x00" + destination))
		rows = append(rows, outboxRow{
			ID:          hex.EncodeToString(digest[:]),
			Destination: destination,
			EventType:   eventType,
			Intent:      intent,
			State:       outboxStatePending,
		})
	}
	return rows, nil
}

func insertOutboxRows(ctx context.Context, db *sql.DB, rows []outboxRow, now string) error {
	if db == nil {
		return errors.New("notification outbox database is unavailable")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (
				id, destination, event_type, action, target_type, target_name, status, message,
				event_created_at, actor, client_ip, meta_json, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING
		`, row.ID, row.Destination, row.EventType, row.Intent.Action, row.Intent.TargetType,
			row.Intent.TargetName, row.Intent.Status, row.Intent.Message, row.Intent.CreatedAt,
			row.Intent.Actor, row.Intent.ClientIP, row.Intent.MetaJSON, outboxStatePending, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func recoverExpiredOutboxClaims(ctx context.Context, db *sql.DB, now time.Time, claimTTL time.Duration) error {
	if db == nil {
		return nil
	}
	cutoff := now.Add(-claimTTL).UTC().Format(time.RFC3339Nano)
	nowText := now.UTC().Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, claimed_at = '', next_attempt_at = ?, updated_at = ?
		 WHERE state = ? AND claimed_at <= ?
	`, outboxStateRetrying, nowText, nowText, outboxStateClaimed, cutoff)
	return err
}

func claimNextOutboxRow(ctx context.Context, db *sql.DB, now time.Time) (*outboxRow, error) {
	if db == nil {
		return nil, nil
	}
	nowText := now.UTC().Format(time.RFC3339Nano)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var row outboxRow
	err = tx.QueryRowContext(ctx, `
		SELECT id, destination, event_type, action, target_type, target_name, status, message,
		       event_created_at, actor, client_ip, meta_json, state, attempts, next_attempt_at, claimed_at
		  FROM notification_outbox
		 WHERE state IN (?, ?) AND (next_attempt_at = '' OR next_attempt_at <= ?)
		 ORDER BY rowid
		 LIMIT 1
	`, outboxStatePending, outboxStateRetrying, nowText).Scan(
		&row.ID, &row.Destination, &row.EventType, &row.Intent.Action, &row.Intent.TargetType,
		&row.Intent.TargetName, &row.Intent.Status, &row.Intent.Message, &row.Intent.CreatedAt,
		&row.Intent.Actor, &row.Intent.ClientIP, &row.Intent.MetaJSON, &row.State, &row.Attempts,
		&row.NextAttemptAt, &row.ClaimedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE notification_outbox SET state = ?, claimed_at = ?, updated_at = ?
		 WHERE id = ? AND state IN (?, ?)
	`, outboxStateClaimed, nowText, nowText, row.ID, outboxStatePending, outboxStateRetrying)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		if err == nil {
			err = errors.New("notification outbox claim conflict")
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	row.State = outboxStateClaimed
	row.ClaimedAt = nowText
	return &row, nil
}

func completeOutboxRow(ctx context.Context, db *sql.DB, id string, outcome outboxOutcome, now string) error {
	result, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, attempts = ?, next_attempt_at = ?, claimed_at = '', attempted_at = ?,
		       completed_at = ?, status_code = ?, error = ?, duration_ms = ?, updated_at = ?
		 WHERE id = ? AND state = ?
	`, outcome.State, outcome.Attempts, outcome.NextAttemptAt, outcome.AttemptedAt,
		outcome.CompletedAt, outcome.StatusCode, outcome.Error, outcome.DurationMS, now, id, outboxStateClaimed)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("notification outbox completion lost its claim")
	}
	return nil
}

func releaseOutboxClaim(ctx context.Context, db *sql.DB, id, nextAttemptAt, now string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, claimed_at = '', next_attempt_at = ?, updated_at = ?
		 WHERE id = ? AND state = ?
	`, outboxStateRetrying, nextAttemptAt, now, id, outboxStateClaimed)
	return err
}

func loadOutboxCounts(ctx context.Context, db *sql.DB) (outboxCounts, error) {
	var counts outboxCounts
	if db == nil {
		return counts, nil
	}
	err := db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN state IN (?, ?) THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN state = ? THEN 1 ELSE 0 END), 0)
		FROM notification_outbox
	`, outboxStatePending, outboxStateClaimed, outboxStateRetrying).Scan(&counts.Pending, &counts.Retrying)
	return counts, err
}

func nextOutboxWake(ctx context.Context, db *sql.DB, claimTTL time.Duration) (time.Time, bool, error) {
	if db == nil {
		return time.Time{}, false, nil
	}
	var nextAttempt, oldestClaim string
	err := db.QueryRowContext(ctx, `
		SELECT
			COALESCE(MIN(CASE WHEN state = ? AND next_attempt_at != '' THEN next_attempt_at END), ''),
			COALESCE(MIN(CASE WHEN state = ? AND claimed_at != '' THEN claimed_at END), '')
		  FROM notification_outbox
	`, outboxStateRetrying, outboxStateClaimed).Scan(&nextAttempt, &oldestClaim)
	if err != nil {
		return time.Time{}, false, err
	}
	var wake time.Time
	if nextAttempt != "" {
		parsed, err := time.Parse(time.RFC3339Nano, nextAttempt)
		if err != nil {
			return time.Time{}, false, err
		}
		wake = parsed
	}
	if oldestClaim != "" {
		parsed, err := time.Parse(time.RFC3339Nano, oldestClaim)
		if err != nil {
			return time.Time{}, false, err
		}
		claimWake := parsed.Add(claimTTL)
		if wake.IsZero() || claimWake.Before(wake) {
			wake = claimWake
		}
	}
	return wake, !wake.IsZero(), nil
}

func pruneOutbox(ctx context.Context, db *sql.DB, now time.Time, retention time.Duration) (int64, error) {
	if db == nil || retention <= 0 {
		return 0, nil
	}
	cutoff := now.Add(-retention).UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(ctx, `
		DELETE FROM notification_outbox
		 WHERE state IN (?, ?, ?) AND completed_at != '' AND completed_at < ?
	`, outboxStateSucceeded, outboxStateFailed, outboxStateSkipped, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
