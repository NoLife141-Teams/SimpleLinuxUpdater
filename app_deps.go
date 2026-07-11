package main

import (
	"database/sql"
	"fmt"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	"debian-updater/internal/events"
	healthpkg "debian-updater/internal/health"
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
	ApplicationTime        *apptimepkg.Module
	HostHealthObservation  healthpkg.Observation

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

func setGlobalSessionManager(sm *scs.SessionManager) {
	sessionManagerMu.Lock()
	sessionManager = sm
	sessionManagerMu.Unlock()
}
