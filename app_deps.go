package main

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"debian-updater/internal/events"
	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/scs/v2"
)

type AppDeps struct {
	DB     func() *sql.DB
	DBPath func() string

	AuditService           *AuditService
	AuthService            *AuthService
	AuthSessionCommands    *authSessionCommands
	BackupService          *BackupService
	NotificationService    *NotificationService
	ServerState            *serverpkg.State
	ServerInventoryService *ServerInventoryService
	PolicyService          *PolicyService
	PolicyRepository       policypkg.Repository
	UpdateService          *UpdateService
	ObservabilityService   *ObservabilityService
	MetricsTokenService    *MetricsTokenService
	GlobalSSHCredential    *serverpkg.GlobalSSHCredential
	MaintenanceCoordinator *maintenancepkg.Coordinator

	JobManager           *JobManager
	CurrentJobManager    func() *JobManager
	NewJobManager        func(*sql.DB) *JobManager
	SetCurrentJobManager func(*JobManager)

	SessionManager            *scs.SessionManager
	CurrentSessionManager     func() *scs.SessionManager
	NewSessionManager         func(*sql.DB) (*scs.SessionManager, error)
	SetSessionManager         func(*scs.SessionManager)
	LoginRateLimiter          *AuthRateLimiter
	PasswordChangeRateLimiter *AuthRateLimiter
	SetupRateLimiter          *AuthRateLimiter
	MetricsRateLimiter        *AuthRateLimiter

	TrustedProxies                  func() []string
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

func reloadAppRuntimeState(deps AppDeps) error {
	if deps.DB == nil {
		deps.DB = getDB
	}
	_ = deps.DB()
	if deps.MaintenanceCoordinator != nil && !deps.MaintenanceCoordinator.Snapshot().Active {
		if err := deps.MaintenanceCoordinator.Initialize(context.Background()); err != nil {
			return err
		}
	}
	if deps.NewJobManager == nil {
		notify := deps.NotifyDashboardEvent
		deps.NewJobManager = func(db *sql.DB) *JobManager {
			return newJobManagerWithRuntime(db, notify, deps.ServerState, nil)
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
	if deps.GlobalSSHCredential != nil {
		deps.GlobalSSHCredential.ResetCache()
		_, _ = deps.GlobalSSHCredential.Resolve(context.Background(), "")
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
