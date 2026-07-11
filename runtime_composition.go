package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	internalbackup "debian-updater/internal/backup"
	"debian-updater/internal/events"
	healthpkg "debian-updater/internal/health"
	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/crypto/ssh"
)

type runtimeComposition struct {
	deps        AppDeps
	resetCaches func()
}

func newRuntimeComposition(overrides AppDeps) *runtimeComposition {
	return &runtimeComposition{deps: overrides, resetCaches: resetRuntimeCaches}
}

func (c *runtimeComposition) PreparePersistenceReplacement(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("prepare persistence replacement: runtime composition is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("prepare persistence replacement: %w", err)
	}
	if c.resetCaches != nil {
		c.resetCaches()
	}
	if c.deps.GlobalSSHCredential != nil {
		c.deps.GlobalSSHCredential.ResetCache()
	}
	if c.deps.MetricsTokenService != nil {
		c.deps.MetricsTokenService.RestoreCache("", false, "")
	}
	return nil
}

func (c *runtimeComposition) ReloadRestoredState(ctx context.Context) error {
	if c == nil {
		return fmt.Errorf("reload restored runtime: runtime composition is unavailable")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reload restored runtime: %w", err)
	}
	if err := c.PreparePersistenceReplacement(ctx); err != nil {
		return err
	}
	deps := c.deps
	if deps.DB == nil {
		return fmt.Errorf("reopen restored persistence: database is unavailable")
	}
	db := deps.DB()
	if db == nil {
		return fmt.Errorf("reopen restored persistence: database is unavailable")
	}
	if deps.MaintenanceCoordinator != nil && !deps.MaintenanceCoordinator.Snapshot().Active {
		if err := deps.MaintenanceCoordinator.Initialize(ctx); err != nil {
			return fmt.Errorf("restore Maintenance Coordination: %w", err)
		}
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reload restored runtime: %w", err)
	}
	if deps.NewJobManager == nil || deps.SetCurrentJobManager == nil {
		return fmt.Errorf("rebuild restored job manager: runtime dependency is unavailable")
	}
	jm := deps.NewJobManager(db)
	if jm == nil {
		return fmt.Errorf("rebuild restored job manager: job manager unavailable")
	}
	if err := jm.MarkUnfinishedJobsInterrupted(); err != nil {
		return fmt.Errorf("interrupt unfinished restored jobs: %w", err)
	}
	deps.SetCurrentJobManager(jm)
	if deps.ServerInventoryService == nil || deps.ServerState == nil {
		return fmt.Errorf("reload restored Server inventory: runtime dependency is unavailable")
	}
	deps.ServerInventoryService.Load()
	initializeServerStateStatuses(deps.ServerState)
	if deps.GlobalSSHCredential != nil {
		deps.GlobalSSHCredential.ResetCache()
		_, _ = deps.GlobalSSHCredential.Resolve(ctx, "")
	}
	if deps.MetricsTokenService != nil {
		deps.MetricsTokenService.RestoreCache("", false, "")
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("reload restored runtime: %w", err)
	}
	if deps.NewSessionManager == nil || deps.SetSessionManager == nil {
		return fmt.Errorf("rebuild restored auth session manager: runtime dependency is unavailable")
	}
	sm, err := deps.NewSessionManager(db)
	if err != nil {
		return fmt.Errorf("rebuild restored auth session manager: %w", err)
	}
	deps.SetSessionManager(sm)
	return nil
}

func (c *runtimeComposition) Compose() AppDeps {
	deps := c.deps
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.DBPath == nil {
		deps.DBPath = dbPath
	}
	if deps.ApplicationTime == nil {
		deps.ApplicationTime = apptimepkg.New(apptimepkg.Deps{Store: appTimeSQLiteStore{}, Detector: appTimeSystemDetector{}})
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.HostHealthObservation == nil {
		deps.HostHealthObservation = healthpkg.SQLiteObservation{DB: deps.DB, Now: deps.Now}
	}
	if deps.JobTimestampNow == nil {
		deps.JobTimestampNow = jobTimestampNow
	}
	if deps.LoadRetryPolicy == nil {
		deps.LoadRetryPolicy = loadRetryPolicyFromEnv
	}
	if deps.DashboardEventBroker == nil {
		deps.DashboardEventBroker = events.NewBroker()
	}
	if deps.NotifyDashboardEvent == nil {
		broker := deps.DashboardEventBroker
		deps.NotifyDashboardEvent = func(reason string) {
			if broker != nil {
				broker.Publish(reason)
			}
		}
	}
	if deps.NotificationService == nil {
		deps.NotificationService = NewNotificationService(NotificationServiceDeps{
			DB: deps.DB,
		})
	}
	if deps.AuthService == nil {
		deps.AuthService = NewAuthService(deps.DB)
	}
	if deps.MaintenanceCoordinator == nil {
		deps.MaintenanceCoordinator = maintenancepkg.NewCoordinator(maintenancepkg.Deps{
			Store: maintenancepkg.SQLiteStore{DB: deps.DB},
			Now:   deps.Now,
		})
	}
	if deps.AuditService == nil {
		deps.AuditService = newAuditServiceWithHealthObservation(deps.DB, deps.NotifyDashboardEvent, func() (*time.Location, string) {
			value := deps.ApplicationTime.Current()
			return value.Location, value.DisplayName
		}, deps.NotificationService, deps.Now, deps.HostHealthObservation, deps.MaintenanceCoordinator)
	}
	if deps.MetricsTokenService == nil {
		deps.MetricsTokenService = NewMetricsTokenService(MetricsTokenDeps{
			DB:     deps.DB,
			DBPath: deps.DBPath,
		})
	}
	if deps.ServerState == nil {
		deps.ServerState = newServerState()
	}
	if deps.GlobalSSHCredential == nil {
		deps.GlobalSSHCredential = serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
			Store:               serverpkg.SQLiteGlobalSSHCredentialStore{DB: deps.DB},
			Encrypt:             encryptSecret,
			Decrypt:             decryptSecret,
			ActiveServerActions: deps.ServerState.ActiveActionNames,
			Logf:                log.Printf,
		})
	}
	if deps.ServerInventoryService == nil {
		deps.ServerInventoryService = newServerInventoryServiceWithHealthObservation(deps.ServerState, deps.DB, deps.DBPath, deps.HostHealthObservation)
	}
	if deps.NewJobManager == nil {
		notify := deps.NotifyDashboardEvent
		deps.NewJobManager = func(db *sql.DB) *JobManager {
			return newJobManagerWithRuntime(db, notify, deps.ServerState, nil)
		}
	}
	if deps.CurrentJobManager == nil {
		var jobMu sync.RWMutex
		manager := deps.JobManager
		setManager := deps.SetCurrentJobManager
		deps.CurrentJobManager = func() *JobManager {
			jobMu.RLock()
			jm := manager
			jobMu.RUnlock()
			if jm != nil {
				return jm
			}
			return currentJobManager()
		}
		deps.SetCurrentJobManager = func(jm *JobManager) {
			jobMu.Lock()
			manager = jm
			jobMu.Unlock()
			if setManager != nil {
				setManager(jm)
				return
			}
			setCurrentJobManager(jm)
		}
	} else if deps.SetCurrentJobManager == nil {
		deps.SetCurrentJobManager = setCurrentJobManager
	}
	if deps.PolicyRepository == nil {
		deps.PolicyRepository = policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{
			DB:          deps.DB,
			NowString:   jobTimestampNow,
			MarshalJSON: marshalJobJSON,
		})
	}
	if deps.StartJobRunner == nil {
		deps.StartJobRunner = func(jobID string, run func()) {
			startJobRunnerWithManager(deps.CurrentJobManager, jobID, run)
		}
	}
	if deps.StartScheduledRunReconciliation == nil {
		deps.StartScheduledRunReconciliation = func(runID int64, jobID string) {
			newScheduledRunLifecycle(deps).watchUpdatePolicyRunForJob(runID, jobID)
		}
	}
	recordAudit := func(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any) {
		if err := deps.AuditService.Record(actor, clientIP, action, targetType, targetName, status, message, meta); err != nil {
			log.Printf("audit write failed: action=%s target=%s err=%v", action, targetName, err)
		}
	}
	factsRepo := deps.HostHealthObservation
	if deps.PolicyService == nil {
		deps.PolicyService = NewPolicyService(PolicyServiceDeps{
			ListPolicies:        deps.PolicyRepository.ListPolicies,
			LoadOverrides:       deps.PolicyRepository.LoadAllOverrides,
			LoadGlobalBlackouts: deps.PolicyRepository.LoadGlobalBlackouts,
			ListRuns:            deps.PolicyRepository.ListRuns,
			CurrentLocation: func() *time.Location {
				return deps.ApplicationTime.Current().Location
			},
			Maintenance:     deps.MaintenanceCoordinator,
			ApplicationTime: deps.ApplicationTime,
			Now:             deps.Now,
			SnapshotServers: func() []Server {
				return deps.ServerState.CloneServers()
			},
			MarkInterruptedRuns: deps.PolicyRepository.MarkInterruptedRuns,
			HandleScheduledRun: func(req policypkg.ScheduledRunRequest) policypkg.ScheduledRunResult {
				return newScheduledRunLifecycle(deps).HandleScheduledRun(req)
			},
		})
	}
	if deps.UpdateService == nil {
		hostMaintenanceSessions := newHostMaintenanceSessionFactory(
			func(server Server) ([]ssh.AuthMethod, error) {
				resolved, err := deps.GlobalSSHCredential.Resolve(context.Background(), server.Key)
				if err != nil {
					return nil, err
				}
				server.Key = resolved.Key
				return serverpkg.BuildAuthMethods(server)
			},
			func() (ssh.HostKeyCallback, error) {
				return serverpkg.HostKeyCallback(appKnownHostsDeps(deps.DBPath))
			},
			func(server Server, config *ssh.ClientConfig) (sshConnection, error) {
				return getDialSSHConnection()(server, config)
			},
		)
		deps.UpdateService = NewUpdateService(UpdateServiceDeps{
			ServerState:             deps.ServerState,
			HostMaintenanceSessions: hostMaintenanceSessions,
			CurrentJobManager:       deps.CurrentJobManager,
			StartJobRunner:          func(jobID string, run func()) { startJobRunnerWithManager(deps.CurrentJobManager, jobID, run) },
			AuditWithActor:          recordAudit,
			SaveServerFacts:         factsRepo.AcceptCollectedFacts,
			UpdateScheduledDiscoveryMeta: func(jobID string, discovery PackageDiscoveryOutcome) {
				newScheduledRunLifecycle(deps).updateScheduledJobDiscoveryMeta(jobID, discovery)
			},
			UpdatePolicyRun: deps.PolicyRepository.UpdateRun,
			LoadScheduledJobBehavior: func(jobID string) scheduledJobBehavior {
				return newScheduledRunLifecycle(deps).loadScheduledJobBehavior(jobID)
			},
		})
	}
	if deps.ObservabilityService == nil {
		policyScheduleDeps := deps.PolicyService.EnsureDeps()
		policyScheduleDeps.ListRuns = deps.PolicyRepository.ListRuns
		policyScheduleDeps.CurrentLocation = func() *time.Location { return deps.ApplicationTime.Current().Location }
		policyScheduleDeps.Now = deps.Now
		policyScheduleDeps.SnapshotServers = func() []Server {
			return deps.ServerState.CloneServers()
		}
		policyScheduleService := policypkg.NewService(policyScheduleDeps)
		deps.ObservabilityService = NewObservabilityService(ObservabilityServiceDeps{
			DB:     deps.DB,
			DBPath: deps.DBPath,
			CurrentTimezone: func() (*time.Location, string) {
				value := deps.ApplicationTime.Current()
				return value.Location, value.DisplayName
			},
			CurrentLocation: func() *time.Location { return deps.ApplicationTime.Current().Location },
			FormatTimestamp: func(raw string, location *time.Location, displayName string) (string, string) {
				return (apptimepkg.Interpretation{
					Location:    location,
					DisplayName: displayName,
				}).Format(raw, jobTimestampLayout)
			},
			ParseAppTimestamp: func(raw string) (time.Time, error) {
				return apptimepkg.ParseInstant(raw, jobTimestampLayout)
			},
			ServerSnapshot: func() ([]Server, map[string]*ServerStatus) {
				deps.ServerState.Lock()
				defer deps.ServerState.Unlock()
				return serverpkg.CloneServers(deps.ServerState.Servers()), serverpkg.CloneStatusMap(deps.ServerState.StatusMap())
			},
			HostHealthObservation: factsRepo,
			ProjectPolicySchedule: policyScheduleService.ProjectSchedule,
		})
	}
	if deps.NewSessionManager == nil {
		deps.NewSessionManager = newSessionManager
	}
	if deps.CurrentSessionManager == nil {
		var sessionMu sync.RWMutex
		session := deps.SessionManager
		setSession := deps.SetSessionManager
		deps.CurrentSessionManager = func() *scs.SessionManager {
			sessionMu.RLock()
			sm := session
			sessionMu.RUnlock()
			if sm != nil {
				return sm
			}
			return currentSessionManager()
		}
		deps.SetSessionManager = func(sm *scs.SessionManager) {
			sessionMu.Lock()
			session = sm
			sessionMu.Unlock()
			if setSession != nil {
				setSession(sm)
				return
			}
			setGlobalSessionManager(sm)
		}
	} else if deps.SetSessionManager == nil {
		deps.SetSessionManager = setGlobalSessionManager
	}
	if deps.LoginRateLimiter == nil {
		deps.LoginRateLimiter = loginRateLimiter
	}
	if deps.PasswordChangeRateLimiter == nil {
		deps.PasswordChangeRateLimiter = passwordChangeRateLimiter
	}
	if deps.SetupRateLimiter == nil {
		deps.SetupRateLimiter = setupRateLimiter
	}
	if deps.MetricsRateLimiter == nil {
		deps.MetricsRateLimiter = metricsRateLimiter
	}
	if deps.AuthSessionCommands == nil {
		deps.AuthSessionCommands = newAuthSessionCommandsWithDeps(authSessionCommandDepsFromAppDeps(deps))
	}
	if deps.TrustedProxies == nil {
		deps.TrustedProxies = trustedProxiesFromEnv
	}
	if deps.BackupService == nil {
		c.deps = deps
		deps.BackupService = NewBackupServiceWithDeps(internalbackup.ServiceDeps{
			DB:                  deps.DB,
			DBPath:              deps.DBPath,
			GlobalSSHCredential: deps.GlobalSSHCredential,
			RestoredRuntime:     c,
		})
	}
	c.deps = deps
	return deps
}
