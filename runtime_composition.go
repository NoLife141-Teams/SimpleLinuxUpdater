package main

import (
	"context"
	"database/sql"
	"log"
	"sync"
	"time"

	internalbackup "debian-updater/internal/backup"
	"debian-updater/internal/events"
	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/alexedwards/scs/v2"
	"golang.org/x/crypto/ssh"
)

type runtimeComposition struct {
	deps AppDeps
}

func newRuntimeComposition(overrides AppDeps) *runtimeComposition {
	return &runtimeComposition{deps: overrides}
}

func (c *runtimeComposition) Compose() AppDeps {
	deps := c.deps
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.DBPath == nil {
		deps.DBPath = dbPath
	}
	if deps.CurrentAppTimezone == nil {
		deps.CurrentAppTimezone = currentAppTimezone
	}
	if deps.CurrentAppLocation == nil {
		deps.CurrentAppLocation = currentAppLocation
	}
	if deps.AppTimezoneDisplayName == nil {
		deps.AppTimezoneDisplayName = currentAppTimezoneDisplayName
	}
	if deps.AppTimezoneResolvedName == nil {
		deps.AppTimezoneResolvedName = currentAppTimezoneResolvedName
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
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
	if deps.AuditService == nil {
		deps.AuditService = newAuditServiceWithNotificationsAndClock(deps.DB, deps.NotifyDashboardEvent, deps.CurrentAppTimezone, deps.NotificationService, deps.Now)
	}
	if deps.BackupBarrier == nil {
		deps.BackupBarrier = backupRestoreMu
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
		deps.ServerInventoryService = newServerInventoryServiceWithStateDBPath(deps.ServerState, deps.DB, deps.DBPath)
	}
	if deps.NewJobManager == nil {
		notify := deps.NotifyDashboardEvent
		deps.NewJobManager = func(db *sql.DB) *JobManager {
			return newJobManagerWithRuntime(db, notify, deps.ServerState, deps.CurrentMaintenanceActive)
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
	if deps.CurrentMaintenanceActive == nil {
		deps.CurrentMaintenanceActive = func() bool {
			return currentMaintenanceState().Active
		}
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
	factsRepo := updatespkg.SQLiteServerFactsRepository{DB: deps.DB, Now: deps.Now}
	if deps.PolicyService == nil {
		deps.PolicyService = NewPolicyService(PolicyServiceDeps{
			ListPolicies:             deps.PolicyRepository.ListPolicies,
			LoadOverrides:            deps.PolicyRepository.LoadAllOverrides,
			LoadGlobalBlackouts:      deps.PolicyRepository.LoadGlobalBlackouts,
			ListRuns:                 deps.PolicyRepository.ListRuns,
			CurrentLocation:          deps.CurrentAppLocation,
			CurrentMaintenanceActive: deps.CurrentMaintenanceActive,
			Now:                      deps.Now,
			TryBackupRestoreReadLock: deps.BackupBarrier.TryRLock,
			UnlockBackupRestoreRead:  deps.BackupBarrier.RUnlock,
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
			SaveServerFacts:         factsRepo.Save,
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
		policyScheduleDeps.CurrentLocation = deps.CurrentAppLocation
		policyScheduleDeps.Now = deps.Now
		policyScheduleDeps.SnapshotServers = func() []Server {
			return deps.ServerState.CloneServers()
		}
		policyScheduleService := policypkg.NewService(policyScheduleDeps)
		deps.ObservabilityService = NewObservabilityService(ObservabilityServiceDeps{
			DB:              deps.DB,
			DBPath:          deps.DBPath,
			CurrentTimezone: deps.CurrentAppTimezone,
			CurrentLocation: deps.CurrentAppLocation,
			ServerSnapshot: func() ([]Server, map[string]*ServerStatus) {
				deps.ServerState.Lock()
				defer deps.ServerState.Unlock()
				return serverpkg.CloneServers(deps.ServerState.Servers()), serverpkg.CloneStatusMap(deps.ServerState.StatusMap())
			},
			LoadServerFacts:             factsRepo.LoadAll,
			ListHealthSnapshots:         factsRepo.ListHealthSnapshots,
			HealthSnapshotRetentionDays: factsRepo.HealthSnapshotRetentionDays,
			ProjectPolicySchedule:       policyScheduleService.ProjectSchedule,
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
	if deps.InitializeMaintenanceState == nil {
		deps.InitializeMaintenanceState = initializeMaintenanceState
	}
	if deps.BackupService == nil {
		runtimeDeps := deps
		metricsTokenService := deps.MetricsTokenService
		deps.BackupService = NewBackupServiceWithDeps(internalbackup.ServiceDeps{
			DB:                  deps.DB,
			DBPath:              deps.DBPath,
			GlobalSSHCredential: deps.GlobalSSHCredential,
			ResetRuntimeCaches: func() {
				resetRuntimeCaches()
				if runtimeDeps.GlobalSSHCredential != nil {
					runtimeDeps.GlobalSSHCredential.ResetCache()
				}
				if metricsTokenService != nil {
					metricsTokenService.RestoreCache("", false, "")
				}
			},
			ReloadRuntimeState: func() error {
				return reloadAppRuntimeState(runtimeDeps)
			},
		})
	}
	return deps
}
