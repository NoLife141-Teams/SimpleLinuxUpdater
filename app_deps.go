package main

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"debian-updater/internal/events"
	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/scs/v2"
)

type AppDeps struct {
	DB     func() *sql.DB
	DBPath func() string

	AuditService           *AuditService
	AuthService            *AuthService
	BackupService          *BackupService
	BackupBarrier          *BackupBarrier
	NotificationService    *NotificationService
	ServerState            *serverpkg.State
	ServerInventoryService *ServerInventoryService
	PolicyService          *PolicyService
	PolicyRepository       policypkg.Repository
	UpdateService          *UpdateService
	ObservabilityService   *ObservabilityService
	MetricsTokenService    *MetricsTokenService

	JobManager           *JobManager
	CurrentJobManager    func() *JobManager
	NewJobManager        func(*sql.DB) *JobManager
	SetCurrentJobManager func(*JobManager)

	GetGlobalKey   func() string
	SetGlobalKey   func(string) error
	ClearGlobalKey func() error
	HasGlobalKey   func() (bool, error)

	SessionManager            *scs.SessionManager
	CurrentSessionManager     func() *scs.SessionManager
	NewSessionManager         func(*sql.DB) (*scs.SessionManager, error)
	SetSessionManager         func(*scs.SessionManager)
	LoginRateLimiter          *AuthRateLimiter
	PasswordChangeRateLimiter *AuthRateLimiter
	SetupRateLimiter          *AuthRateLimiter
	MetricsRateLimiter        *AuthRateLimiter

	TrustedProxies                  func() []string
	InitializeMaintenanceState      func() error
	CurrentMaintenanceActive        func() bool
	Now                             func() time.Time
	JobTimestampNow                 func() string
	LoadRetryPolicy                 func() RetryPolicy
	StartJobRunner                  func(string, func())
	StartScheduledRunReconciliation func(int64, string)
	NotifyDashboardEvent            func(string)
	DashboardEventBroker            *events.Broker
	CurrentAppTimezone              func() (*time.Location, string)
	CurrentAppLocation              func() *time.Location
	AppTimezoneDisplayName          func() string
	AppTimezoneResolvedName         func() string
}

func NewDefaultAppDeps() AppDeps {
	return AppDeps{}.withDefaults()
}

func (deps AppDeps) withDefaults() AppDeps {
	return newRuntimeComposition(deps).Compose()
}

func (deps AppDeps) initializeJobManager() error {
	deps = deps.withDefaults()
	jm := deps.JobManager
	if jm == nil {
		jm = deps.NewJobManager(deps.DB())
	}
	if jm == nil {
		return fmt.Errorf("job manager unavailable")
	}
	if err := jm.MarkUnfinishedJobsInterrupted(); err != nil {
		return err
	}
	deps.SetCurrentJobManager(jm)
	return nil
}

func (deps AppDeps) initializeSessionManager() error {
	deps = deps.withDefaults()
	sm := deps.SessionManager
	if sm == nil {
		var err error
		sm, err = deps.NewSessionManager(deps.DB())
		if err != nil {
			return err
		}
	}
	deps.SetSessionManager(sm)
	return nil
}

func newAppGlobalKeyStore(dbProvider func() *sql.DB) (func() string, func(string) error, func() error, func() (bool, error)) {
	if dbProvider == nil {
		dbProvider = getDB
	}
	var keyMu sync.RWMutex
	cachedKey := ""
	getCached := func() string {
		keyMu.RLock()
		defer keyMu.RUnlock()
		return cachedKey
	}
	setCached := func(key string) {
		keyMu.Lock()
		cachedKey = key
		keyMu.Unlock()
		globalKeyMu.Lock()
		globalKey = key
		globalKeyMu.Unlock()
	}
	getKey := func() string {
		db := dbProvider()
		for attempt := 1; attempt <= 3; attempt++ {
			var enc string
			err := db.QueryRow("SELECT value FROM settings WHERE key = ?", globalKeySetting).Scan(&enc)
			if err == sql.ErrNoRows {
				setCached("")
				return ""
			}
			if err != nil {
				if strings.Contains(strings.ToLower(err.Error()), "database is locked") && attempt < 3 {
					time.Sleep(75 * time.Millisecond)
					continue
				}
				cached := getCached()
				log.Printf("Failed to load global SSH key: %v", err)
				if strings.TrimSpace(cached) != "" {
					log.Printf("Using cached global SSH key due to read failure")
				}
				return cached
			}
			key, decErr := decryptSecret(enc)
			if decErr != nil {
				cached := getCached()
				log.Printf("Failed to decrypt global SSH key: %v", decErr)
				if strings.TrimSpace(cached) != "" {
					log.Printf("Using cached global SSH key due to decrypt failure")
				}
				return cached
			}
			setCached(key)
			return key
		}
		return ""
	}
	setKey := func(key string) error {
		enc, err := encryptSecret(key)
		if err != nil {
			return err
		}
		if _, err := dbProvider().Exec(
			"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
			globalKeySetting, enc,
		); err != nil {
			return err
		}
		setCached(key)
		return nil
	}
	clearKey := func() error {
		if _, err := dbProvider().Exec("DELETE FROM settings WHERE key = ?", globalKeySetting); err != nil {
			return err
		}
		setCached("")
		return nil
	}
	hasKey := func() (bool, error) {
		var enc string
		err := dbProvider().QueryRow("SELECT value FROM settings WHERE key = ?", globalKeySetting).Scan(&enc)
		if err == sql.ErrNoRows {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if strings.TrimSpace(enc) == "" {
			return false, nil
		}
		return true, nil
	}
	return getKey, setKey, clearKey, hasKey
}

func reloadAppRuntimeState(deps AppDeps) error {
	if deps.DB == nil {
		deps.DB = getDB
	}
	_ = deps.DB()
	if deps.CurrentMaintenanceActive == nil {
		deps.CurrentMaintenanceActive = func() bool {
			return currentMaintenanceState().Active
		}
	}
	if deps.InitializeMaintenanceState == nil {
		deps.InitializeMaintenanceState = initializeMaintenanceState
	}
	if !deps.CurrentMaintenanceActive() {
		if err := deps.InitializeMaintenanceState(); err != nil {
			return err
		}
	}
	if deps.NewJobManager == nil {
		notify := deps.NotifyDashboardEvent
		deps.NewJobManager = func(db *sql.DB) *JobManager {
			return newJobManagerWithRuntime(db, notify, deps.ServerState, deps.CurrentMaintenanceActive)
		}
	}
	if deps.SetCurrentJobManager == nil {
		deps.SetCurrentJobManager = setCurrentJobManager
	}
	jm := deps.NewJobManager(deps.DB())
	if jm == nil {
		return fmt.Errorf("job manager unavailable")
	}
	if err := jm.MarkUnfinishedJobsInterrupted(); err != nil {
		return err
	}
	deps.SetCurrentJobManager(jm)
	if deps.ServerInventoryService != nil {
		deps.ServerInventoryService.Load()
		initializeServerStateStatuses(deps.ServerState)
	} else {
		loadServers()
	}
	if deps.GetGlobalKey != nil {
		_ = deps.GetGlobalKey()
	} else {
		_ = getGlobalKey()
	}
	_ = getMetricsBearerTokenHash()
	if deps.NewSessionManager == nil {
		deps.NewSessionManager = newSessionManager
	}
	if deps.SetSessionManager == nil {
		deps.SetSessionManager = setGlobalSessionManager
	}
	sm, err := deps.NewSessionManager(deps.DB())
	if err != nil {
		return err
	}
	deps.SetSessionManager(sm)
	return nil
}

func setGlobalSessionManager(sm *scs.SessionManager) {
	sessionManagerMu.Lock()
	sessionManager = sm
	sessionManagerMu.Unlock()
}
