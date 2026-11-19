package notifications

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// ConditionFact contains accepted domain facts only. Unknown observations must
// be omitted, never translated into Active=false.
type ConditionFact struct {
	ID             string
	EventType      string
	TargetType     string
	TargetName     string
	ServerIdentity string `json:",omitempty"`
	Generation     string
	Active         bool
	OneShot        bool
	Message        string
	MetaJSON       string
	CreatedAt      string
}

type Condition struct {
	Fact      ConditionFact
	Episode   int64
	UpdatedAt string
	// Keep the current delivery identity when an offline recovery combines
	// histories with different counters. Use the floor for the next episode.
	EpisodeFloor int64 `json:",omitempty"`
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
	Cursor                  string
	Conditions              map[string]Condition
	Admissions              map[string]bool
	Reminders               map[string]string
	ApprovalAdmissions      map[string][]approvalAdmission `json:",omitempty"`
	ApprovalHistoryMigrated bool                           `json:",omitempty"`
}

type approvalAdmission struct {
	ServerIdentity string `json:",omitempty"`
	Generation     string
	Fingerprint    string
	OutboxID       string `json:",omitempty"`
}

func (admission approvalAdmission) intentID(conditionID string) string {
	return "condition/approval-plan/" + url.PathEscape(conditionID) + "/" +
		url.PathEscape(admission.ServerIdentity) + "/" + url.PathEscape(admission.Generation) + "/" + admission.Fingerprint
}

func (fact ConditionFact) approvalAdmission(fingerprint string) approvalAdmission {
	return approvalAdmission{ServerIdentity: fact.ServerIdentity, Generation: fact.Generation, Fingerprint: fingerprint}
}

func (state *conditionState) findApprovalAdmission(conditionID string, admission approvalAdmission) (approvalAdmission, bool) {
	for _, previous := range state.ApprovalAdmissions[conditionID] {
		// Older jobs can establish the plan's job and generation without an
		// inventory identity. Preserve their admission when that data is absent.
		if previous.Generation == admission.Generation && previous.Fingerprint == admission.Fingerprint &&
			(previous.ServerIdentity == admission.ServerIdentity || previous.ServerIdentity == "" || admission.ServerIdentity == "") {
			return previous, true
		}
	}
	return approvalAdmission{}, false
}

func (state *conditionState) recordApprovalAdmission(conditionID string, admission approvalAdmission) {
	if state.ApprovalAdmissions == nil {
		state.ApprovalAdmissions = make(map[string][]approvalAdmission)
	}
	state.ApprovalAdmissions[conditionID] = append(state.ApprovalAdmissions[conditionID], admission)
}

func episodeAdmissionID(condition Condition, fingerprint string) string {
	return fmt.Sprintf("condition/%s/%d/%s", condition.Fact.ID, condition.Episode, fingerprint)
}

const aptConditionPrefix = "apt-reconciliation/"

func APTReconciliationConditionID(target, jobID string) string {
	digest := sha256.Sum256([]byte(target + "\x00" + jobID))
	return fmt.Sprintf("%s%x", aptConditionPrefix, digest[:])
}

func conditionFingerprints(settings Settings) map[string]string {
	fingerprints := map[string]string{}
	for _, destination := range []string{DestinationWebhook, DestinationDiscord, DestinationTelegram} {
		if fingerprint := destinationConfigFingerprint(settings, destination); fingerprint != "" {
			fingerprints[fingerprint] = destination
		}
	}
	return fingerprints
}

func migrateApprovalOutboxHistory(ctx context.Context, db *sql.DB, state *conditionState) error {
	if db == nil || state.ApprovalHistoryMigrated {
		return nil
	}
	// The old episode ledger discarded admissions after destination changes.
	// Retained outbox rows still identify those plans and their destinations.
	rows, err := db.QueryContext(ctx, `
		SELECT id, destination, destination_fingerprint, meta_json FROM notification_outbox
		WHERE event_type = ? AND destination_fingerprint <> ''
		ORDER BY CASE state WHEN 'succeeded' THEN 0 WHEN 'failed' THEN 1 WHEN 'skipped' THEN 3 ELSE 2 END, rowid
	`, EventApprovalPublished)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, destination, fingerprint, rawMeta string
		if err := rows.Scan(&id, &destination, &fingerprint, &rawMeta); err != nil {
			return err
		}
		var meta struct {
			ConditionID string `json:"condition_id"`
			Episode     int64  `json:"condition_episode"`
			Generation  uint64 `json:"approval_generation"`
		}
		if err := json.Unmarshal([]byte(rawMeta), &meta); err != nil || meta.ConditionID == "" || meta.Episode <= 0 || meta.Generation == 0 {
			continue
		}
		legacy := Condition{Fact: ConditionFact{ID: meta.ConditionID}, Episode: meta.Episode}
		if id != outboxDeliveryID(episodeAdmissionID(legacy, fingerprint), destination) {
			continue
		}
		admission := approvalAdmission{Generation: strconv.FormatUint(meta.Generation, 10), Fingerprint: fingerprint, OutboxID: id}
		if _, exists := state.findApprovalAdmission(meta.ConditionID, admission); !exists {
			state.recordApprovalAdmission(meta.ConditionID, admission)
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	state.ApprovalHistoryMigrated = true
	return nil
}

func migrateEpisodeApprovalAdmissions(state *conditionState, fingerprints map[string]string) {
	for _, condition := range state.Conditions {
		fact := condition.Fact
		if fact.EventType != EventApprovalPublished || fact.Generation == "" {
			continue
		}
		for fingerprint, destination := range fingerprints {
			legacyID := episodeAdmissionID(condition, fingerprint)
			if !state.Admissions[legacyID] {
				continue
			}
			admission := fact.approvalAdmission(fingerprint)
			if _, exists := state.findApprovalAdmission(fact.ID, admission); exists {
				continue
			}
			// Preserve an admission even if its outbox row has been pruned.
			admission.OutboxID = outboxDeliveryID(legacyID, destination)
			state.recordApprovalAdmission(fact.ID, admission)
		}
	}
}

func ensureConditionSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS notification_conditions (id INTEGER PRIMARY KEY CHECK(id=1), state_json TEXT NOT NULL);
		CREATE TABLE IF NOT EXISTS notification_server_identities (server_name TEXT PRIMARY KEY, identity TEXT NOT NULL UNIQUE);
		CREATE TABLE IF NOT EXISTS notification_apt_admissions (identity TEXT PRIMARY KEY NOT NULL) WITHOUT ROWID;
	`)
	return err
}

const conditionStateQuery = `SELECT state_json FROM notification_conditions WHERE id=1`

func loadConditionState(ctx context.Context, db *sql.DB) (conditionState, error) {
	if db != nil {
		return readConditionState(db.QueryRowContext(ctx, conditionStateQuery))
	}
	return readConditionState(nil)
}

func readConditionState(row *sql.Row) (conditionState, error) {
	state := conditionState{Conditions: map[string]Condition{}, Admissions: map[string]bool{}, Reminders: map[string]string{}}
	if row == nil {
		return state, nil
	}
	var raw string
	err := row.Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return state, err
	}
	if state.Conditions == nil {
		state.Conditions = map[string]Condition{}
	}
	if state.Admissions == nil {
		state.Admissions = map[string]bool{}
	}
	if state.Reminders == nil {
		state.Reminders = map[string]string{}
	}
	return state, nil
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
	db := s.database()
	state, err := loadAndMigrateConditionState(ctx, db)
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
	fingerprints := conditionFingerprints(settings)
	if err := migrateApprovalOutboxHistory(ctx, db, &state); err != nil {
		return err
	}
	migrateEpisodeApprovalAdmissions(&state, fingerprints)
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
		episodeFloor := previous.EpisodeFloor
		if fact.Active && (!exists || !previous.Fact.Active || previous.Fact.Generation != fact.Generation || previous.Fact.EventType != fact.EventType) {
			episode = max(episode, episodeFloor) + 1
			episodeFloor = 0
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
		state.Conditions[fact.ID] = Condition{Fact: fact, Episode: episode, UpdatedAt: updated, EpisodeFloor: episodeFloor}
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
			fingerprint := destinationConfigFingerprint(settings, destination)
			identity := episodeAdmissionID(condition, fingerprint)
			exactApproval := fact.EventType == EventApprovalPublished && fact.Generation != ""
			exactAPT := fact.EventType == EventAPTReconciliationRequired
			admission := fact.approvalAdmission(fingerprint)
			switch {
			case exactApproval:
				if _, exists := state.findApprovalAdmission(fact.ID, admission); exists {
					continue
				}
				identity = admission.intentID(fact.ID)
			case exactAPT:
				identity = aptAdmissionID(fact, fingerprint)
				exists, err := hasAPTAdmission(ctx, db, identity)
				if err != nil {
					return err
				}
				if exists {
					continue
				}
			default:
				if state.Admissions[identity] {
					continue
				}
			}
			intent := conditionIntent(condition)
			intent.ID = identity
			admitted, err := buildOutboxRows(intent, fact.EventType, []string{destination}, now)
			if err != nil {
				return err
			}
			admitted[0].DestinationFingerprint = fingerprint
			rows = append(rows, admitted...)
			if exactAPT {
				continue
			}
			if exactApproval {
				admission.OutboxID = admitted[0].ID
				state.recordApprovalAdmission(fact.ID, admission)
			} else {
				state.Admissions[identity] = true
			}
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
	pruneConditionState(&state, fingerprints, now)
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if string(original) == string(raw) && len(rows) == 0 {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	admittedRows := rows[:0]
	for _, row := range rows {
		if row.EventType != EventAPTReconciliationRequired {
			admittedRows = append(admittedRows, row)
			continue
		}
		result, err := tx.ExecContext(ctx, insertAPTAdmissionSQL, row.Intent.ID)
		if err != nil {
			return err
		}
		n, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if n == 1 {
			admittedRows = append(admittedRows, row)
		}
	}
	if err := insertOutboxRowsTx(ctx, tx, admittedRows, now.Format(time.RFC3339Nano)); err != nil {
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
		if !fact.Active || fact.OneShot || fact.EventType == EventAPTReconciliationRequired || !eventEnabled(settings.EventTypes, fact.EventType) {
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: %s", fact.TargetName, fact.Message))
	}
	if len(lines) == 0 {
		return DeliveryIntent{}, false
	}
	const limit = 3500
	const overflow = "\nMore conditions are available in Status or Administration."
	var message strings.Builder
	fmt.Fprintf(&message, "%d unresolved conditions:", len(lines))
	remaining := limit - message.Len() - len(overflow)
	for _, line := range lines {
		size := utf8.RuneCountInString(line) + 1
		if size > remaining {
			message.WriteString(overflow)
			break
		}
		message.WriteByte('\n')
		message.WriteString(line)
		remaining -= size
	}
	return DeliveryIntent{Action: EventReminder, TargetType: "fleet", TargetName: "SimpleLinuxUpdater", Status: "attention", Message: message.String(), CreatedAt: now.Format(time.RFC3339Nano)}, true
}

type conditionDeliveryEligibility uint8

const (
	conditionIneligible conditionDeliveryEligibility = iota
	conditionEligible
	reminderSuperseded
)

func (s *Service) revalidateOutboxCondition(ctx context.Context, row *outboxRow, settings Settings) (conditionDeliveryEligibility, error) {
	if row.EventType != EventReminder && !strings.HasPrefix(row.Intent.ID, "condition/") {
		var meta struct {
			ID string `json:"condition_id"`
		}
		_ = json.Unmarshal([]byte(row.Intent.MetaJSON), &meta)
		if meta.ID == "" {
			return conditionEligible, nil
		}
	}
	if s.deps.ReadConditions != nil {
		if !s.conditionMonitoringStarted() {
			return conditionIneligible, errors.New("notification condition monitoring has not started")
		}
		if err := s.reconcileConditions(ctx); err != nil {
			return conditionIneligible, err
		}
	}
	state, err := loadAndMigrateConditionState(ctx, s.database())
	if err != nil {
		return conditionIneligible, err
	}
	if row.EventType == EventReminder {
		if !settings.RemindersEnabled {
			return conditionIneligible, nil
		}
		intent, ok := reminderIntent(state, settings, s.deps.Now())
		if !ok {
			return conditionIneligible, nil
		}
		// Changing a destination removes its old fingerprint from the reminder
		// ledger even when the underlying conditions still need a reminder.
		if state.Reminders[row.DestinationFingerprint] != row.Intent.CreatedAt {
			return reminderSuperseded, nil
		}
		row.Intent = intent
		return conditionEligible, nil
	}
	var meta struct {
		ID      string `json:"condition_id"`
		Episode int64  `json:"condition_episode"`
	}
	if err := json.Unmarshal([]byte(row.Intent.MetaJSON), &meta); err != nil {
		return conditionIneligible, err
	}
	condition, ok := state.Conditions[meta.ID]
	if !ok || !condition.Fact.Active || condition.Fact.EventType != row.EventType || !eventEnabled(settings.EventTypes, row.EventType) {
		return conditionIneligible, nil
	}
	fact := condition.Fact
	if row.EventType == EventApprovalPublished && fact.Generation != "" {
		admission, exists := state.findApprovalAdmission(fact.ID, fact.approvalAdmission(row.DestinationFingerprint))
		if !exists || row.ID != admission.OutboxID {
			return conditionIneligible, nil
		}
	} else if row.EventType == EventAPTReconciliationRequired {
		identity := aptAdmissionID(fact, row.DestinationFingerprint)
		exists, err := hasAPTAdmission(ctx, s.database(), identity)
		if err != nil {
			return conditionIneligible, err
		}
		if !exists || row.ID != outboxDeliveryID(identity, row.Destination) {
			return conditionIneligible, nil
		}
	} else if condition.Episode != meta.Episode {
		return conditionIneligible, nil
	}
	row.Intent = conditionIntent(condition)
	return conditionEligible, nil
}

func (s *Service) restoreConditions(ctx context.Context, db *sql.DB) error {
	s.persistenceMu.Lock()
	state := s.replacementConditions
	s.persistenceMu.Unlock()
	if state == nil {
		return nil
	}
	restored := *state
	// Do not replay historical audit events from the restored database. Current
	// domain state is revalidated by the source before any pending delivery.
	restored.Cursor = "restore"
	return saveConditionState(ctx, db, &restored)
}

func saveConditionState(ctx context.Context, store interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, state *conditionState) error {
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	_, err = store.ExecContext(ctx, `INSERT INTO notification_conditions(id,state_json) VALUES(1,?) ON CONFLICT(id) DO UPDATE SET state_json=excluded.state_json`, string(raw))
	return err
}

func isConditionEvent(event string) bool {
	switch event {
	case EventApprovalPublished, EventApprovalExpired, EventRebootRequired, EventAPTReconciliationRequired, EventBackupExportFailed, EventBackupVerifyFailed, EventBackupCoverageMissing, EventBackupCoverageStale:
		return true
	}
	return false
}

// Keep episode counters and exact approval admissions while their job facts
// are retained. Other event types only keep configured destination fingerprints.
func pruneConditionState(state *conditionState, fingerprints map[string]string, now time.Time) {
	for id, condition := range state.Conditions {
		updated, _ := time.Parse(time.RFC3339Nano, condition.UpdatedAt)
		exactFact := condition.Fact.EventType == EventApprovalPublished || condition.Fact.EventType == EventAPTReconciliationRequired
		if (condition.Fact.OneShot || (!condition.Fact.Active && exactFact)) && !updated.IsZero() && now.Sub(updated) > defaultOutboxRetention {
			delete(state.Conditions, id)
		}
	}
	keep := map[string]bool{}
	for _, condition := range state.Conditions {
		if (condition.Fact.EventType == EventApprovalPublished && condition.Fact.Generation != "") || condition.Fact.EventType == EventAPTReconciliationRequired {
			continue
		}
		for fingerprint := range fingerprints {
			key := episodeAdmissionID(condition, fingerprint)
			if state.Admissions[key] {
				keep[key] = true
			}
		}
	}
	state.Admissions = keep
	for id := range state.ApprovalAdmissions {
		if _, exists := state.Conditions[id]; !exists {
			delete(state.ApprovalAdmissions, id)
		}
	}
	for fingerprint := range state.Reminders {
		if fingerprints[fingerprint] == "" {
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
