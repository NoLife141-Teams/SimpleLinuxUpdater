package policies

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

const SchedulerWatermarkSetting = "update_policy_scheduler_watermark_utc"

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
