package notifications

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	minRetryAfter = time.Second
	maxRetryAfter = time.Hour
)

// Interpret only one Retry-After value. No untrusted header text is retained.
func retryAfterDeadline(values []string, now time.Time) time.Time {
	if len(values) != 1 {
		return time.Time{}
	}
	value := strings.TrimSpace(values[0])
	if value == "" {
		return time.Time{}
	}
	if strings.Trim(value, "0123456789") == "" {
		seconds, err := strconv.ParseUint(value, 10, 64)
		if err != nil || seconds > uint64(maxRetryAfter/time.Second) {
			return now.Add(maxRetryAfter)
		}
		return now.Add(max(minRetryAfter, time.Duration(seconds)*time.Second))
	}
	at, err := http.ParseTime(value)
	if err != nil || !at.After(now) {
		return time.Time{}
	}
	return now.Add(min(maxRetryAfter, max(minRetryAfter, at.Sub(now))))
}

func destinationRateLimit(ctx context.Context, db *sql.DB, destination, fingerprint string) (time.Time, error) {
	if db == nil || fingerprint == "" {
		return time.Time{}, nil
	}
	var deadline string
	err := db.QueryRowContext(ctx, `
		SELECT rate_limited_until FROM notification_destination_health WHERE destination=? AND fingerprint=?
	`, destination, fingerprint).Scan(&deadline)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil || deadline == "" {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, deadline)
}
