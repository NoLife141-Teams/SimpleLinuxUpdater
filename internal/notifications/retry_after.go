package notifications

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
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

type rateLimitKey struct {
	destination string
	fingerprint string
}

type failedRateLimitCompletion struct {
	rowID    string
	outcome  outboxOutcome
	deadline time.Time
}

type failedRateLimitCompletions struct {
	mu    sync.Mutex
	items map[rateLimitKey]failedRateLimitCompletion
}

func (c *failedRateLimitCompletions) remember(key rateLimitKey, failed failedRateLimitCompletion, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[rateLimitKey]failedRateLimitCompletion)
	}
	for key, item := range c.items {
		if !item.deadline.After(now) {
			delete(c.items, key)
		}
	}
	c.items[key] = failed
}

func (c *failedRateLimitCompletions) get(key rateLimitKey) (failedRateLimitCompletion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	completion, ok := c.items[key]
	return completion, ok
}

func (c *failedRateLimitCompletions) forget(key rateLimitKey) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.items, key)
}

// A failed transaction leaves the claim in SQLite. Retry completing it before
// sending again on this configuration. Until it can be saved, the running
// worker still honors the provider's deadline without holding other lanes.
func (s *Service) recoverRateLimitCompletion(row outboxRow, key rateLimitKey) (handled, ok bool) {
	pending, exists := s.rateLimitCompletions.get(key)
	if !exists {
		return false, false
	}

	if err := s.persistOutboxCompletion(pending.rowID, pending.outcome); err == nil {
		s.rateLimitCompletions.forget(key)
		return row.ID == pending.rowID, true
	}
	delay := pending.deadline.Sub(s.deps.Now())
	if delay <= 0 {
		s.rateLimitCompletions.forget(key)
		return false, false
	}
	retryDelay := min(delay, max(s.deps.PollInterval, defaultOutboxPollInterval))
	s.releaseClaim(row, retryDelay, "destination retry deadline could not be persisted")
	return true, false
}
