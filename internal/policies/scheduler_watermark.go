package policies

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	SchedulerWatermarkSetting       = "update_policy_scheduler_watermark_utc"
	SchedulerStateFingerprintSetting = "update_policy_scheduler_state_fingerprint"
	schedulerRecoveryScopePrefix     = "update_policy_scheduler_recovery_scope:"
)

func (r *SQLiteRepository) LoadSchedulerWatermark() (time.Time, bool, error) {
	if r == nil || r.database() == nil {
		return time.Time{}, false, errors.New("policy repository database is unavailable")
	}
	raw, err := r.getSettingValue(SchedulerWatermarkSetting)
	if err != nil {
		return time.Time{}, false, err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("parse policy scheduler watermark %q: %w", raw, err)
	}
	return parsed.UTC().Truncate(time.Minute), true, nil
}

func (r *SQLiteRepository) SaveSchedulerWatermark(value time.Time) error {
	if r == nil || r.database() == nil {
		return errors.New("policy repository database is unavailable")
	}
	if value.IsZero() {
		return errors.New("policy scheduler watermark is required")
	}
	canonical := value.UTC().Truncate(time.Minute).Format(time.RFC3339Nano)
	return r.upsertSettingValue(SchedulerWatermarkSetting, canonical)
}

func (r *SQLiteRepository) LoadSchedulerStateFingerprint() (string, bool, error) {
	if r == nil || r.database() == nil {
		return "", false, errors.New("policy repository database is unavailable")
	}
	raw, err := r.getSettingValue(SchedulerStateFingerprintSetting)
	if err != nil {
		return "", false, err
	}
	raw = strings.TrimSpace(raw)
	return raw, raw != "", nil
}

func (r *SQLiteRepository) SaveSchedulerStateFingerprint(value string) error {
	if r == nil || r.database() == nil {
		return errors.New("policy repository database is unavailable")
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return errors.New("policy scheduler state fingerprint is required")
	}
	return r.upsertSettingValue(SchedulerStateFingerprintSetting, value)
}

func schedulerRecoveryScopeSettingKey(policyID int64, scheduledForUTC string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(scheduledForUTC)))
	return fmt.Sprintf("%s%d:%x", schedulerRecoveryScopePrefix, policyID, digest[:])
}

func (r *SQLiteRepository) HasSchedulerRecoveryScope(policyID int64, scheduledForUTC string) (bool, error) {
	if r == nil || r.database() == nil {
		return false, errors.New("policy repository database is unavailable")
	}
	if policyID <= 0 || strings.TrimSpace(scheduledForUTC) == "" {
		return false, errors.New("policy scheduler recovery scope is required")
	}
	raw, err := r.getSettingValue(schedulerRecoveryScopeSettingKey(policyID, scheduledForUTC))
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(raw) != "", nil
}

func (r *SQLiteRepository) MarkSchedulerRecoveryScope(policyID int64, scheduledForUTC string) error {
	if r == nil || r.database() == nil {
		return errors.New("policy repository database is unavailable")
	}
	if policyID <= 0 || strings.TrimSpace(scheduledForUTC) == "" {
		return errors.New("policy scheduler recovery scope is required")
	}
	return r.upsertSettingValue(schedulerRecoveryScopeSettingKey(policyID, scheduledForUTC), "1")
}
