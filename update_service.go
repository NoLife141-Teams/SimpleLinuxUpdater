package main

import (
	"context"
	"log"
	"time"

	healthpkg "debian-updater/internal/health"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"golang.org/x/crypto/ssh"
)

type UpdateServiceDeps = updatespkg.ServiceDeps
type UpdateService = updatespkg.Service
type UpdateRunRequest = updatespkg.UpdateRunRequest
type AutoremoveRunRequest = updatespkg.AutoremoveRunRequest
type SudoersRunRequest = updatespkg.SudoersRunRequest
type ScheduledScanRunRequest = updatespkg.ScheduledScanRunRequest
type PackageDiscoveryOutcome = updatespkg.PackageDiscoveryOutcome
type scheduledJobBehavior = updatespkg.ScheduledJobBehavior
type scheduledJobDiscovery = updatespkg.ScheduledJobDiscovery
type scheduledJobMeta = updatespkg.ScheduledJobMeta
type HostMaintenanceSession = updatespkg.HostMaintenanceSession
type HostMaintenanceSessionFactory = updatespkg.HostMaintenanceSessionFactory
type HostMaintenanceSessionFactoryFunc = updatespkg.HostMaintenanceSessionFactoryFunc
type HostMaintenanceSessionRequest = updatespkg.HostMaintenanceSessionRequest
type HostMaintenanceSessionFuncs = updatespkg.HostMaintenanceSessionFuncs
type HostCommandRequest = updatespkg.HostCommandRequest
type HostCommandResult = updatespkg.HostCommandResult
type HostOperationRequest = updatespkg.HostOperationRequest
type HostPackageDiscoveryResult = updatespkg.HostPackageDiscoveryResult
type HostMaintenanceError = updatespkg.HostMaintenanceError

const HostMaintenanceStageAuth = updatespkg.HostMaintenanceStageAuth

func NewUpdateService(deps UpdateServiceDeps) *UpdateService {
	return updatespkg.NewService(updateServiceDepsWithDefaults(deps))
}

func defaultUpdateService() *UpdateService {
	return NewUpdateService(UpdateServiceDeps{})
}

func updateServiceDepsWithDefaults(d UpdateServiceDeps) UpdateServiceDeps {
	if d.ServerState == nil {
		d.ServerState = globalServerState()
	}
	if d.HostMaintenanceSessions == nil {
		credential := serverpkg.NewGlobalSSHCredential(serverpkg.GlobalSSHCredentialDeps{
			Store:               serverpkg.SQLiteGlobalSSHCredentialStore{DB: getDB},
			Encrypt:             encryptSecret,
			Decrypt:             decryptSecret,
			ActiveServerActions: globalServerState().ActiveActionNames,
			Logf:                log.Printf,
		})
		d.HostMaintenanceSessions = newHostMaintenanceSessionFactory(func(server Server) ([]ssh.AuthMethod, error) {
			resolved, err := credential.Resolve(context.Background(), server.Key)
			if err != nil {
				return nil, err
			}
			server.Key = resolved.Key
			return serverpkg.BuildAuthMethods(server)
		}, getHostKeyCallback, getDialSSHConnection())
	}
	if d.CurrentJobManager == nil {
		d.CurrentJobManager = currentJobManager
	}
	if d.StartJobRunner == nil {
		d.StartJobRunner = startJobRunner
	}
	if d.AuditWithActor == nil {
		d.AuditWithActor = auditWithActor
	}
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.JobTimestampNow == nil {
		d.JobTimestampNow = jobTimestampNow
	}
	if d.LoadCommandTimeout == nil {
		d.LoadCommandTimeout = loadSSHCommandTimeoutFromEnv
	}
	if d.LoadPostUpdateCheckConfig == nil {
		d.LoadPostUpdateCheckConfig = loadPostUpdateCheckConfigFromEnv
	}
	if d.LoadScheduledJobBehavior == nil {
		d.LoadScheduledJobBehavior = func(jobID string) scheduledJobBehavior {
			return defaultScheduledRunLifecycle().LoadJobBehavior(jobID)
		}
	}
	if d.SaveServerFacts == nil {
		d.SaveServerFacts = (healthpkg.SQLiteObservation{DB: getDB}).AcceptCollectedFacts
	}
	if d.UpdateScheduledDiscoveryMeta == nil {
		d.UpdateScheduledDiscoveryMeta = func(jobID string, discovery PackageDiscoveryOutcome) {
			defaultScheduledRunLifecycle().UpdateJobDiscovery(jobID, discovery)
		}
	}
	if d.UpdatePolicyRun == nil {
		d.UpdatePolicyRun = updateUpdatePolicyRun
	}
	if d.IsPostcheckFailureBlocking == nil {
		d.IsPostcheckFailureBlocking = updatespkg.IsPostcheckFailureBlocking
	}
	if d.SummarizeUnitNames == nil {
		d.SummarizeUnitNames = updatespkg.SummarizeUnitNames
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	return d
}

func newHostMaintenanceSessionFactory(
	buildAuth func(serverpkg.Server) ([]ssh.AuthMethod, error),
	hostKeyCallback func() (ssh.HostKeyCallback, error),
	dial func(serverpkg.Server, *ssh.ClientConfig) (sshConnection, error),
) HostMaintenanceSessionFactory {
	return updatespkg.NewProductionHostMaintenanceSessionFactory(updatespkg.ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods:  buildAuth,
		HostKeyCallback:   hostKeyCallback,
		DialSSH:           dial,
		RunCommand:        runSSHCommandWithContext,
		SSHConnectTimeout: sshConnectTimeout,
		Logf:              log.Printf,
	})
}

func updateServiceEnsureDeps(service *UpdateService) UpdateServiceDeps {
	if service == nil {
		return updateServiceDepsWithDefaults(UpdateServiceDeps{})
	}
	return updateServiceDepsWithDefaults(service.EnsureDeps())
}
