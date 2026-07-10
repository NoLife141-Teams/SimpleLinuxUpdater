package servers

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const globalSSHCredentialSetting = "global_ssh_key"

type GlobalSSHCredentialSource string

const (
	GlobalSSHCredentialSourceNone   GlobalSSHCredentialSource = "none"
	GlobalSSHCredentialSourceServer GlobalSSHCredentialSource = "server"
	GlobalSSHCredentialSourceGlobal GlobalSSHCredentialSource = "global"
)

type GlobalSSHCredentialResolution struct {
	Key      string
	Source   GlobalSSHCredentialSource
	Degraded bool
}

type GlobalSSHCredentialStatus struct {
	Configured bool
}

type GlobalSSHCredentialStore interface {
	Read(context.Context) (string, error)
	Write(context.Context, string) error
	Delete(context.Context) error
}

type SQLiteGlobalSSHCredentialStore struct {
	DB func() *sql.DB
	Tx *sql.Tx
}

type globalSSHCredentialSQL interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func (s SQLiteGlobalSSHCredentialStore) connection() (globalSSHCredentialSQL, error) {
	if s.Tx != nil {
		return s.Tx, nil
	}
	if s.DB == nil {
		return nil, errors.New("database is not initialized")
	}
	db := s.DB()
	if db == nil {
		return nil, errors.New("database is not initialized")
	}
	return db, nil
}

func (s SQLiteGlobalSSHCredentialStore) Read(ctx context.Context) (string, error) {
	db, err := s.connection()
	if err != nil {
		return "", err
	}
	var encrypted string
	err = db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", globalSSHCredentialSetting).Scan(&encrypted)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return encrypted, err
}

func (s SQLiteGlobalSSHCredentialStore) Write(ctx context.Context, encrypted string) error {
	db, err := s.connection()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		globalSSHCredentialSetting,
		encrypted,
	)
	return err
}

func (s SQLiteGlobalSSHCredentialStore) Delete(ctx context.Context) error {
	db, err := s.connection()
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, "DELETE FROM settings WHERE key = ?", globalSSHCredentialSetting)
	return err
}

type GlobalSSHCredentialDeps struct {
	Store               GlobalSSHCredentialStore
	Encrypt             func(string) (string, error)
	Decrypt             func(string) (string, error)
	Validate            func(string) error
	ActiveServerActions func() []string
	ReadAttempts        int
	RetryDelay          time.Duration
	Sleep               func(time.Duration)
	Logf                func(string, ...any)
}

type GlobalSSHCredential struct {
	deps GlobalSSHCredentialDeps

	cacheMu         sync.RWMutex
	cachedKey       string
	cacheLoaded     bool
	cacheGeneration uint64
}

func NewGlobalSSHCredential(deps GlobalSSHCredentialDeps) *GlobalSSHCredential {
	if deps.Validate == nil {
		deps.Validate = ValidateGlobalSSHCredential
	}
	if deps.ReadAttempts <= 0 {
		deps.ReadAttempts = 3
	}
	if deps.RetryDelay <= 0 {
		deps.RetryDelay = 75 * time.Millisecond
	}
	if deps.Sleep == nil {
		deps.Sleep = time.Sleep
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	if deps.ActiveServerActions == nil {
		deps.ActiveServerActions = func() []string { return nil }
	}
	return &GlobalSSHCredential{deps: deps}
}

func (c *GlobalSSHCredential) Resolve(ctx context.Context, perServerKey string) (GlobalSSHCredentialResolution, error) {
	if key := strings.TrimSpace(perServerKey); key != "" {
		return GlobalSSHCredentialResolution{Key: key, Source: GlobalSSHCredentialSourceServer}, nil
	}
	if c == nil || c.deps.Store == nil {
		return GlobalSSHCredentialResolution{}, errors.New("global SSH credential is not configured")
	}

	generation := c.generation()
	encrypted, err := c.read(ctx)
	if err != nil {
		if cached, ok := c.cached(); ok {
			c.deps.Logf("global SSH credential read failed; using last-known-good credential: %v", err)
			return cachedGlobalSSHCredentialResolution(cached), nil
		}
		return GlobalSSHCredentialResolution{}, fmt.Errorf("load global SSH credential: %w", err)
	}
	if strings.TrimSpace(encrypted) == "" {
		c.updateCacheIfCurrent(generation, "", true)
		return GlobalSSHCredentialResolution{Source: GlobalSSHCredentialSourceNone}, nil
	}
	if c.deps.Decrypt == nil {
		return GlobalSSHCredentialResolution{}, errors.New("global SSH credential decryption is not configured")
	}
	key, err := c.deps.Decrypt(encrypted)
	if err != nil {
		if cached, ok := c.cached(); ok {
			c.deps.Logf("global SSH credential decrypt failed; using last-known-good credential: %v", err)
			return cachedGlobalSSHCredentialResolution(cached), nil
		}
		return GlobalSSHCredentialResolution{}, fmt.Errorf("decrypt global SSH credential: %w", err)
	}
	c.updateCacheIfCurrent(generation, key, true)
	if strings.TrimSpace(key) == "" {
		return GlobalSSHCredentialResolution{Source: GlobalSSHCredentialSourceNone}, nil
	}
	return GlobalSSHCredentialResolution{Key: key, Source: GlobalSSHCredentialSourceGlobal}, nil
}

func cachedGlobalSSHCredentialResolution(key string) GlobalSSHCredentialResolution {
	if strings.TrimSpace(key) == "" {
		return GlobalSSHCredentialResolution{Source: GlobalSSHCredentialSourceNone, Degraded: true}
	}
	return GlobalSSHCredentialResolution{Key: key, Source: GlobalSSHCredentialSourceGlobal, Degraded: true}
}

func (c *GlobalSSHCredential) Status(ctx context.Context) (GlobalSSHCredentialStatus, error) {
	if c == nil || c.deps.Store == nil {
		return GlobalSSHCredentialStatus{}, errors.New("global SSH credential is not configured")
	}
	encrypted, err := c.read(ctx)
	if err != nil {
		return GlobalSSHCredentialStatus{}, fmt.Errorf("read global SSH credential status: %w", err)
	}
	return GlobalSSHCredentialStatus{Configured: strings.TrimSpace(encrypted) != ""}, nil
}

func (c *GlobalSSHCredential) ReencryptStored(ctx context.Context) error {
	if c == nil || c.deps.Store == nil || c.deps.Decrypt == nil || c.deps.Encrypt == nil {
		return errors.New("global SSH credential re-encryption is not configured")
	}
	encrypted, err := c.read(ctx)
	if err != nil {
		return fmt.Errorf("read global SSH credential for re-encryption: %w", err)
	}
	if strings.TrimSpace(encrypted) == "" {
		return nil
	}
	key, err := c.deps.Decrypt(encrypted)
	if err != nil {
		return fmt.Errorf("decrypt global SSH credential for re-encryption: %w", err)
	}
	reencrypted, err := c.deps.Encrypt(key)
	if err != nil {
		return fmt.Errorf("encrypt global SSH credential for re-encryption: %w", err)
	}
	if err := c.deps.Store.Write(ctx, reencrypted); err != nil {
		return fmt.Errorf("persist re-encrypted global SSH credential: %w", err)
	}
	c.replaceCache(key)
	return nil
}

func (c *GlobalSSHCredential) Replace(ctx context.Context, key string) CommandResult {
	if blocked := c.blockedMutation("global_key.upload"); blocked != nil {
		return *blocked
	}
	if c == nil || c.deps.Store == nil || c.deps.Encrypt == nil {
		return globalCredentialFailure("global_key.upload", "Failed to save global key", errors.New("global SSH credential persistence is not configured"))
	}
	key = strings.TrimSpace(key)
	if err := c.deps.Validate(key); err != nil {
		return failedCommand(CommandOutcomeInvalid, "global_key.upload", "global_key", "global", "Invalid global SSH key", err.Error(), nil)
	}
	encrypted, err := c.deps.Encrypt(key)
	if err != nil {
		return globalCredentialFailure("global_key.upload", "Failed to save global key", err)
	}
	if err := c.deps.Store.Write(ctx, encrypted); err != nil {
		return globalCredentialFailure("global_key.upload", "Failed to save global key", err)
	}
	c.replaceCache(key)
	return successMessageCommand("global_key.upload", "global_key", "global", "Global key saved", nil)
}

func ValidateGlobalSSHCredential(key string) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("private key is empty")
	}
	if _, err := ssh.ParsePrivateKey([]byte(key)); err != nil {
		var passphraseMissing *ssh.PassphraseMissingError
		if errors.As(err, &passphraseMissing) {
			return errors.New("passphrase-protected private keys are not supported")
		}
		return errors.New("value is not a usable private SSH key")
	}
	return nil
}

func (c *GlobalSSHCredential) CheckReplace() CommandResult {
	if blocked := c.blockedMutation("global_key.upload"); blocked != nil {
		return *blocked
	}
	return CommandResult{Outcome: CommandOutcomeSuccess}
}

func (c *GlobalSSHCredential) Clear(ctx context.Context) CommandResult {
	if blocked := c.blockedMutation("global_key.clear"); blocked != nil {
		return *blocked
	}
	if c == nil || c.deps.Store == nil {
		return globalCredentialFailure("global_key.clear", "Failed to clear global key", errors.New("global SSH credential persistence is not configured"))
	}
	if err := c.deps.Store.Delete(ctx); err != nil {
		return globalCredentialFailure("global_key.clear", "Failed to clear global key", err)
	}
	c.replaceCache("")
	return successMessageCommand("global_key.clear", "global_key", "global", "Global key cleared", nil)
}

func (c *GlobalSSHCredential) ResetCache() {
	if c == nil {
		return
	}
	c.replaceCache("")
	c.cacheMu.Lock()
	c.cacheLoaded = false
	c.cacheMu.Unlock()
}

func (c *GlobalSSHCredential) blockedMutation(action string) *CommandResult {
	if c == nil {
		return nil
	}
	active := append([]string(nil), c.deps.ActiveServerActions()...)
	if len(active) == 0 {
		return nil
	}
	sort.Strings(active)
	result := failedCommand(CommandOutcomeConflict, action, "global_key", "global", "Server action already in progress", "wait for active server actions to finish before changing the global SSH key", map[string]any{"active_servers": active})
	result.ActiveServers = active
	return &result
}

func (c *GlobalSSHCredential) read(ctx context.Context) (string, error) {
	var value string
	var err error
	for attempt := 1; attempt <= c.deps.ReadAttempts; attempt++ {
		value, err = c.deps.Store.Read(ctx)
		if err == nil || !strings.Contains(strings.ToLower(err.Error()), "database is locked") || attempt == c.deps.ReadAttempts {
			return value, err
		}
		c.deps.Sleep(c.deps.RetryDelay)
	}
	return value, err
}

func (c *GlobalSSHCredential) generation() uint64 {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.cacheGeneration
}

func (c *GlobalSSHCredential) cached() (string, bool) {
	c.cacheMu.RLock()
	defer c.cacheMu.RUnlock()
	return c.cachedKey, c.cacheLoaded
}

func (c *GlobalSSHCredential) updateCacheIfCurrent(generation uint64, key string, loaded bool) {
	c.cacheMu.Lock()
	defer c.cacheMu.Unlock()
	if c.cacheGeneration != generation {
		return
	}
	c.cachedKey = key
	c.cacheLoaded = loaded
}

func (c *GlobalSSHCredential) replaceCache(key string) {
	c.cacheMu.Lock()
	c.cachedKey = key
	c.cacheLoaded = true
	c.cacheGeneration++
	c.cacheMu.Unlock()
}

func globalCredentialFailure(action, message string, err error) CommandResult {
	return failedCommand(CommandOutcomeFailed, action, "global_key", "global", message, err.Error(), map[string]any{"error": err.Error()})
}
