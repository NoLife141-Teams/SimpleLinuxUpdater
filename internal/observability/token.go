package observability

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
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

type MetricsAccessLifecycleState string

const (
	MetricsAccessLifecycleDisabled  MetricsAccessLifecycleState = "disabled"
	MetricsAccessLifecycleUnknown   MetricsAccessLifecycleState = "unknown"
	MetricsAccessLifecycleNeverUsed MetricsAccessLifecycleState = "never_used"
	MetricsAccessLifecycleCurrent   MetricsAccessLifecycleState = "current"
	MetricsAccessLifecycleStale     MetricsAccessLifecycleState = "stale"
)

const (
	DefaultMetricsCredentialLifecycleSettingKey = "metrics_bearer_token_lifecycle_v1"
	DefaultMetricsCredentialStaleAfter          = 30 * 24 * time.Hour
	DefaultMetricsCredentialUsageWriteInterval  = time.Minute
)

type MetricsAccessDetails struct {
	Enabled              bool                        `json:"enabled"`
	LifecycleState       MetricsAccessLifecycleState `json:"lifecycle_state"`
	CreatedAt            string                      `json:"created_at"`
	RotatedAt            string                      `json:"rotated_at"`
	LastUsedAt           string                      `json:"last_used_at"`
	LastUsedOriginMasked string                      `json:"last_used_origin_masked"`
	NeverUsed            bool                        `json:"never_used"`
	Stale                bool                        `json:"stale"`
	StaleAfterDays       int                         `json:"stale_after_days"`
}

type MetricsAccessCredential interface {
	Status(context.Context) (MetricsAccessStatus, error)
	Details(context.Context) (MetricsAccessDetails, error)
	Rotate(context.Context) (string, error)
	Disable(context.Context) error
	Verify(context.Context, string) (MetricsAccessVerification, error)
	VerifyWithOrigin(context.Context, string, string) (MetricsAccessVerification, error)
	Invalidate()
}

type metricsAccessCredential struct {
	deps MetricsAccessCredentialDeps
	mu   sync.Mutex

	record                   MetricsCredentialRecord
	lastUsagePersistedAt     time.Time
	lastUsagePersistedOrigin string
	loaded                   bool
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
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.StaleAfter <= 0 {
		d.StaleAfter = DefaultMetricsCredentialStaleAfter
	}
	if d.UsageWriteInterval <= 0 {
		d.UsageWriteInterval = DefaultMetricsCredentialUsageWriteInterval
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
	if c.record.Hash == "" {
		return MetricsAccessDisabled, nil
	}
	return MetricsAccessEnabled, nil
}

func (c *metricsAccessCredential) Details(ctx context.Context) (MetricsAccessDetails, error) {
	if c == nil {
		return MetricsAccessDetails{}, errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return MetricsAccessDetails{}, err
	}
	return metricsAccessDetails(c.record, c.deps.Now().UTC(), c.deps.StaleAfter), nil
}

func (c *metricsAccessCredential) Verify(ctx context.Context, presented string) (MetricsAccessVerification, error) {
	return c.VerifyWithOrigin(ctx, presented, "unknown")
}

func (c *metricsAccessCredential) VerifyWithOrigin(ctx context.Context, presented, origin string) (MetricsAccessVerification, error) {
	if c == nil {
		return MetricsAccessUnavailableVerification, errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return MetricsAccessUnavailableVerification, err
	}
	if c.record.Hash == "" {
		return MetricsAccessDisabledVerification, nil
	}
	accepted, err := c.deps.ComparePasswordAndHash(presented, c.record.Hash)
	if err != nil {
		return MetricsAccessUnavailableVerification, fmt.Errorf("verify Metrics Access Credential: %w", err)
	}
	if !accepted {
		return MetricsAccessRejected, nil
	}
	now := c.deps.Now().UTC()
	lifecycle := c.record.Lifecycle
	maskedOrigin := maskMetricsCredentialOrigin(origin)
	if shouldPersistMetricsCredentialUsage(c.lastUsagePersistedAt, c.lastUsagePersistedOrigin, now, maskedOrigin, c.deps.UsageWriteInterval) {
		lifecycle.LastUsedAt = metricsCredentialTimestamp(now)
		lifecycle.LastUsedOriginMasked = maskedOrigin
		if err := c.deps.Store.UpdateLifecycle(ctx, lifecycle); err != nil {
			return MetricsAccessUnavailableVerification, fmt.Errorf("record Metrics Access Credential usage: %w", err)
		}
		c.lastUsagePersistedAt = now
		c.lastUsagePersistedOrigin = maskedOrigin
	} else {
		lifecycle.LastUsedAt = metricsCredentialTimestamp(now)
		lifecycle.LastUsedOriginMasked = maskedOrigin
	}
	c.record.Lifecycle = lifecycle
	return MetricsAccessAccepted, nil
}

func (c *metricsAccessCredential) Rotate(ctx context.Context) (string, error) {
	if c == nil {
		return "", errors.New("metrics access credential is unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.loadLocked(ctx); err != nil {
		return "", err
	}
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
	now := metricsCredentialTimestamp(c.deps.Now())
	lifecycle := MetricsCredentialLifecycle{CreatedAt: now}
	if c.loaded && c.record.Hash != "" {
		lifecycle.CreatedAt = c.record.Lifecycle.CreatedAt
		lifecycle.RotatedAt = now
	}
	record := MetricsCredentialRecord{Hash: hash, Lifecycle: lifecycle}
	if err := c.deps.Store.Replace(ctx, record); err != nil {
		return "", fmt.Errorf("persist Metrics Access Credential: %w", err)
	}
	c.record = record
	c.lastUsagePersistedAt = time.Time{}
	c.lastUsagePersistedOrigin = ""
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
	c.record = MetricsCredentialRecord{}
	c.lastUsagePersistedAt = time.Time{}
	c.lastUsagePersistedOrigin = ""
	c.loaded = true
	return nil
}

func (c *metricsAccessCredential) Invalidate() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.record = MetricsCredentialRecord{}
	c.lastUsagePersistedAt = time.Time{}
	c.lastUsagePersistedOrigin = ""
	c.loaded = false
}

func (c *metricsAccessCredential) loadLocked(ctx context.Context) error {
	if c.loaded {
		return nil
	}
	record, err := c.deps.Store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load Metrics Access Credential: %w", err)
	}
	record.Hash = strings.TrimSpace(record.Hash)
	record.Lifecycle = normalizeMetricsCredentialLifecycle(record.Lifecycle)
	c.record = record
	c.lastUsagePersistedAt, _ = time.Parse(time.RFC3339, record.Lifecycle.LastUsedAt)
	c.lastUsagePersistedOrigin = record.Lifecycle.LastUsedOriginMasked
	c.loaded = true
	return nil
}

type unavailableMetricsCredentialStore struct{}

func (unavailableMetricsCredentialStore) Load(context.Context) (MetricsCredentialRecord, error) {
	return MetricsCredentialRecord{}, errors.New("metrics access credential store is unavailable")
}
func (unavailableMetricsCredentialStore) Replace(context.Context, MetricsCredentialRecord) error {
	return errors.New("metrics access credential store is unavailable")
}
func (unavailableMetricsCredentialStore) UpdateLifecycle(context.Context, MetricsCredentialLifecycle) error {
	return errors.New("metrics access credential store is unavailable")
}
func (unavailableMetricsCredentialStore) Delete(context.Context) error {
	return errors.New("metrics access credential store is unavailable")
}

type SQLiteMetricsCredentialStore struct {
	DB                  func() *sql.DB
	SettingKey          string
	LifecycleSettingKey string
	RetryDelay          time.Duration
}

func (s SQLiteMetricsCredentialStore) Load(ctx context.Context) (MetricsCredentialRecord, error) {
	if s.DB == nil {
		return MetricsCredentialRecord{}, errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return MetricsCredentialRecord{}, errors.New("metrics access credential database is unavailable")
	}
	key, lifecycleKey := s.settingKeys()
	delay := s.RetryDelay
	if delay <= 0 {
		delay = 75 * time.Millisecond
	}
	for attempt := 1; attempt <= 3; attempt++ {
		record, err := loadMetricsCredentialRecord(ctx, db, key, lifecycleKey)
		if err == nil {
			return record, nil
		}
		if !strings.Contains(strings.ToLower(err.Error()), "database is locked") || attempt == 3 {
			return MetricsCredentialRecord{}, err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return MetricsCredentialRecord{}, ctx.Err()
		case <-timer.C:
		}
	}
	return MetricsCredentialRecord{}, errors.New("load Metrics Access Credential failed")
}

func (s SQLiteMetricsCredentialStore) Replace(ctx context.Context, record MetricsCredentialRecord) error {
	if s.DB == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	key, lifecycleKey := s.settingKeys()
	lifecycle, err := json.Marshal(normalizeMetricsCredentialLifecycle(record.Lifecycle))
	if err != nil {
		return fmt.Errorf("encode Metrics Access Credential lifecycle: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, strings.TrimSpace(record.Hash)); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", lifecycleKey, string(lifecycle)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s SQLiteMetricsCredentialStore) UpdateLifecycle(ctx context.Context, lifecycle MetricsCredentialLifecycle) error {
	if s.DB == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	db := s.DB()
	if db == nil {
		return errors.New("metrics access credential database is unavailable")
	}
	_, lifecycleKey := s.settingKeys()
	encoded, err := json.Marshal(normalizeMetricsCredentialLifecycle(lifecycle))
	if err != nil {
		return fmt.Errorf("encode Metrics Access Credential lifecycle: %w", err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", lifecycleKey, string(encoded))
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
	key, lifecycleKey := s.settingKeys()
	_, err := db.ExecContext(ctx, "DELETE FROM settings WHERE key IN (?, ?)", key, lifecycleKey)
	return err
}

func (s SQLiteMetricsCredentialStore) settingKeys() (string, string) {
	key := strings.TrimSpace(s.SettingKey)
	if key == "" {
		key = DefaultMetricsTokenSettingKey
	}
	lifecycleKey := strings.TrimSpace(s.LifecycleSettingKey)
	if lifecycleKey == "" {
		lifecycleKey = DefaultMetricsCredentialLifecycleSettingKey
	}
	return key, lifecycleKey
}

func loadMetricsCredentialRecord(ctx context.Context, db *sql.DB, hashKey, lifecycleKey string) (MetricsCredentialRecord, error) {
	rows, err := db.QueryContext(ctx, "SELECT key, value FROM settings WHERE key IN (?, ?)", hashKey, lifecycleKey)
	if err != nil {
		return MetricsCredentialRecord{}, err
	}
	defer rows.Close()
	record := MetricsCredentialRecord{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return MetricsCredentialRecord{}, err
		}
		switch key {
		case hashKey:
			record.Hash = strings.TrimSpace(value)
		case lifecycleKey:
			var lifecycle MetricsCredentialLifecycle
			if json.Unmarshal([]byte(value), &lifecycle) == nil {
				record.Lifecycle = normalizeMetricsCredentialLifecycle(lifecycle)
			}
		}
	}
	return record, rows.Err()
}

func metricsAccessDetails(record MetricsCredentialRecord, now time.Time, staleAfter time.Duration) MetricsAccessDetails {
	enabled := strings.TrimSpace(record.Hash) != ""
	lifecycle := normalizeMetricsCredentialLifecycle(record.Lifecycle)
	details := MetricsAccessDetails{
		Enabled:              enabled,
		LifecycleState:       MetricsAccessLifecycleDisabled,
		CreatedAt:            lifecycle.CreatedAt,
		RotatedAt:            lifecycle.RotatedAt,
		LastUsedAt:           lifecycle.LastUsedAt,
		LastUsedOriginMasked: lifecycle.LastUsedOriginMasked,
		StaleAfterDays:       int(staleAfter / (24 * time.Hour)),
	}
	if !enabled {
		return details
	}
	if lifecycle.LastUsedAt == "" {
		if lifecycle.CreatedAt == "" && lifecycle.RotatedAt == "" {
			details.LifecycleState = MetricsAccessLifecycleUnknown
			return details
		}
		details.LifecycleState = MetricsAccessLifecycleNeverUsed
		details.NeverUsed = true
		return details
	}
	lastUsed, err := time.Parse(time.RFC3339, lifecycle.LastUsedAt)
	if err == nil && now.Sub(lastUsed) >= staleAfter {
		details.LifecycleState = MetricsAccessLifecycleStale
		details.Stale = true
		return details
	}
	details.LifecycleState = MetricsAccessLifecycleCurrent
	return details
}

func normalizeMetricsCredentialLifecycle(lifecycle MetricsCredentialLifecycle) MetricsCredentialLifecycle {
	lifecycle.CreatedAt = normalizeMetricsCredentialTimestamp(lifecycle.CreatedAt)
	lifecycle.RotatedAt = normalizeMetricsCredentialTimestamp(lifecycle.RotatedAt)
	lifecycle.LastUsedAt = normalizeMetricsCredentialTimestamp(lifecycle.LastUsedAt)
	lifecycle.LastUsedOriginMasked = maskMetricsCredentialOrigin(lifecycle.LastUsedOriginMasked)
	if lifecycle.LastUsedAt == "" {
		lifecycle.LastUsedOriginMasked = ""
	}
	return lifecycle
}

func normalizeMetricsCredentialTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(time.RFC3339)
}

func metricsCredentialTimestamp(value time.Time) string {
	return value.UTC().Format(time.RFC3339)
}

func maskMetricsCredentialOrigin(origin string) string {
	value := strings.TrimSpace(origin)
	if value == "" || strings.EqualFold(value, "unknown") {
		return "unknown"
	}
	if strings.HasSuffix(value, ".x") {
		parts := strings.Split(value, ".")
		if len(parts) == 4 && parts[3] == "x" {
			candidate := net.ParseIP(strings.Join([]string{parts[0], parts[1], parts[2], "0"}, "."))
			if candidate != nil && candidate.To4() != nil {
				return value
			}
		}
		return "unknown"
	}
	if strings.HasSuffix(value, "/64") {
		candidate := net.ParseIP(strings.TrimSuffix(value, "/64"))
		if candidate == nil || candidate.To4() != nil {
			return "unknown"
		}
		return maskMetricsCredentialIP(candidate)
	}
	ip := net.ParseIP(value)
	if ip == nil {
		return "unknown"
	}
	return maskMetricsCredentialIP(ip)
}

func shouldPersistMetricsCredentialUsage(lastPersistedAt time.Time, lastPersistedOrigin string, now time.Time, maskedOrigin string, interval time.Duration) bool {
	if lastPersistedOrigin != maskedOrigin {
		return true
	}
	return lastPersistedAt.IsZero() || now.Sub(lastPersistedAt) >= interval
}

func maskMetricsCredentialIP(ip net.IP) string {
	if ipv4 := ip.To4(); ipv4 != nil {
		return fmt.Sprintf("%d.%d.%d.x", ipv4[0], ipv4[1], ipv4[2])
	}
	ipv6 := ip.To16()
	if ipv6 == nil {
		return "unknown"
	}
	masked := append(net.IP(nil), ipv6...)
	for i := 8; i < len(masked); i++ {
		masked[i] = 0
	}
	return masked.String() + "/64"
}
