package observability

import (
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type MetricsTokenService struct {
	deps MetricsTokenDeps
	mu   sync.RWMutex

	tokenHash string
	loaded    bool
	dbPath    string
}

func (s *MetricsTokenService) SnapshotCache() (string, bool, string) {
	if s == nil {
		return "", false, ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.tokenHash, s.loaded, s.dbPath
}

func (s *MetricsTokenService) RestoreCache(tokenHash string, loaded bool, dbPath string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenHash = tokenHash
	s.loaded = loaded
	s.dbPath = dbPath
}

func NewMetricsTokenService(deps MetricsTokenDeps) *MetricsTokenService {
	return &MetricsTokenService{deps: deps.withDefaults()}
}

func (s *MetricsTokenService) EnsureDeps() MetricsTokenDeps {
	if s == nil {
		return MetricsTokenDeps{}.withDefaults()
	}
	return s.deps.withDefaults()
}

func (d MetricsTokenDeps) withDefaults() MetricsTokenDeps {
	if d.DBPath == nil {
		d.DBPath = func() string { return "" }
	}
	if d.RandomRead == nil {
		d.RandomRead = func([]byte) (int, error) { return 0, errors.New("random reader unavailable") }
	}
	if d.HashPassword == nil {
		d.HashPassword = func(string) (string, error) { return "", errors.New("password hasher unavailable") }
	}
	if d.ComparePasswordAndHash == nil {
		d.ComparePasswordAndHash = func(string, string) (bool, error) { return false, nil }
	}
	if d.StateRLock == nil {
		d.StateRLock = func() {}
	}
	if d.StateRUnlock == nil {
		d.StateRUnlock = func() {}
	}
	if d.StateLock == nil {
		d.StateLock = func() {}
	}
	if d.StateUnlock == nil {
		d.StateUnlock = func() {}
	}
	if strings.TrimSpace(d.SettingKey) == "" {
		d.SettingKey = DefaultMetricsTokenSettingKey
	}
	if d.EntropyBytes <= 0 {
		d.EntropyBytes = DefaultMetricsTokenEntropy
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return d
}

func (s *MetricsTokenService) Status() bool {
	return strings.TrimSpace(s.Hash()) != ""
}

func (s *MetricsTokenService) GenerateToken() (string, error) {
	deps := s.EnsureDeps()
	buf := make([]byte, deps.EntropyBytes)
	if _, err := deps.RandomRead(buf); err != nil {
		return "", fmt.Errorf("generate metrics token entropy: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *MetricsTokenService) Hash() string {
	deps := s.EnsureDeps()
	cacheDBPath := deps.DBPath()
	db := deps.DB()

	deps.StateRLock()
	defer deps.StateRUnlock()

	s.mu.RLock()
	if s.loaded && s.dbPath == cacheDBPath {
		cached := s.tokenHash
		s.mu.RUnlock()
		return cached
	}
	cached := ""
	if s.dbPath == cacheDBPath {
		cached = s.tokenHash
	}
	s.mu.RUnlock()

	for attempt := 1; attempt <= 3; attempt++ {
		var tokenHash string
		err := db.QueryRow("SELECT value FROM settings WHERE key = ?", deps.SettingKey).Scan(&tokenHash)
		if err == sql.ErrNoRows {
			s.mu.Lock()
			s.tokenHash = ""
			s.loaded = true
			s.dbPath = cacheDBPath
			s.mu.Unlock()
			return ""
		}
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "database is locked") && attempt < 3 {
				time.Sleep(75 * time.Millisecond)
				continue
			}
			deps.Logf("Failed to load metrics bearer token hash: %v", err)
			return strings.TrimSpace(cached)
		}
		tokenHash = strings.TrimSpace(tokenHash)
		s.mu.Lock()
		s.tokenHash = tokenHash
		s.loaded = true
		s.dbPath = cacheDBPath
		s.mu.Unlock()
		return tokenHash
	}
	return strings.TrimSpace(cached)
}

func (s *MetricsTokenService) SetHash(tokenHash string) error {
	deps := s.EnsureDeps()
	tokenHash = strings.TrimSpace(tokenHash)
	if tokenHash == "" {
		return errors.New("metrics bearer token hash is required")
	}
	db := deps.DB()
	_, err := db.Exec(
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		deps.SettingKey, tokenHash,
	)
	if err != nil {
		return err
	}
	deps.StateLock()
	defer deps.StateUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenHash = tokenHash
	s.loaded = true
	s.dbPath = deps.DBPath()
	return nil
}

func (s *MetricsTokenService) Clear() error {
	deps := s.EnsureDeps()
	db := deps.DB()
	if _, err := db.Exec("DELETE FROM settings WHERE key = ?", deps.SettingKey); err != nil {
		return err
	}
	deps.StateLock()
	defer deps.StateUnlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokenHash = ""
	s.loaded = true
	s.dbPath = deps.DBPath()
	return nil
}

func (s *MetricsTokenService) Rotate() (string, error) {
	deps := s.EnsureDeps()
	token, err := s.GenerateToken()
	if err != nil {
		return "", err
	}
	tokenHash, err := deps.HashPassword(token)
	if err != nil {
		return "", err
	}
	if err := s.SetHash(tokenHash); err != nil {
		return "", err
	}
	return token, nil
}

func (s *MetricsTokenService) VerifyBearerToken(token string) (bool, error) {
	tokenHash := strings.TrimSpace(s.Hash())
	if tokenHash == "" {
		return false, sql.ErrNoRows
	}
	return s.EnsureDeps().ComparePasswordAndHash(token, tokenHash)
}
