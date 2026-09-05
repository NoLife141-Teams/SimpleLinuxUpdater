package main

import (
	"context"
	"log"
	"net/http"
	"sync"
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
type AptRepairRunRequest = updatespkg.AptRepairRunRequest
type RebootRunRequest = updatespkg.RebootRunRequest
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
type VulnerabilityScanner = updatespkg.VulnerabilityScanner
type VulnerabilityScannerFunc = updatespkg.VulnerabilityScannerFunc

const HostMaintenanceStageAuth = updatespkg.HostMaintenanceStageAuth

var (
	defaultVulnerabilityScannerMu sync.Mutex
	defaultVulnerabilityScanner   VulnerabilityScanner

	applicationMaintenanceContextMu sync.RWMutex
	applicationMaintenanceCtx       context.Context = context.Background()
)

func currentApplicationMaintenanceContext() context.Context {
	applicationMaintenanceContextMu.RLock()
	ctx := applicationMaintenanceCtx
	applicationMaintenanceContextMu.RUnlock()
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func setApplicationMaintenanceContext(ctx context.Context) func() {
	if ctx == nil {
		ctx = context.Background()
	}
	applicationMaintenanceContextMu.Lock()
	previous := applicationMaintenanceCtx
	applicationMaintenanceCtx = ctx
	applicationMaintenanceContextMu.Unlock()
	return func() {
		applicationMaintenanceContextMu.Lock()
		applicationMaintenanceCtx = previous
		applicationMaintenanceContextMu.Unlock()
	}
}

func applicationVulnerabilityScanner() VulnerabilityScanner {
	defaultVulnerabilityScannerMu.Lock()
	defer defaultVulnerabilityScannerMu.Unlock()
	if defaultVulnerabilityScanner == nil {
		defaultVulnerabilityScanner = updatespkg.NewOSVVulnerabilityScanner(updatespkg.OSVVulnerabilityScannerDeps{
			HTTPClient: &http.Client{Timeout: 30 * time.Second},
		})
	}
	return defaultVulnerabilityScanner
}

func replaceApplicationVulnerabilityScanner(scanner VulnerabilityScanner) func() {
	defaultVulnerabilityScannerMu.Lock()
	previous := defaultVulnerabilityScanner
	defaultVulnerabilityScanner = scanner
	defaultVulnerabilityScannerMu.Unlock()
	return func() {
		defaultVulnerabilityScannerMu.Lock()
		defaultVulnerabilityScanner = previous
		defaultVulnerabilityScannerMu.Unlock()
	}
}

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
			resolved, err := credential.Resolve(currentApplicationMaintenanceContext(), server.Key)
			if err != nil {
				return nil, err
			}
			server.Key = resolved.Key
			return serverpkg.BuildAuthMethods(server)
		}, getHostKeyCallback, getDialSSHConnection())
	}
	if d.VulnerabilityScanner == nil {
		d.VulnerabilityScanner = applicationVulnerabilityScanner()
	}
	if d.CurrentJobManager == nil {
		d.CurrentJobManager = currentJobManager
	}
	if d.StartJobRunner == nil {
		d.StartJobRunner = func(jobID string, run func()) {
			startJobRunner(jobID, run)
		}
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
	inner := updatespkg.NewProductionHostMaintenanceSessionFactory(updatespkg.ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods:    buildAuth,
		HostKeyCallback:     hostKeyCallback,
		DialSSH:             dial,
		RunCommand:          runSSHCommandWithContext,
		RunStreamingCommand: runSSHCommandWithContextStreaming,
		SSHConnectTimeout:   sshConnectTimeout,
		Logf:                log.Printf,
	})
	return newLifecycleHostMaintenanceSessionFactory(currentApplicationMaintenanceContext(), inner)
}

func updateServiceEnsureDeps(service *UpdateService) UpdateServiceDeps {
	if service == nil {
		return updateServiceDepsWithDefaults(UpdateServiceDeps{})
	}
	return updateServiceDepsWithDefaults(service.EnsureDeps())
}
