package main

import (
	"io"
	"time"

	"golang.org/x/crypto/ssh"
)

type updateSSHOperationWithRetryFunc func(Server, *ssh.ClientConfig, *sshConnection, RetryPolicy, string, string, *int, func() error) error

type UpdateServiceDeps struct {
	BuildAuthMethods             func(Server) ([]ssh.AuthMethod, error)
	HostKeyCallback              func() (ssh.HostKeyCallback, error)
	DialSSHWithRetry             func(Server, *ssh.ClientConfig, RetryPolicy, string, *int) (sshConnection, error)
	RunSSHOperationWithRetry     updateSSHOperationWithRetryFunc
	RunSSHCommandWithTimeout     func(sshConnection, string, io.Reader, time.Duration) (string, string, error)
	CurrentJobManager            func() *JobManager
	AuditWithActor               func(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any)
	Now                          func() time.Time
	JobTimestampNow              func() string
	LoadCommandTimeout           func() time.Duration
	LoadPostUpdateCheckConfig    func() PostUpdateCheckConfig
	LoadScheduledJobBehavior     func(string) scheduledJobBehavior
	RunUpdatePrechecks           func(sshConnection) updatePrecheckSummary
	RunPostUpdateHealthChecks    func(sshConnection, PostUpdateCheckConfig, map[string]struct{}) updatePostcheckSummary
	ListFailedSystemdUnits       func(sshConnection) ([]string, string, error)
	CollectServerFacts           func(Server, sshConnection, time.Duration) serverFactsRecord
	SaveServerFacts              func(serverFactsRecord) error
	GetUpgradable                func(sshConnection, time.Duration) ([]PendingUpdate, []string, error)
	QueryPackageCVEs             func(sshConnection, string) ([]string, error)
	UpdateScheduledDiscoveryMeta func(string, []string, []PendingUpdate)
	StartPendingCVEEnrichment    func(Server, *ssh.ClientConfig, []PendingUpdate, string, string, string)
	UpdatePolicyRun              func(int64, updatePolicyRunUpdate) error
}

type UpdateRunRequest struct {
	Server   Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type AutoremoveRunRequest struct {
	Server   Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type SudoersRunRequest struct {
	Server       Server
	SudoPassword string
	Actor        string
	ClientIP     string
	Policy       RetryPolicy
	JobID        string
}

type ScheduledScanRunRequest struct {
	JobID           string
	RunID           int64
	ScheduledForUTC string
	Server          Server
	Policy          UpdatePolicy
	RetryPolicy     RetryPolicy
}

type UpdateService struct {
	deps UpdateServiceDeps
}

func NewUpdateService(deps UpdateServiceDeps) *UpdateService {
	deps = deps.withDefaults()
	return &UpdateService{deps: deps}
}

func defaultUpdateService() *UpdateService {
	return NewUpdateService(UpdateServiceDeps{})
}

func (s *UpdateService) ensureDeps() UpdateServiceDeps {
	if s == nil {
		return UpdateServiceDeps{}.withDefaults()
	}
	return s.deps.withDefaults()
}

func (d UpdateServiceDeps) withDefaults() UpdateServiceDeps {
	if d.BuildAuthMethods == nil {
		d.BuildAuthMethods = buildAuthMethods
	}
	if d.HostKeyCallback == nil {
		d.HostKeyCallback = getHostKeyCallback
	}
	if d.DialSSHWithRetry == nil {
		d.DialSSHWithRetry = dialSSHWithRetry
	}
	if d.RunSSHOperationWithRetry == nil {
		d.RunSSHOperationWithRetry = runSSHOperationWithRetry
	}
	if d.RunSSHCommandWithTimeout == nil {
		d.RunSSHCommandWithTimeout = runSSHCommandWithTimeout
	}
	if d.CurrentJobManager == nil {
		d.CurrentJobManager = currentJobManager
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
		d.LoadScheduledJobBehavior = loadScheduledJobBehavior
	}
	if d.RunUpdatePrechecks == nil {
		d.RunUpdatePrechecks = runUpdatePrechecks
	}
	if d.RunPostUpdateHealthChecks == nil {
		d.RunPostUpdateHealthChecks = runPostUpdateHealthChecks
	}
	if d.ListFailedSystemdUnits == nil {
		d.ListFailedSystemdUnits = listFailedSystemdUnits
	}
	if d.CollectServerFacts == nil {
		d.CollectServerFacts = collectServerFactsWithConnection
	}
	if d.SaveServerFacts == nil {
		d.SaveServerFacts = saveServerFacts
	}
	if d.GetUpgradable == nil {
		d.GetUpgradable = getUpgradable
	}
	if d.QueryPackageCVEs == nil {
		d.QueryPackageCVEs = queryPackageCVEs
	}
	if d.UpdateScheduledDiscoveryMeta == nil {
		d.UpdateScheduledDiscoveryMeta = updateScheduledJobDiscoveryMeta
	}
	if d.StartPendingCVEEnrichment == nil {
		d.StartPendingCVEEnrichment = startPendingUpdateCVEEnrichment
	}
	if d.UpdatePolicyRun == nil {
		d.UpdatePolicyRun = updateUpdatePolicyRun
	}
	return d
}
