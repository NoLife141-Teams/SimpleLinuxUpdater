package backup

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	DefaultRecoveryStaleAfter            = 7 * 24 * time.Hour
	DefaultRecoveryEvidenceRetentionDays = 90

	RecoveryStateHealthy     = "healthy"
	RecoveryStateStale       = "stale"
	RecoveryStateNever       = "never"
	RecoveryStateFailed      = "failed"
	RecoveryStateUnavailable = "unavailable"
)

type RecoveryHealth struct {
	State           string                  `json:"state"`
	Message         string                  `json:"message"`
	CheckedAt       string                  `json:"checked_at"`
	StaleAfterHours int                     `json:"stale_after_hours"`
	Export          RecoveryOperationHealth `json:"export"`
	Verification    RecoveryOperationHealth `json:"verification"`
	Schedule        RecoverySchedule        `json:"schedule"`
	Retention       RecoveryRetention       `json:"retention"`
}

type RecoveryOperationHealth struct {
	State         string `json:"state"`
	LastAttemptAt string `json:"last_attempt_at,omitempty"`
	LastSuccessAt string `json:"last_success_at,omitempty"`
	SizeBytes     *int64 `json:"size_bytes,omitempty"`
	Message       string `json:"message"`
}

type RecoverySchedule struct {
	Scheduled    bool   `json:"scheduled"`
	NextBackupAt string `json:"next_backup_at,omitempty"`
	Message      string `json:"message"`
}

type RecoveryRetention struct {
	EvidenceDays        int    `json:"evidence_days"`
	ArchiveRetained     bool   `json:"archive_retained_by_app"`
	AutomaticDeletion   bool   `json:"automatic_deletion"`
	EvidenceDescription string `json:"evidence_description"`
	ArchiveDescription  string `json:"archive_description"`
}

type recoveryAuditFact struct {
	CreatedAt string
	Status    string
	Message   string
	MetaJSON  string
}

func (s *Service) RecoveryHealth(ctx context.Context) (RecoveryHealth, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if s == nil {
		now := time.Now().UTC()
		health := newRecoveryHealth(now, DefaultRecoveryStaleAfter, DefaultRecoveryEvidenceRetentionDays)
		return unavailableRecoveryHealth(health), errors.New("backup recovery evidence service is unavailable")
	}
	now := s.deps.Now().UTC()
	staleAfter := s.deps.RecoveryStaleAfter
	retentionDays := s.deps.RecoveryEvidenceRetentionDays
	health := newRecoveryHealth(now, staleAfter, retentionDays)
	if s.deps.DB == nil {
		return unavailableRecoveryHealth(health), errors.New("backup recovery evidence database is unavailable")
	}
	db := s.deps.DB()
	if db == nil {
		return unavailableRecoveryHealth(health), errors.New("backup recovery evidence database is unavailable")
	}

	exportFacts, err := loadRecoveryAuditFacts(ctx, db, "backup.export")
	if err != nil {
		return unavailableRecoveryHealth(health), fmt.Errorf("load backup export evidence: %w", err)
	}
	verificationFacts, err := loadRecoveryAuditFacts(ctx, db, "backup.verify")
	if err != nil {
		return unavailableRecoveryHealth(health), fmt.Errorf("load backup verification evidence: %w", err)
	}

	health.Export, err = projectRecoveryOperation(exportFacts, "backup.export", now, staleAfter)
	if err != nil {
		return unavailableRecoveryHealth(health), fmt.Errorf("project backup export evidence: %w", err)
	}
	health.Verification, err = projectRecoveryOperation(verificationFacts, "backup.verify", now, staleAfter)
	if err != nil {
		return unavailableRecoveryHealth(health), fmt.Errorf("project backup verification evidence: %w", err)
	}
	health.State, health.Message = aggregateRecoveryHealth(health.Export, health.Verification, staleAfter)
	return health, nil
}

func newRecoveryHealth(now time.Time, staleAfter time.Duration, retentionDays int) RecoveryHealth {
	return RecoveryHealth{
		State:           RecoveryStateNever,
		Message:         "No successful backup export and verification have both been recorded.",
		CheckedAt:       now.Format(time.RFC3339),
		StaleAfterHours: int(staleAfter / time.Hour),
		Export:          neverRecoveryOperation("No backup export has been recorded."),
		Verification:    neverRecoveryOperation("No backup verification has been recorded."),
		Schedule: RecoverySchedule{
			Scheduled: false,
			Message:   "No backup is scheduled.",
		},
		Retention: RecoveryRetention{
			EvidenceDays:        retentionDays,
			ArchiveRetained:     false,
			AutomaticDeletion:   false,
			EvidenceDescription: fmt.Sprintf("Backup recovery evidence is retained in audit history for up to %d days.", retentionDays),
			ArchiveDescription:  "Exported archives are downloaded to operator-managed storage and are not retained or deleted by the app.",
		},
	}
}

func unavailableRecoveryHealth(health RecoveryHealth) RecoveryHealth {
	health.State = RecoveryStateUnavailable
	health.Message = "Backup recovery evidence is unavailable."
	health.Export = RecoveryOperationHealth{State: RecoveryStateUnavailable, Message: "Backup export evidence is unavailable."}
	health.Verification = RecoveryOperationHealth{State: RecoveryStateUnavailable, Message: "Backup verification evidence is unavailable."}
	return health
}

func neverRecoveryOperation(message string) RecoveryOperationHealth {
	return RecoveryOperationHealth{State: RecoveryStateNever, Message: message}
}

func loadRecoveryAuditFacts(ctx context.Context, db *sql.DB, action string) ([]recoveryAuditFact, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT created_at, status, message, meta_json
		  FROM audit_events
		 WHERE action = ?
		   AND target_type = 'backup'
		 ORDER BY id DESC
	`, action)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	facts := make([]recoveryAuditFact, 0, 8)
	for rows.Next() {
		var fact recoveryAuditFact
		if err := rows.Scan(&fact.CreatedAt, &fact.Status, &fact.Message, &fact.MetaJSON); err != nil {
			return nil, err
		}
		facts = append(facts, fact)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return facts, nil
}

func projectRecoveryOperation(facts []recoveryAuditFact, action string, now time.Time, staleAfter time.Duration) (RecoveryOperationHealth, error) {
	if len(facts) == 0 {
		if action == "backup.verify" {
			return neverRecoveryOperation("No backup verification has been recorded."), nil
		}
		return neverRecoveryOperation("No backup export has been recorded."), nil
	}

	latest := facts[0]
	if _, err := time.Parse(time.RFC3339, latest.CreatedAt); err != nil {
		return RecoveryOperationHealth{}, fmt.Errorf("invalid latest timestamp %q: %w", latest.CreatedAt, err)
	}
	var lastSuccess *recoveryAuditFact
	for i := range facts {
		if recoveryFactSucceeded(action, facts[i]) {
			lastSuccess = &facts[i]
			break
		}
	}

	result := RecoveryOperationHealth{
		State:         RecoveryStateFailed,
		LastAttemptAt: latest.CreatedAt,
		Message:       strings.TrimSpace(latest.Message),
	}
	if result.Message == "" {
		result.Message = "The latest backup operation failed."
	}
	if lastSuccess != nil {
		successAt, err := time.Parse(time.RFC3339, lastSuccess.CreatedAt)
		if err != nil {
			return RecoveryOperationHealth{}, fmt.Errorf("invalid successful timestamp %q: %w", lastSuccess.CreatedAt, err)
		}
		result.LastSuccessAt = lastSuccess.CreatedAt
		result.SizeBytes = recoveryFactSize(action, *lastSuccess)
		if recoveryFactSucceeded(action, latest) {
			if now.Sub(successAt) > staleAfter {
				result.State = RecoveryStateStale
				result.Message = fmt.Sprintf("The last successful operation is older than the %d-hour recovery threshold.", int(staleAfter/time.Hour))
			} else {
				result.State = RecoveryStateHealthy
				result.Message = strings.TrimSpace(lastSuccess.Message)
				if result.Message == "" {
					result.Message = "The latest backup operation succeeded."
				}
			}
		}
	}
	return result, nil
}

func recoveryFactSucceeded(action string, fact recoveryAuditFact) bool {
	if strings.TrimSpace(fact.Status) != "success" {
		return false
	}
	if action != "backup.verify" {
		return true
	}
	var metadata struct {
		RestoreReady *bool `json:"restore_ready"`
	}
	if err := json.Unmarshal([]byte(fact.MetaJSON), &metadata); err != nil || metadata.RestoreReady == nil {
		// Verification audits created before restore-readiness reviews represented only valid archives.
		return true
	}
	return *metadata.RestoreReady
}

func recoveryFactSize(action string, fact recoveryAuditFact) *int64 {
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal([]byte(fact.MetaJSON), &metadata); err != nil {
		return nil
	}
	keys := []string{"bytes"}
	if action == "backup.verify" {
		keys = []string{"archive_size_bytes", "total_bytes"}
	}
	for _, key := range keys {
		raw, ok := metadata[key]
		if !ok {
			continue
		}
		var size int64
		if err := json.Unmarshal(raw, &size); err == nil && size >= 0 {
			return &size
		}
	}
	return nil
}

func aggregateRecoveryHealth(export, verification RecoveryOperationHealth, staleAfter time.Duration) (string, string) {
	states := []string{export.State, verification.State}
	for _, state := range states {
		if state == RecoveryStateFailed {
			return RecoveryStateFailed, "The latest backup export or verification did not produce accepted recovery evidence."
		}
	}
	for _, state := range states {
		if state == RecoveryStateNever {
			return RecoveryStateNever, "No successful backup export and verification have both been recorded."
		}
	}
	for _, state := range states {
		if state == RecoveryStateStale {
			return RecoveryStateStale, fmt.Sprintf("Backup recovery evidence is older than the %d-hour threshold.", int(staleAfter/time.Hour))
		}
	}
	return RecoveryStateHealthy, "Recent successful backup export and verification evidence is available."
}
