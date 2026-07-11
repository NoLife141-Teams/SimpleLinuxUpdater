package observability

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type MetricsAccessStatus string

const (
	MetricsAccessEnabled     MetricsAccessStatus = "enabled"
	MetricsAccessDisabled    MetricsAccessStatus = "disabled"
	MetricsAccessUnavailable MetricsAccessStatus = "unavailable"
)

type MetricsAccessVerification string

const (
	MetricsAccessAccepted                MetricsAccessVerification = "accepted"
	MetricsAccessRejected                MetricsAccessVerification = "rejected"
	MetricsAccessDisabledVerification    MetricsAccessVerification = "disabled"
	MetricsAccessUnavailableVerification MetricsAccessVerification = "unavailable"
)

type MetricsAccessCredential interface {
	Status(context.Context) (MetricsAccessStatus, error)
	Rotate(context.Context) (string, error)
	Disable(context.Context) error
	Verify(context.Context, string) (MetricsAccessVerification, error)
	Invalidate()
}

type metricsAccessCredential struct {
	deps MetricsAccessCredentialDeps
	mu   sync.Mutex

	hash   string
	loaded bool
}

func NewMetricsAccessCredential(deps MetricsAccessCredentialDeps) MetricsAccessCredential {
	return &metricsAccessCredential{deps: deps.withDefaults()}
}

func (d MetricsAccessCredentialDeps) withDefaults() MetricsAccessCredentialDeps {
	if d.Store == nil {
		d.Store = unavailableMetricsCredentialStore{}
	}
	if d.RandomRead == nil {
		d.RandomRead = rand.Read
	}
	if d.HashPassword == nil {
		d.HashPassword = func(string) (string, error) { return "", errors.New("password hasher unavailable") }
	}
	if d.ComparePasswordAndHash == nil {
		d.ComparePasswordAndHash = func(string, string) (bool, error) { return false, nil }
	}
	if d.EntropyBytes <= 0 {
		d.EntropyBytes = DefaultMetricsTokenEntropy
	}
	return d
}

func (c *metricsAccessCredential) Status(ctx context.Context) (MetricsAccessStatus, error) {
	if c == nil {
		return MetricsAccessUnavailable, errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return MetricsAccessUnavailable, err
	}
	if c.hash == "" {
		return MetricsAccessDisabled, nil
	}
	return MetricsAccessEnabled, nil
}

func (c *metricsAccessCredential) Verify(ctx context.Context, presented string) (MetricsAccessVerification, error) {
	if c == nil {
		return MetricsAccessUnavailableVerification, errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return MetricsAccessUnavailableVerification, err
	}
	if c.hash == "" {
		return MetricsAccessDisabledVerification, nil
	}
	accepted, err := c.deps.ComparePasswordAndHash(presented, c.hash)
	if err != nil {
		return MetricsAccessUnavailableVerification, fmt.Errorf("verify Metrics Access Credential: %w", err)
	}
	if !accepted {
		return MetricsAccessRejected, nil
	}
	return MetricsAccessAccepted, nil
}

func (c *metricsAccessCredential) Rotate(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	buf := make([]byte, c.deps.EntropyBytes)
	n, err := c.deps.RandomRead(buf)
	if err != nil {
		return "", fmt.Errorf("generate Metrics Access Credential entropy: %w", err)
	}
	if n != len(buf) {
		return "", fmt.Errorf("generate Metrics Access Credential entropy: read %d of %d bytes", n, len(buf))
	}
	clear := base64.RawURLEncoding.EncodeToString(buf)
	hash, err := c.deps.HashPassword(clear)
	if err != nil {
		return "", fmt.Errorf("hash Metrics Access Credential: %w", err)
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return "", errors.New("hash Metrics Access Credential: empty hash")
	}
	if err := c.deps.Store.Replace(ctx, hash); err != nil {
		return "", fmt.Errorf("persist Metrics Access Credential: %w", err)
	}
	c.hash = hash
	c.loaded = true
	return clear, nil
}

func (c *metricsAccessCredential) Disable(ctx context.Context) error {
	if c == nil {
		return errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.deps.Store.Delete(ctx); err != nil {
		return fmt.Errorf("disable Metrics Access Credential: %w", err)
	}
	c.hash = ""
	c.loaded = true
	return nil
}

func (c *metricsAccessCredential) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.hash = ""
	c.loaded = false
}

func (c *metricsAccessCredential) loadLocked(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	hash, err := c.deps.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load Metrics Access Credential: %w", err)
	}
	c.hash = strings.TrimSpace(hash)
	c.loaded = true
	return nil
}

type unavailableMetricsCredentialStore struct{}

func (unavailableMetricsCredentialStore) Load(context.Context) (string, error) {
	return "", errors.New("metrics access credential store is unavailable")
}
func (unavailableMetricsCredentialStore) Replace(context.Context, string) error {
	return errors.New("metrics access credential store is unavailable")
}
func (unavailableMetricsCredentialStore) Delete(context.Context) error {
	return errors.New("metrics access credential store is unavailable")
}

type SQLiteMetricsCredentialStore struct {
	DB         func() *sql.DB
	SettingKey string
	RetryDelay time.Duration
}

func (s SQLiteMetricsCredentialStore) Load(ctx context.Context) (string, error) {
	if s.DB == nil {
		return "", errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return "", errors.New("metrics access credential database is unavailable")
	}
	key := strings.TrimSpace(s.SettingKey)
	if key == "" {
		key = DefaultMetricsTokenSettingKey
	}
	delay := s.RetryDelay
	if delay <= 0 {
		delay = 75 * time.Millisecond
	}
	for attempt := 1; attempt <= 3; attempt++ {
		var hash string
		err := db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&hash)
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		if err == nil {
			return strings.TrimSpace(hash), nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "database is locked") || attempt == 3 {
			return "", err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return "", ctx.Err()
		case <-timer.C:
		}
	}
	return "", errors.New("load Metrics Access Credential failed")
}

func (s SQLiteMetricsCredentialStore) Replace(ctx context.Context, hash string) error {
	if s.DB == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	key := strings.TrimSpace(s.SettingKey)
	if key == "" {
		key = DefaultMetricsTokenSettingKey
	}
	_, err := db.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, strings.TrimSpace(hash))
	return err
}

func (s SQLiteMetricsCredentialStore) Delete(ctx context.Context) error {
	if s.DB == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	key := strings.TrimSpace(s.SettingKey)
	if key == "" {
		key = DefaultMetricsTokenSettingKey
	}
	_, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", key)
	return err
}
