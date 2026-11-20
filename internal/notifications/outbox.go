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

	defaultOutboxClaimTTL     = 2 * time.Minute
	defaultOutboxRetention    = 30 * 24 * time.Hour
	indeterminateRetryDelay   = 30 * time.Second
	outboxIndeterminateReason = "delivery_outcome_indeterminate"
)

var errOutboxCompletionLostClaim = errors.New("notification outbox completion lost its claim")
var errOutboxAttemptNotAdmitted = errors.New("notification outbox attempt no longer eligible")

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
	AttemptID              string
	AttemptedAt            string
}

type outboxOutcome struct {
	State                  string
	Attempts               int
	NextAttemptAt          string
	AttemptedAt            string
	LastIndeterminateAt    string
	CompletedAt            string
	StatusCode             int
	Error                  string
	DurationMS             int64
	DestinationFingerprint string
	RateLimitedUntil       string
	AttemptID              string
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
		       attempts, next_attempt_at, claimed_at, attempted_at, last_indeterminate_at, completed_at, status_code,
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
			&snapshot.Outcome.LastIndeterminateAt, &snapshot.Outcome.CompletedAt, &snapshot.Outcome.StatusCode, &snapshot.Outcome.Error,
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
			if snapshot.Outcome.Error != outboxIndeterminateReason {
				snapshot.Outcome.CompletedAt = ""
			}
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
					SET state=?, next_attempt_at='', claimed_at='', attempt_id='', completed_at=?,
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
		// Both copies can retain the same delivery. Keep recorded outcomes
		// and spent attempts, even when the other copy is still in progress.
		result, err := tx.ExecContext(ctx, `
			INSERT INTO notification_outbox (
				id, destination, event_type, action, target_type, target_name, status, message,
				event_created_at, actor, client_ip, meta_json, destination_fingerprint, state,
				attempts, next_attempt_at, claimed_at, attempted_at, last_indeterminate_at, completed_at, status_code,
				error, duration_ms, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
				attempts = max(notification_outbox.attempts, excluded.attempts),
				next_attempt_at = CASE WHEN excluded.state = 'retrying' AND excluded.attempts = notification_outbox.attempts
				    AND rtrim(notification_outbox.next_attempt_at, 'Z') > rtrim(excluded.next_attempt_at, 'Z')
				    THEN notification_outbox.next_attempt_at ELSE excluded.next_attempt_at END,
				claimed_at = excluded.claimed_at,
				attempt_id = '',
				attempted_at = excluded.attempted_at,
				last_indeterminate_at = CASE WHEN rtrim(notification_outbox.last_indeterminate_at, 'Z') >
				    rtrim(excluded.last_indeterminate_at, 'Z') THEN notification_outbox.last_indeterminate_at
				    ELSE excluded.last_indeterminate_at END,
				completed_at = excluded.completed_at,
				status_code = excluded.status_code,
				error = excluded.error,
				duration_ms = excluded.duration_ms,
				created_at = excluded.created_at,
				updated_at = excluded.updated_at
			WHERE NOT (
				(notification_outbox.state = ? AND excluded.state <> ?)
				OR (excluded.state IN (?, ?, ?) AND (
					notification_outbox.state IN (?, ?) OR notification_outbox.attempts > excluded.attempts
				))
				OR (notification_outbox.state = ? AND notification_outbox.error <> ? AND excluded.error = ?)
				OR (notification_outbox.state = ? AND excluded.state = ? AND excluded.error = ?
				    AND notification_outbox.error <> ?
				    AND notification_outbox.attempts = excluded.attempts
				    AND notification_outbox.attempted_at <> ''
				    AND notification_outbox.attempted_at = excluded.attempted_at
				    AND notification_outbox.destination_fingerprint = excluded.destination_fingerprint
				    AND (notification_outbox.status_code <> 0 OR notification_outbox.error <> ''))
			)
		`, row.ID, row.Destination, row.EventType, row.Intent.Action, row.Intent.TargetType,
			row.Intent.TargetName, row.Intent.Status, row.Intent.Message, row.Intent.CreatedAt,
			row.Intent.Actor, row.Intent.ClientIP, row.Intent.MetaJSON, row.DestinationFingerprint,
			outcome.State, outcome.Attempts, outcome.NextAttemptAt, row.ClaimedAt,
			outcome.AttemptedAt, outcome.LastIndeterminateAt, outcome.CompletedAt, outcome.StatusCode, outcome.Error,
			outcome.DurationMS, snapshot.CreatedAt, now,
			outboxStateSucceeded, outboxStateSucceeded,
			outboxStatePending, outboxStateClaimed, outboxStateRetrying, outboxStateFailed, outboxStateSkipped,
			outboxStateFailed, outboxIndeterminateReason, outboxIndeterminateReason,
			outboxStateRetrying, outboxStateRetrying, outboxIndeterminateReason, outboxIndeterminateReason)
		if err != nil {
			return err
		}
		if err := mergeUnappliedAttemptEvidence(ctx, tx, result, row.ID, outcome); err != nil {
			return err
		}
	}
	for id, outcome := range replacement.TerminalOutcomes {
		// The archive may have captured this last attempt before its result was
		// recorded. That recovered failure stands only without live evidence of
		// successful delivery or a recorded completion of the same attempt.
		recorded := (outcome.State == outboxStateSucceeded || outcome.State == outboxStateFailed) &&
			outcome.Error != outboxIndeterminateReason && outcome.AttemptedAt != "" && outcome.CompletedAt != ""
		result, err := tx.ExecContext(ctx, `
			UPDATE notification_outbox
			   SET state = ?, attempts = max(attempts, ?), next_attempt_at = ?, claimed_at = '', attempt_id = '', attempted_at = ?,
			       last_indeterminate_at = CASE WHEN rtrim(last_indeterminate_at, 'Z') < rtrim(?, 'Z')
			           THEN ? ELSE last_indeterminate_at END,
			       completed_at = ?, status_code = ?, error = ?, duration_ms = ?, updated_at = ?
			 WHERE id = ? AND (state IN (?, ?, ?) OR (
			     state = ? AND error = ? AND ? AND (? OR (attempts = ? AND attempted_at = ?))
			 ))
		`, outcome.State, outcome.Attempts, outcome.NextAttemptAt, outcome.AttemptedAt, outcome.LastIndeterminateAt, outcome.LastIndeterminateAt,
			outcome.CompletedAt, outcome.StatusCode, outcome.Error, outcome.DurationMS, now, id,
			outboxStatePending, outboxStateClaimed, outboxStateRetrying,
			outboxStateFailed, outboxIndeterminateReason, recorded, outcome.State == outboxStateSucceeded, outcome.Attempts, outcome.AttemptedAt)
		if err != nil {
			return err
		}
		if err := mergeUnappliedAttemptEvidence(ctx, tx, result, id, outcome); err != nil {
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
		if err := persistDestinationHealth(ctx, tx, event, false); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// The archive can have a more conclusive result (for example, a recorded
// success). Keep it while preserving attempts and interruption evidence seen
// in the live installation.
func mergeUnappliedAttemptEvidence(ctx context.Context, tx *sql.Tx, result sql.Result, id string, outcome outboxOutcome) error {
	if outcome.Attempts == 0 && outcome.LastIndeterminateAt == "" {
		return nil
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 0 {
		return err
	}
	_, err = tx.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET attempts = max(attempts, ?),
		       last_indeterminate_at = CASE WHEN rtrim(last_indeterminate_at, 'Z') < rtrim(?, 'Z')
		           THEN ? ELSE last_indeterminate_at END
		 WHERE id = ? AND (attempts < ? OR rtrim(last_indeterminate_at, 'Z') < rtrim(?, 'Z'))
	`, outcome.Attempts, outcome.LastIndeterminateAt, outcome.LastIndeterminateAt,
		id, outcome.Attempts, outcome.LastIndeterminateAt)
	return err
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
			attempt_id TEXT NOT NULL DEFAULT '',
			attempted_at TEXT NOT NULL DEFAULT '',
			last_indeterminate_at TEXT NOT NULL DEFAULT '',
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
	if err := migrateOutboxColumns(ctx, db); err != nil {
		return fmt.Errorf("migrate notification outbox: %w", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notification_outbox_due
		ON notification_outbox (state, next_attempt_at, created_at, id)`); err != nil {
		return fmt.Errorf("index notification outbox due rows: %w", err)
	}
	// RFC3339Nano omits trailing fractional zeroes. Removing the final Z
	// preserves chronological ordering for both whole and fractional seconds.
	if _, err := db.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_notification_outbox_terminal_time
		ON notification_outbox (state, rtrim(completed_at, 'Z')) WHERE completed_at <> '';
		DROP INDEX IF EXISTS idx_notification_outbox_terminal;`); err != nil {
		return fmt.Errorf("index notification outbox terminal rows: %w", err)
	}
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

func migrateOutboxColumns(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(notification_outbox)`)
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid int
		var name, columnType string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	migrations := []struct {
		column string
		query  string
	}{
		{"destination_fingerprint", `ALTER TABLE notification_outbox ADD COLUMN destination_fingerprint TEXT NOT NULL DEFAULT ''`},
		{"attempt_id", `ALTER TABLE notification_outbox ADD COLUMN attempt_id TEXT NOT NULL DEFAULT ''`},
		{"last_indeterminate_at", `ALTER TABLE notification_outbox ADD COLUMN last_indeterminate_at TEXT NOT NULL DEFAULT ''`},
	}
	for _, migration := range migrations {
		if !columns[migration.column] {
			if _, err := db.ExecContext(ctx, migration.query); err != nil {
				return fmt.Errorf("add %s: %w", migration.column, err)
			}
		}
	}
	return nil
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

// A zero claim TTL is used after startup or persistence handoff, when every
// claim belongs to a stopped worker even if the wall clock has moved backward.
func recoverExpiredOutboxClaims(ctx context.Context, db *sql.DB, now time.Time, claimTTL time.Duration, active outboxDeliveries) error {
	if db == nil {
		return nil
	}
	cutoff := now.Add(-claimTTL).UTC().Format(time.RFC3339Nano)
	nowText := now.UTC().Format(time.RFC3339Nano)
	retryAt := now.UTC().Add(indeterminateRetryDelay).Format(time.RFC3339Nano)
	_, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET state = CASE WHEN attempt_id <> '' AND completed_at = '' AND attempts >= ? THEN ? ELSE ? END,
		       next_attempt_at = CASE WHEN attempt_id <> '' AND completed_at = ''
		           THEN CASE WHEN attempts >= ? THEN '' ELSE ? END ELSE ? END,
		       last_indeterminate_at = CASE WHEN attempt_id <> '' AND completed_at = ''
		           THEN attempted_at ELSE last_indeterminate_at END,
		       status_code = CASE WHEN attempt_id <> '' AND completed_at = '' THEN 0 ELSE status_code END,
		       duration_ms = CASE WHEN attempt_id <> '' AND completed_at = '' THEN 0 ELSE duration_ms END,
		       error = CASE WHEN attempt_id <> '' AND completed_at = '' THEN ? ELSE error END,
		       completed_at = CASE WHEN attempt_id <> '' AND completed_at = '' THEN ? ELSE completed_at END,
		       claimed_at = '', updated_at = ?
		 WHERE state = ? AND (? OR rtrim(claimed_at, 'Z') <= rtrim(?, 'Z'))
		   AND destination IS NOT ? AND destination IS NOT ? AND destination IS NOT ?
	`, defaultAttempts, outboxStateFailed, outboxStateRetrying, defaultAttempts, retryAt, nowText,
		outboxIndeterminateReason, nowText, nowText, outboxStateClaimed, claimTTL == 0, cutoff,
		active[0], active[1], active[2])
	return err
}

// Eligibility and wake scheduling share the same destination binding. Legacy
// unbound rows use the current binding until an HTTP 429 binds their retry.
const outboxCurrentFingerprint = `json_extract(binding.configuration, '$.destination_fingerprints.' || o.destination)`

const outboxDestinationEnabled = `COALESCE(json_extract(binding.configuration, CASE o.destination
	WHEN 'webhook' THEN '$.enabled'
	WHEN 'discord' THEN '$.discord_enabled'
	WHEN 'telegram' THEN '$.telegram_enabled'
END), 0)`

const outboxSchedulingJoins = `
	LEFT JOIN (
		SELECT CASE WHEN json_valid(value) THEN value END AS configuration
		FROM settings WHERE key=?
	) binding ON TRUE
	LEFT JOIN notification_destination_health health
		ON health.destination=o.destination
		AND health.fingerprint=COALESCE(NULLIF(o.destination_fingerprint, ''), ` + outboxCurrentFingerprint + `)
`

// A disabled or rotated destination's cooldown must not delay revalidating its rows.
// Preserve independent delays, such as a new backoff after a revalidation error.
const outboxEffectiveDue = `CASE
	WHEN binding.configuration IS NOT NULL AND (NOT ` + outboxDestinationEnabled + `
		OR (json_type(binding.configuration, '$.destination_fingerprints')='object'
		AND o.destination_fingerprint<>'' AND o.destination_fingerprint IS NOT ` + outboxCurrentFingerprint + `))
	THEN CASE WHEN o.status_code=429 AND o.next_attempt_at=health.rate_limited_until THEN ''
		ELSE rtrim(o.next_attempt_at, 'Z') END
	ELSE max(rtrim(o.next_attempt_at, 'Z'), COALESCE(rtrim(health.rate_limited_until, 'Z'), ''))
END`

func claimNextOutboxRow(ctx context.Context, db *sql.DB, now time.Time, active outboxDeliveries) (*outboxRow, error) {
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
		SELECT o.id, o.destination, o.event_type, o.action, o.target_type, o.target_name, o.status, o.message,
		       o.event_created_at, o.actor, o.client_ip, o.meta_json, o.destination_fingerprint, o.state, o.attempts,
		       o.next_attempt_at, o.claimed_at, o.attempt_id
		  FROM notification_outbox o
		`+outboxSchedulingJoins+`
		 WHERE o.state IN (?, ?) AND `+outboxEffectiveDue+` <= rtrim(?, 'Z')
		   AND o.destination IS NOT ? AND o.destination IS NOT ? AND o.destination IS NOT ?
		 ORDER BY o.rowid
		 LIMIT 1
	`, SettingsKey, outboxStatePending, outboxStateRetrying, nowText, active[0], active[1], active[2]).Scan(
		&row.ID, &row.Destination, &row.EventType, &row.Intent.Action, &row.Intent.TargetType,
		&row.Intent.TargetName, &row.Intent.Status, &row.Intent.Message, &row.Intent.CreatedAt,
		&row.Intent.Actor, &row.Intent.ClientIP, &row.Intent.MetaJSON, &row.DestinationFingerprint,
		&row.State, &row.Attempts, &row.NextAttemptAt, &row.ClaimedAt, &row.AttemptID,
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

// admitOutboxAttempt consumes the attempt before an HTTP request can leave the
// worker. The token ties a later completion to this particular admission
// across claim expiry, settings changes and database replacement.
func admitOutboxAttempt(ctx context.Context, db *sql.DB, row *outboxRow, fingerprint string, now time.Time) error {
	token := rand.Text()
	attemptedAt := now.UTC().Format(time.RFC3339Nano)
	result, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET attempts = attempts + 1, attempt_id = ?, attempted_at = ?, completed_at = '',
		       status_code = 0, error = '', duration_ms = 0, destination_fingerprint = ?, updated_at = ?
		 WHERE id = ? AND state = ? AND claimed_at = ?
		   AND attempts = ? AND attempts < ? AND (attempt_id = '' OR completed_at <> '')
		   AND (destination_fingerprint = '' OR destination_fingerprint = ?)
	`, token, attemptedAt, fingerprint, attemptedAt, row.ID, outboxStateClaimed,
		row.ClaimedAt, row.Attempts, defaultAttempts, fingerprint)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errOutboxAttemptNotAdmitted
	}
	row.Attempts++
	row.AttemptID = token
	row.AttemptedAt = attemptedAt
	row.DestinationFingerprint = fingerprint
	return nil
}

func completeOutboxRow(ctx context.Context, db *sql.DB, id string, outcome outboxOutcome, now string, recoverReleased bool) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Recovery can finish an indeterminate attempt while its token still
	// matches. An intervening admission or completion replaces that token.
	var destination string
	err = tx.QueryRowContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, attempts = ?, next_attempt_at = ?, claimed_at = '', attempted_at = ?,
		       completed_at = ?, status_code = ?, error = ?, duration_ms = ?, updated_at = ?, attempt_id = '',
		       destination_fingerprint = CASE WHEN destination_fingerprint='' AND ?<>''
		           THEN ? ELSE destination_fingerprint END
		 WHERE id = ? AND (state = ? OR (? AND (state = ? OR (state = ? AND error = ?))))
		   AND attempt_id = ? AND attempts = ?
		 RETURNING destination
	`, outcome.State, outcome.Attempts, outcome.NextAttemptAt, outcome.AttemptedAt,
		outcome.CompletedAt, outcome.StatusCode, outcome.Error, outcome.DurationMS, now,
		outcome.RateLimitedUntil, outcome.DestinationFingerprint, id, outboxStateClaimed,
		recoverReleased, outboxStateRetrying, outboxStateFailed, outboxIndeterminateReason, outcome.AttemptID, outcome.Attempts).Scan(&destination)
	if errors.Is(err, sql.ErrNoRows) {
		return errOutboxCompletionLostClaim
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
			attemptAt: outcome.CompletedAt, state: outcome.State, rateLimitedUntil: outcome.RateLimitedUntil,
		}
		if success {
			event.successAt = outcome.CompletedAt
		}
		if err := persistDestinationHealth(ctx, tx, event, true); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func releaseOutboxClaim(ctx context.Context, db *sql.DB, id, nextAttemptAt, now string) error {
	_, err := db.ExecContext(ctx, `
		UPDATE notification_outbox
		   SET state = ?, claimed_at = '', next_attempt_at = ?, updated_at = ?
		 WHERE id = ? AND state = ? AND (attempt_id = '' OR completed_at <> '')
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

func nextOutboxWake(ctx context.Context, db *sql.DB, claimTTL time.Duration, active outboxDeliveries) (time.Time, bool, error) {
	if db == nil {
		return time.Time{}, false, nil
	}
	// Wake scheduling can run after each delivery. Use the state index so a
	// large retained delivery history does not slow down a draining lane.
	var nextAttempt, oldestClaim string
	err := db.QueryRowContext(ctx, `
		SELECT
			COALESCE(MIN(CASE WHEN o.state IN (?, ?) THEN NULLIF(`+outboxEffectiveDue+`, '') END) || 'Z', ''),
			COALESCE(MIN(CASE WHEN o.state = ? AND o.claimed_at != '' THEN rtrim(o.claimed_at, 'Z') END) || 'Z', '')
		  FROM notification_outbox o
		`+outboxSchedulingJoins+`
		 WHERE o.state IN (?, ?, ?)
		   AND o.destination IS NOT ? AND o.destination IS NOT ? AND o.destination IS NOT ?
	`, outboxStatePending, outboxStateRetrying, outboxStateClaimed, SettingsKey,
		outboxStatePending, outboxStateRetrying, outboxStateClaimed, active[0], active[1], active[2]).Scan(&nextAttempt, &oldestClaim)
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
		 WHERE state IN (?, ?, ?) AND completed_at <> '' AND rtrim(completed_at, 'Z') < rtrim(?, 'Z')
	`, outboxStateSucceeded, outboxStateFailed, outboxStateSkipped, cutoff)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
