package notifications

import (
	"context"
	"database/sql"
	"errors"
	"maps"
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

func (c *failedRateLimitCompletions) remember(key rateLimitKey, failed failedRateLimitCompletion) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.items == nil {
		c.items = make(map[rateLimitKey]failedRateLimitCompletion)
	}
	c.items[key] = failed
}

func (c *failedRateLimitCompletions) find(rowID string, key rateLimitKey) (rateLimitKey, failedRateLimitCompletion, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// A legacy unbound row can be reclaimed after its credentials rotate.
	// Finish its known HTTP attempt before revalidating the original binding.
	for actualKey, completion := range c.items {
		if completion.rowID == rowID {
			return actualKey, completion, true
		}
	}
	completion, ok := c.items[key]
	return key, completion, ok
}

func (c *failedRateLimitCompletions) deadline(key rateLimitKey) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.items[key].deadline
}

func (s *Service) knownRateLimit(ctx context.Context, db *sql.DB, key rateLimitKey) (time.Time, error) {
	// Read memory first so a concurrent successful commit cannot disappear
	// between the two reads when the recovery entry is removed.
	pending := s.rateLimitCompletions.deadline(key)
	until, err := destinationRateLimit(ctx, db, key.destination, key.fingerprint)
	if pending.After(until) {
		until = pending
	}
	return until, err
}

func (c *failedRateLimitCompletions) snapshot() map[rateLimitKey]failedRateLimitCompletion {
	c.mu.Lock()
	defer c.mu.Unlock()
	return maps.Clone(c.items)
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
	key, pending, exists := s.rateLimitCompletions.find(row.ID, key)
	if !exists {
		return false, false
	}

	err := s.persistOutboxCompletion(context.Background(), pending.rowID, pending.outcome, true)
	if err == nil {
		s.rateLimitCompletions.forget(key)
		return row.ID == pending.rowID, true
	}
	remaining := pending.deadline.Sub(s.deps.Now())
	if remaining <= 0 && errors.Is(err, errOutboxCompletionLostClaim) {
		s.rateLimitCompletions.forget(key)
		return false, false
	}
	// Expiry lifts the provider's pause, but cannot undo the HTTP attempt.
	// Keep its outcome until SQLite accepts it or the row has advanced.
	retryDelay := max(s.deps.PollInterval, defaultOutboxPollInterval)
	if remaining > 0 {
		retryDelay = min(remaining, retryDelay)
	}
	s.releaseClaim(row, retryDelay, "destination retry deadline could not be persisted")
	return true, false
}

// After draining the workers, a graceful restart or persistence replacement
// can keep a known response even when there are no other eligible rows.
func (s *Service) flushRateLimitCompletions(ctx context.Context) {
	for key, pending := range s.rateLimitCompletions.snapshot() {
		if err := s.persistOutboxCompletion(ctx, pending.rowID, pending.outcome, true); err == nil {
			s.rateLimitCompletions.forget(key)
		} else {
			s.deps.Logf("notification rate limit still could not be persisted for %q: %v", pending.rowID, err)
		}
	}
}
