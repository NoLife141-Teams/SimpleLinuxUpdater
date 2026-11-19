package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"debian-updater/internal/installationhealth"
)

const (
	ConditionEvaluationHealthy  = "healthy"
	ConditionEvaluationDegraded = "degraded"
)

type InstallationHealthFacts struct {
	Enabled                   bool       `json:"enabled"`
	Pending                   int        `json:"pending"`
	Retrying                  int        `json:"retrying"`
	Failed                    int        `json:"failed"`
	ConditionEvaluationState  string     `json:"condition_evaluation_state,omitempty"`
	LastConditionEvaluationAt *time.Time `json:"last_condition_evaluation_at,omitempty"`
	LastSuccessfulDeliveryAt  *time.Time `json:"last_successful_delivery_at,omitempty"`
	OldestPendingAgeSeconds   *float64   `json:"oldest_pending_age_seconds,omitempty"`
}

type conditionEvaluationHealth struct {
	mu          sync.RWMutex
	state       string
	lastSuccess time.Time
}

func (h *conditionEvaluationHealth) observe(at time.Time, degraded bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = ConditionEvaluationHealthy
	if degraded {
		h.state = ConditionEvaluationDegraded
		return
	}
	h.lastSuccess = at.UTC()
}

func (h *conditionEvaluationHealth) degraded() bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.state == ConditionEvaluationDegraded
}

func (h *conditionEvaluationHealth) reset() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.state = ""
	h.lastSuccess = time.Time{}
}

func (h *conditionEvaluationHealth) snapshot() InstallationHealthFacts {
	h.mu.RLock()
	defer h.mu.RUnlock()
	facts := InstallationHealthFacts{ConditionEvaluationState: h.state}
	if facts.ConditionEvaluationState == "" {
		facts.ConditionEvaluationState = "not_observed"
	}
	if !h.lastSuccess.IsZero() {
		last := h.lastSuccess
		facts.LastConditionEvaluationAt = &last
	}
	return facts
}

// InstallationHealth reads aggregate observations without decrypting delivery
// settings, acquiring work, or returning destination or event identities.
func (s *Service) InstallationHealth(ctx context.Context) (installationhealth.WorkerSnapshot, InstallationHealthFacts, error) {
	worker := s.health.Snapshot(false)
	facts := s.conditionHealth.snapshot()
	db := s.database()
	if db == nil {
		return worker, facts, errors.New("notification persistence unavailable")
	}
	var raw string
	err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", SettingsKey).Scan(&raw)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return worker, facts, err
	}
	var settings persistedSettings
	if raw != "" {
		if err = json.Unmarshal([]byte(raw), &settings); err != nil {
			return worker, facts, err
		}
	}
	facts.Enabled = settings.Enabled || settings.DiscordEnabled || settings.TelegramEnabled
	worker.Enabled = facts.Enabled
	var oldest sql.NullString
	var success string
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(state IN ('pending', 'claimed')), 0),
		       COALESCE(SUM(state='retrying'), 0),
		       COALESCE(SUM(state='failed'), 0),
		       MIN(CASE WHEN state IN ('pending', 'claimed', 'retrying') AND created_at<>'' THEN rtrim(created_at, 'Z') END),
		       (SELECT last_success_at FROM notification_delivery_health WHERE id=1)
		  FROM notification_outbox
	`).Scan(&facts.Pending, &facts.Retrying, &facts.Failed, &oldest, &success)
	if err != nil {
		return worker, facts, err
	}
	if at, err := time.Parse(time.RFC3339Nano, success); err == nil {
		facts.LastSuccessfulDeliveryAt = &at
	}
	if oldest.Valid {
		if at, err := time.Parse(time.RFC3339Nano, oldest.String+"Z"); err == nil {
			age := max(0, s.deps.Now().Sub(at).Seconds())
			facts.OldestPendingAgeSeconds = &age
		}
	}
	return worker, facts, nil
}

func persistSuccessfulDelivery(ctx context.Context, tx *sql.Tx, at string) error {
	if at == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `
		UPDATE notification_delivery_health SET last_success_at=?
		WHERE id=1 AND rtrim(last_success_at, 'Z') < rtrim(?, 'Z')
	`, at, at)
	return err
}
