package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ConditionFact contains accepted domain facts only. Unknown observations must
// be omitted, never translated into Active=false.
type ConditionFact struct {
	ID         string
	EventType  string
	TargetType string
	TargetName string
	Generation string
	Active     bool
	OneShot    bool
	Message    string
	MetaJSON   string
	CreatedAt  string
}

type Condition struct {
	Fact      ConditionFact
	Episode   int64
	UpdatedAt string
}

type ConditionBatch struct {
	Rebased bool
	Facts   []ConditionFact
	Cursor  string
}

// ConditionSource is supplied by runtime composition. Cursor and facts are
// committed atomically with notification admissions; a failed read advances neither.
type ConditionSource func(context.Context, string, []Condition) (ConditionBatch, error)

type conditionState struct {
	Cursor     string
	Conditions map[string]Condition
	Admissions map[string]bool
	Reminders  map[string]string
}

func ensureConditionSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS notification_conditions (id INTEGER PRIMARY KEY CHECK(id=1), state_json TEXT NOT NULL); CREATE TABLE IF NOT EXISTS notification_server_identities(server_name TEXT PRIMARY KEY, identity TEXT NOT NULL UNIQUE)`)
	return err
}

func loadConditionState(ctx context.Context, db *sql.DB) (conditionState, error) {
	state := conditionState{Conditions: map[string]Condition{}, Admissions: map[string]bool{}, Reminders: map[string]string{}}
	if db == nil {
		return state, nil
	}
	var raw string
	err := db.QueryRowContext(ctx, `SELECT state_json FROM notification_conditions WHERE id=1`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}

func conditionList(state conditionState) []Condition {
	list := make([]Condition, 0, len(state.Conditions))
	for _, condition := range state.Conditions {
		list = append(list, condition)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Fact.ID < list[j].Fact.ID })
	return list
}

func (s *Service) reconcileConditions(ctx context.Context) error {
	s.conditionMu.Lock()
	defer s.conditionMu.Unlock()
	state, err := loadConditionState(ctx, s.database())
	if err != nil {
		return err
	}
	original, err := json.Marshal(state)
	if err != nil {
		return err
	}
	batch, err := s.deps.ReadConditions(ctx, state.Cursor, conditionList(state))
	if err != nil {
		return err
	}
	settings, err := s.currentSettings()
	if err != nil {
		return err
	}
	now := s.deps.Now().UTC()
	rows := []outboxRow{}
	freshOneShots := map[string]bool{}
	for _, fact := range batch.Facts {
		if fact.ID == "" {
			continue
		}
		previous, exists := state.Conditions[fact.ID]
		if fact.OneShot && !exists {
			freshOneShots[fact.ID] = true
		}
		if fact.EventType == EventRebootRequired && exists && !batch.Rebased && fact.CreatedAt != "" {
			observed, _ := time.Parse(time.RFC3339Nano, fact.CreatedAt)
			accepted, _ := time.Parse(time.RFC3339Nano, previous.Fact.CreatedAt)
			if observed.Before(accepted) && fact.TargetName == previous.Fact.TargetName {
				continue
			}
		}
		episode := previous.Episode
		if fact.Active && (!exists || !previous.Fact.Active || previous.Fact.Generation != fact.Generation || previous.Fact.EventType != fact.EventType) {
			episode++
		}
		if fact.CreatedAt == "" {
			if exists && previous.Fact.Active == fact.Active {
				fact.CreatedAt = previous.Fact.CreatedAt
			} else {
				fact.CreatedAt = now.Format(time.RFC3339Nano)
			}
		}
		if fact.OneShot && !exists && (!eventEnabled(settings.EventTypes, fact.EventType) || len(enabledDestinations(settings)) == 0) {
			fact.Active = false
		}
		if fact.OneShot && exists {
			fact.Active = previous.Fact.Active
		}
		updated := previous.UpdatedAt
		if !exists || previous.Fact.Active != fact.Active || previous.Episode != episode {
			updated = now.Format(time.RFC3339Nano)
		}
		state.Conditions[fact.ID] = Condition{Fact: fact, Episode: episode, UpdatedAt: updated}
	}
	state.Cursor = batch.Cursor
	for _, condition := range conditionList(state) {
		fact := condition.Fact
		if !fact.Active || (fact.OneShot && !freshOneShots[fact.ID]) || !eventEnabled(settings.EventTypes, fact.EventType) {
			continue
		}
		for _, destination := range enabledDestinations(settings) {
			if !destinationConfigured(settings, destination) {
				continue
			}
			identity := fmt.Sprintf("condition/%s/%d/%s", fact.ID, condition.Episode, destinationConfigFingerprint(settings, destination))
			if state.Admissions[identity] {
				continue
			}
			intent := conditionIntent(condition)
			intent.ID = identity
			admitted, err := buildOutboxRows(intent, fact.EventType, []string{destination}, now)
			if err != nil {
				return err
			}
			admitted[0].DestinationFingerprint = destinationConfigFingerprint(settings, destination)
			rows = append(rows, admitted...)
			state.Admissions[identity] = true
		}
	}
	for _, destination := range enabledDestinations(settings) {
		fingerprint := destinationConfigFingerprint(settings, destination)
		if fingerprint == "" {
			continue
		}
		last, _ := time.Parse(time.RFC3339Nano, state.Reminders[fingerprint])
		if !settings.RemindersEnabled {
			delete(state.Reminders, fingerprint)
			continue
		}
		if last.IsZero() {
			state.Reminders[fingerprint] = now.Format(time.RFC3339Nano)
			continue
		}
		interval := time.Duration(settings.ReminderIntervalMinutes) * time.Minute
		if now.Sub(last) < interval {
			continue
		}
		intent, ok := reminderIntent(state, settings, now)
		if !ok {
			continue
		}
		intent.ID = "reminder/" + fingerprint + "/" + now.Format(time.RFC3339Nano)
		admitted, err := buildOutboxRows(intent, EventReminder, []string{destination}, now)
		if err != nil {
			return err
		}
		admitted[0].DestinationFingerprint = fingerprint
		rows = append(rows, admitted...)
		state.Reminders[fingerprint] = now.Format(time.RFC3339Nano)
	}
	pruneConditionState(&state, settings, now)
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(original) == string(raw) && len(rows) == 0 {
		return nil
	}
	tx, err := s.database().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertOutboxRowsTx(ctx, tx, rows, now.Format(time.RFC3339Nano)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notification_conditions(id,state_json) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json`, string(raw)); err != nil {
		return err
	}
	return tx.Commit()
}

func conditionIntent(condition Condition) DeliveryIntent {
	fact := condition.Fact
	meta := map[string]any{}
	_ = json.Unmarshal([]byte(fact.MetaJSON), &meta)
	if meta == nil {
		meta = map[string]any{}
	}
	meta["condition_id"], meta["condition_episode"] = fact.ID, condition.Episode
	raw, _ := json.Marshal(meta)
	return DeliveryIntent{Action: fact.EventType, TargetType: fact.TargetType, TargetName: fact.TargetName, Status: "attention", Message: fact.Message, CreatedAt: fact.CreatedAt, MetaJSON: string(raw)}
}

func reminderIntent(state conditionState, settings Settings, now time.Time) (DeliveryIntent, bool) {
	lines := []string{}
	for _, condition := range conditionList(state) {
		fact := condition.Fact
		if !fact.Active || fact.OneShot || !eventEnabled(settings.EventTypes, fact.EventType) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", fact.TargetName, fact.Message))
	}
	if len(lines) == 0 {
		return DeliveryIntent{}, false
	}
	message := fmt.Sprintf("%d unresolved conditions:\n%s", len(lines), strings.Join(lines, "\n"))
	return DeliveryIntent{Action: EventReminder, TargetType: "fleet", TargetName: "SimpleLinuxUpdater", Status: "attention", Message: truncate(message, 3500), CreatedAt: now.Format(time.RFC3339Nano)}, true
}

func (s *Service) conditionDeliveryEligible(ctx context.Context, row *outboxRow, settings Settings) (bool, error) {
	if row.EventType != EventReminder && !strings.HasPrefix(row.Intent.ID, "condition/") {
		var meta struct {
			ID string `json:"condition_id"`
		}
		_ = json.Unmarshal([]byte(row.Intent.MetaJSON), &meta)
		if meta.ID == "" {
			return true, nil
		}
	}
	if s.deps.ReadConditions != nil {
		if !s.conditionMonitoringStarted() {
			return false, errors.New("notification condition monitoring has not started")
		}
		if err := s.reconcileConditions(ctx); err != nil {
			return false, err
		}
	}
	state, err := loadConditionState(ctx, s.database())
	if err != nil {
		return false, err
	}
	if row.EventType == EventReminder {
		if !settings.RemindersEnabled || state.Reminders[row.DestinationFingerprint] != row.Intent.CreatedAt {
			return false, nil
		}
		intent, ok := reminderIntent(state, settings, s.deps.Now())
		if ok {
			row.Intent = intent
		}
		return ok, nil
	}
	var meta struct {
		ID      string `json:"condition_id"`
		Episode int64  `json:"condition_episode"`
	}
	if err := json.Unmarshal([]byte(row.Intent.MetaJSON), &meta); err != nil {
		return false, err
	}
	condition, ok := state.Conditions[meta.ID]
	if !ok || !condition.Fact.Active || condition.Episode != meta.Episode || !eventEnabled(settings.EventTypes, row.EventType) {
		return false, nil
	}
	row.Intent = conditionIntent(condition)
	return true, nil
}

func (s *Service) restoreConditions(ctx context.Context, db *sql.DB) error {
	s.persistenceMu.Lock()
	state := s.replacementConditions
	s.persistenceMu.Unlock()
	if state == nil {
		return nil
	}
	// Do not replay historical audit events from the restored database. Current
	// domain state is revalidated by the source before any pending delivery.
	state.Cursor = "restore"
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `INSERT INTO notification_conditions(id,state_json) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json`, string(raw))
	return err
}

func isConditionEvent(event string) bool {
	switch event {
	case EventApprovalPublished, EventApprovalExpired, EventRebootRequired, EventBackupExportFailed, EventBackupVerifyFailed, EventBackupCoverageMissing, EventBackupCoverageStale:
		return true
	}
	return false
}

// Keep per-target episode counters, but discard consumed terminal job facts and
// obsolete admission fingerprints. Audit cursors prevent historical replay.
func pruneConditionState(state *conditionState, settings Settings, now time.Time) {
	for id, condition := range state.Conditions {
		updated, _ := time.Parse(time.RFC3339Nano, condition.UpdatedAt)
		if (condition.Fact.OneShot || (!condition.Fact.Active && condition.Fact.EventType == EventApprovalPublished)) && !updated.IsZero() && now.Sub(updated) > defaultOutboxRetention {
			delete(state.Conditions, id)
		}
	}
	keep := map[string]bool{}
	fingerprints := map[string]bool{}
	for _, destination := range []string{DestinationWebhook, DestinationDiscord, DestinationTelegram} {
		if fingerprint := destinationConfigFingerprint(settings, destination); fingerprint != "" {
			fingerprints[fingerprint] = true
		}
	}
	for _, condition := range state.Conditions {
		for fingerprint := range fingerprints {
			key := fmt.Sprintf("condition/%s/%d/%s", condition.Fact.ID, condition.Episode, fingerprint)
			if state.Admissions[key] {
				keep[key] = true
			}
		}
	}
	state.Admissions = keep
	for fingerprint := range state.Reminders {
		if !fingerprints[fingerprint] {
			delete(state.Reminders, fingerprint)
		}
	}
}

// Inventory calls these hooks in the same transaction as rename/delete. A Server
// identity is independent of endpoint details and health snapshot retention.
func RenameServerIdentityTx(tx *sql.Tx, oldName, newName string) error {
	_, err := tx.Exec(`UPDATE notification_server_identities SET server_name=? WHERE server_name=?`, newName, oldName)
	return err
}
func DeleteServerIdentityTx(tx *sql.Tx, name string) error {
	_, err := tx.Exec(`DELETE FROM notification_server_identities WHERE server_name=?`, name)
	return err
}
