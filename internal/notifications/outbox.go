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

	outboxSkipConditionIneligible     = "condition_ineligible"
	outboxSkipDestinationDisabled     = "destination_disabled"
	outboxSkipDestinationUnconfigured = "destination_unconfigured"
	outboxSkipDestinationChanged      = "destination_configuration_changed"

	defaultOutboxClaimTTL  = 2 * time.Minute
	defaultOutboxRetention = 30 * 24 * time.Hour
)

type outboxRow struct {
	ID                     string
	Destination            string
	DestinationFingerprint string
	EventType              string
	Intent                 DeliveryIntent
	State                  string
	Attempts               int
	NextAttemptAt          string
	ClaimedAt              string
}

type outboxOutcome struct {
	State                  string
	Attempts               int
	NextAttemptAt          string
	AttemptedAt            string
	CompletedAt            string
	StatusCode             int
	Error                  string
	DurationMS             int64
	DestinationFingerprint string
}

type outboxSnapshot struct {
	Row       outboxRow
	Outcome   outboxOutcome
	CreatedAt string
}

type outboxReplacement struct {
	RetainedRows           map[string]outboxSnapshot
	TerminalOutcomes       map[string]outboxOutcome
	APTAdmissions          []string
	LastSuccessfulDelivery string
	DestinationHealth      []destinationHealthEvidence
}

func loadPersistenceEvidence(ctx context.Context, db *sql.DB) (conditionState, outboxReplacement, error) {
	conditions, err := loadAndMigrateConditionState(ctx, db)
	if err != nil {
		return conditionState{}, outboxReplacement{}, err
	}
	if err := migrateApprovalOutboxHistory(ctx, db, &conditions); err != nil {
		return conditionState{}, outboxReplacement{}, err
	}
	rows, err := loadCurrentOutboxRows(ctx, db)
	if err != nil {
		return conditionState{}, outboxReplacement{}, err
	}
	rows.APTAdmissions, err = loadAPTAdmissions(ctx, db)
	if err == nil && db != nil {
		err = db.QueryRowContext(ctx, "SELECT last_success_at FROM notification_delivery_health WHERE id=1").
			Scan(&rows.LastSuccessfulDelivery)
		if err == nil {
			rows.DestinationHealth, err = loadDestinationHealthEvidence(ctx, db)
		}
	}
	return conditions, rows, err
}

func loadCurrentOutboxRows(ctx context.Context, db *sql.DB) (outboxReplacement, error) {
	replacement := outboxReplacement{
		RetainedRows:     make(map[string]outboxSnapshot),
		TerminalOutcomes: make(map[string]outboxOutcome),
	}
	if db == nil {
		return replacement, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, destination, event_type, action, target_type, target_name, status, message,
		       event_created_at, actor, client_ip, meta_json, destination_fingerprint, state,
		       attempts, next_attempt_at, claimed_at, attempted_at, completed_at, status_code,
		       error, duration_ms, created_at
		  FROM notification_outbox
	`)
	if err != nil {
		return outboxReplacement{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot outboxSnapshot
		if err := rows.Scan(
			&snapshot.Row.ID, &snapshot.Row.Destination, &snapshot.Row.EventType,
			&snapshot.Row.Intent.Action, &snapshot.Row.Intent.TargetType, &snapshot.Row.Intent.TargetName,
			&snapshot.Row.Intent.Status, &snapshot.Row.Intent.Message, &snapshot.Row.Intent.CreatedAt,
			&snapshot.Row.Intent.Actor, &snapshot.Row.Intent.ClientIP, &snapshot.Row.Intent.MetaJSON,
			&snapshot.Row.DestinationFingerprint, &snapshot.Outcome.State, &snapshot.Outcome.Attempts,
			&snapshot.Outcome.NextAttemptAt, &snapshot.Row.ClaimedAt, &snapshot.Outcome.AttemptedAt,
			&snapshot.Outcome.CompletedAt, &snapshot.Outcome.StatusCode, &snapshot.Outcome.Error,
			&snapshot.Outcome.DurationMS, &snapshot.CreatedAt,
		); err != nil {
			return outboxReplacement{}, err
		}
		if snapshot.Outcome.State == outboxStateSucceeded || snapshot.Outcome.State == outboxStateFailed || snapshot.Outcome.State == outboxStateSkipped {
			replacement.TerminalOutcomes[snapshot.Row.ID] = snapshot.Outcome
			if snapshot.Row.EventType != EventAPTReconciliationRequired {
				continue
			}
		}
		if snapshot.Outcome.State == outboxStateClaimed {
			snapshot.Outcome.State = outboxStateRetrying
			snapshot.Outcome.NextAttemptAt = ""
		}
		if snapshot.Outcome.State == outboxStateRetrying {
			snapshot.Row.ClaimedAt = ""
			snapshot.Outcome.CompletedAt = ""
		}
		snapshot.Row.State = snapshot.Outcome.State
		snapshot.Row.Attempts = snapshot.Outcome.Attempts
		snapshot.Row.NextAttemptAt = snapshot.Outcome.NextAttemptAt
		replacement.RetainedRows[snapshot.Row.ID] = snapshot
	}
	return replacement, rows.Err()
}

func (replacement *outboxReplacement) suppressExpiredAPTDeliveries(ctx context.Context, tx *sql.Tx, now string) error {
	if len(replacement.APTAdmissions) == 0 {
		return nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT id FROM notification_outbox WHERE event_type=? AND state IN (?, ?, ?)`,
		EventAPTReconciliationRequired, outboxStatePending, outboxStateClaimed, outboxStateRetrying)
	if err != nil {
		return err
	}
	defer rows.Close()
	pending := make(map[string]bool)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return err
		}
		if _, retained := replacement.RetainedRows[id]; !retained {
			pending[id] = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, identity := range replacement.APTAdmissions {
		if len(pending) == 0 {
			break
		}
		for _, destination := range []string{DestinationWebhook, DestinationDiscord, DestinationTelegram} {
			id := outboxDeliveryID(identity, destination)
			if pending[id] {
				// The live ledger knows this admission even after its history has
				// expired. An older archive cannot put it back into the queue.
				if _, err := tx.ExecContext(ctx, `UPDATE notification_outbox
					SET state=?, attempts=0, next_attempt_at='', claimed_at='', attempted_at='', completed_at=?,
					    status_code=0, error=?, duration_ms=0, updated_at=?
					WHERE id=?`, outboxStateSkipped, now, outboxSkipConditionIneligible, now, id); err != nil {
					return err
				}
				delete(pending, id)
			}
		}
	}
	return nil
}

func restoreCurrentOutboxRows(ctx context.Context, db *sql.DB, replacement *outboxReplacement, now string) error {
	if db == nil || replacement == nil {
		return nil
	}
	if len(replacement.RetainedRows) == 0 && len(replacement.TerminalOutcomes) == 0 &&
		len(replacement.APTAdmissions) == 0 && replacement.LastSuccessfulDelivery == "" && len(replacement.DestinationHealth) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, snapshot := range replacement.RetainedRows {
		row := snapshot.Row
		outcome := snapshot.Outcome
		// Deterministic APT admissions can share delivery history across both
		// databases. Keep success, and never reopen a terminal result or roll
		// back the attempt count when the incoming copy is still in progress.
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (
				id, destination, event_type, action, target_type, target_name, status, message,
				event_created_at, actor, client_ip, meta_json, destination_fingerprint, state,
				attempts, next_attempt_at, claimed_at, attempted_at, completed_at, status_code,
				error, duration_ms, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET
				destination = excluded.destination,
				event_type = excluded.event_type,
				action = excluded.action,
				target_type = excluded.target_type,
				target_name = excluded.target_name,
				status = excluded.status,
				message = excluded.message,
				event_created_at = excluded.event_created_at,
				actor = excluded.actor,
				client_ip = excluded.client_ip,
				meta_json = excluded.meta_json,
				destination_fingerprint = excluded.destination_fingerprint,
				state = excluded.state,
				attempts = excluded.attempts,
				next_attempt_at = excluded.next_attempt_at,
				claimed_at = excluded.claimed_at,
				attempted_at = excluded.attempted_at,
				completed_at = excluded.completed_at,
				status_code = excluded.status_code,
				error = excluded.error,
				duration_ms = excluded.duration_ms,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at
			WHERE NOT (notification_outbox.event_type = ? AND excluded.event_type = ? AND (
				(notification_outbox.state = ? AND excluded.state <> ?)
				OR (excluded.state IN (?, ?, ?) AND (
					notification_outbox.state IN (?, ?) OR notification_outbox.attempts > excluded.attempts
				))
			))
		`, row.ID, row.Destination, row.EventType, row.Intent.Action, row.Intent.TargetType,
			row.Intent.TargetName, row.Intent.Status, row.Intent.Message, row.Intent.CreatedAt,
			row.Intent.Actor, row.Intent.ClientIP, row.Intent.MetaJSON, row.DestinationFingerprint,
			outcome.State, outcome.Attempts, outcome.NextAttemptAt, row.ClaimedAt,
			outcome.AttemptedAt, outcome.CompletedAt, outcome.StatusCode, outcome.Error,
			outcome.DurationMS, snapshot.CreatedAt, now,
			EventAPTReconciliationRequired, EventAPTReconciliationRequired, outboxStateSucceeded, outboxStateSucceeded,
			outboxStatePending, outboxStateClaimed, outboxStateRetrying, outboxStateFailed, outboxStateSkipped); err != nil {
			return err
		}
	}
	for id, outcome := range replacement.TerminalOutcomes {
		if _, err := tx.ExecContext(ctx, `
			UPDATE notification_outbox
			   SET state = ?, attempts = ?, next_attempt_at = ?, claimed_at = '', attempted_at = ?,
			       completed_at = ?, status_code = ?, error = ?, duration_ms = ?, updated_at = ?
			 WHERE id = ? AND state IN (?, ?, ?)
		`, outcome.State, outcome.Attempts, outcome.NextAttemptAt, outcome.AttemptedAt,
			outcome.CompletedAt, outcome.StatusCode, outcome.Error, outcome.DurationMS, now, id,
			outboxStatePending, outboxStateClaimed, outboxStateRetrying); err != nil {
			return err
		}
	}
	if err := replacement.suppressExpiredAPTDeliveries(ctx, tx, now); err != nil {
		return err
	}
	if err := mergeAPTAdmissionsTx(ctx, tx, replacement.APTAdmissions); err != nil {
		return err
	}
	if err := persistSuccessfulDelivery(ctx, tx, replacement.LastSuccessfulDelivery); err != nil {
		return err
	}
	for _, event := range replacement.DestinationHealth {
		if err := persistDestinationHealth(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
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
	return ensureSchema(context.Background(), db)
}

func ensureSchema(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return nil
	}
	if err := ensureConditionSchema(ctx, db); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `
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
			destination_fingerprint TEXT NOT NULL DEFAULT '',
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
	if err := ensureOutboxDestinationFingerprintColumn(ctx, db); err != nil {
		return fmt.Errorf("migrate notification outbox destination binding: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notification_outbox_due
		ON notification_outbox (state, next_attempt_at, created_at, id)`); err != nil {
		return fmt.Errorf("index notification outbox due rows: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notification_outbox_terminal
		ON notification_outbox (state, completed_at)`); err != nil {
		return fmt.Errorf("index notification outbox terminal rows: %w", err)
	}
	// RFC3339Nano omits trailing fractional zeroes. Removing the final Z
	// preserves chronological ordering for both whole and fractional seconds.
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notification_outbox_history
		ON notification_outbox (rtrim(created_at, 'Z') DESC, id DESC)`); err != nil {
		return fmt.Errorf("index notification outbox history: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notification_delivery_health (
			id INTEGER PRIMARY KEY CHECK(id=1),
			last_success_at TEXT NOT NULL
		);
		INSERT INTO notification_delivery_health(id, last_success_at)
		SELECT 1, COALESCE((
			SELECT completed_at FROM notification_outbox
			WHERE state='succeeded' AND status_code BETWEEN 200 AND 299
			ORDER BY rtrim(completed_at, 'Z') DESC LIMIT 1
		), '')
		WHERE NOT EXISTS (SELECT 1 FROM notification_delivery_health WHERE id=1);
	`); err != nil {
		return fmt.Errorf("initialize notification delivery health: %w", err)
	}
	if err := ensureDestinationHealthSchema(ctx, db); err != nil {
		return fmt.Errorf("initialize notification destination health: %w", err)
	}
	return nil
}

func ensureOutboxDestinationFingerprintColumn(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(notification_outbox)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		if name == "destination_fingerprint" {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, `ALTER TABLE notification_outbox ADD COLUMN destination_fingerprint TEXT NOT NULL DEFAULT ''`)
	return err
}

func destinationConfigFingerprint(settings Settings, destination string) string {
	var identity string
	switch destination {
	case DestinationWebhook:
		identity = strings.TrimSpace(settings.WebhookURL)
	case DestinationDiscord:
		identity = strings.TrimSpace(settings.DiscordWebhookURL)
	case DestinationTelegram:
		token := strings.TrimSpace(settings.TelegramBotToken)
		chatID := strings.TrimSpace(settings.TelegramChatID)
		if token != "" && chatID != "" {
			identity = token + "\x00" + chatID
		}
	}
	if identity == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(destination + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}

func outboxDeliveryID(intentID, destination string) string {
	digest := sha256.Sum256([]byte(intentID + "\x00" + destination))
	return hex.EncodeToString(digest[:])
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
		rows = append(rows, outboxRow{
			ID:          outboxDeliveryID(intent.ID, destination),
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
	if err := insertOutboxRowsTx(ctx, tx, rows, now); err != nil {
		return err
	}
	return tx.Commit()
}

func insertOutboxRowsTx(ctx context.Context, tx *sql.Tx, rows []outboxRow, now string) error {
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (
				id, destination, event_type, action, target_type, target_name, status, message,
				event_created_at, actor, client_ip, meta_json, destination_fingerprint, state, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING
		`, row.ID, row.Destination, row.EventType, row.Intent.Action, row.Intent.TargetType,
			row.Intent.TargetName, row.Intent.Status, row.Intent.Message, row.Intent.CreatedAt,
			row.Intent.Actor, row.Intent.ClientIP, row.Intent.MetaJSON, row.DestinationFingerprint,
			outboxStatePending, now, now); err != nil {
			return err
		}
	}
	return nil
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
		       event_created_at, actor, client_ip, meta_json, destination_fingerprint, state, attempts,
		       next_attempt_at, claimed_at
		  FROM notification_outbox
		 WHERE state IN (?, ?) AND (next_attempt_at = '' OR next_attempt_at <= ?)
		 ORDER BY rowid
		 LIMIT 1
	`, outboxStatePending, outboxStateRetrying, nowText).Scan(
		&row.ID, &row.Destination, &row.EventType, &row.Intent.Action, &row.Intent.TargetType,
		&row.Intent.TargetName, &row.Intent.Status, &row.Intent.Message, &row.Intent.CreatedAt,
		&row.Intent.Actor, &row.Intent.ClientIP, &row.Intent.MetaJSON, &row.DestinationFingerprint,
		&row.State, &row.Attempts, &row.NextAttemptAt, &row.ClaimedAt,
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
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var destination string
	err = tx.QueryRowContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, attempts = ?, next_attempt_at = ?, claimed_at = '', attempted_at = ?,
		       completed_at = ?, status_code = ?, error = ?, duration_ms = ?, updated_at = ?
		 WHERE id = ? AND state = ?
		 RETURNING destination
	`, outcome.State, outcome.Attempts, outcome.NextAttemptAt, outcome.AttemptedAt,
		outcome.CompletedAt, outcome.StatusCode, outcome.Error, outcome.DurationMS, now, id, outboxStateClaimed).Scan(&destination)
	if errors.Is(err, sql.ErrNoRows) {
		return errors.New("notification outbox completion lost its claim")
	}
	if err != nil {
		return err
	}
	success := outcome.State == outboxStateSucceeded && outcome.StatusCode >= 200 && outcome.StatusCode < 300
	if success {
		if err := persistSuccessfulDelivery(ctx, tx, outcome.CompletedAt); err != nil {
			return err
		}
	}
	if outcome.DestinationFingerprint != "" && outcome.AttemptedAt != "" && outcome.CompletedAt != "" &&
		(success || outcome.State == outboxStateRetrying || outcome.State == outboxStateFailed) {
		event := destinationHealthEvidence{
			destination: destination, fingerprint: outcome.DestinationFingerprint,
			attemptAt: outcome.CompletedAt, state: outcome.State,
		}
		if success {
			event.successAt = outcome.CompletedAt
		}
		if err := persistDestinationHealth(ctx, tx, event); err != nil {
			return err
		}
	}
	return tx.Commit()
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
