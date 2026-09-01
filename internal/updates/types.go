package updates

import (
	"fmt"
	"io"
	"time"

	"debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
)

var (
	AptUpdateCmd        = nonInteractiveAptCommand("update", "update")
	AptUpgradeCmd       = nonInteractiveAptCommand("upgrade", "-y upgrade")
	AptFullUpgradeCmd   = nonInteractiveAptCommand("full-upgrade", "-y full-upgrade")
	AptAutoremoveCmd    = nonInteractiveAptCommand("autoremove", "-y autoremove")
	ControlledRebootCmd = "nohup sh -c 'sleep 1; if [ \"$(id -u)\" -eq 0 ]; then /usr/bin/systemctl reboot; else sudo -n " + RootHelperPath + " reboot; fi' >/dev/null 2>&1 &"
	// AptLockProbeCmd remains the exact pre-helper fallback used only after the
	// typed extended probe is denied by an older sudoers policy.
	AptLockProbeCmd         = `if [ "$(id -u)" -eq 0 ]; then /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock; else sudo -n /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock; fi`
	AptExtendedLockProbeCmd = RootOrSudoHelperCommand("/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock", "lock-probe-extended")
	AptRepairCmd            = RootOrSudoHelperCommand(rootAptRepairCommand(), "repair")
	AptListUpgradableCmd    = ReadOnlyAptCommand("apt-get -s upgrade")
	AptListMetadataCmd      = ReadOnlyAptCommand("apt list --upgradable") + " 2>/dev/null"
	AptFullUpgradeSimCmd    = ReadOnlyAptCommand("apt-get -o Debug::NoLocking=1 --print-uris --yes --download-only full-upgrade")
)

func rootAptRepairCommand() string {
	lockProbe := "/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock"
	dpkgConfigure := fmt.Sprintf("/usr/bin/env %s /usr/bin/dpkg --force-confdef --force-confold --configure -a", AptNonInteractiveEnvironment)
	aptRepair := fmt.Sprintf("/usr/bin/env %s /usr/bin/apt-get %s -y -f install", AptNonInteractiveEnvironment, AptDpkgConffileOptions)
	return buildAptRepairLockGuard(lockProbe) + "; " + dpkgConfigure + " && " + aptRepair + " && " +
		buildAptRepairAuditGuard("/usr/bin/dpkg --audit") + " && /usr/bin/apt-get check"
}

func buildAptRepairLockGuard(lockProbeCmd string) string {
	return "apt_lock_probe_output=$(" + lockProbeCmd + " 2>&1); apt_lock_probe_status=$?; " +
		"if [ \"$apt_lock_probe_status\" -eq 0 ]; then " +
		"printf '%s\\n' 'APT/DPKG repair blocked: package-manager lock is active.' \"$apt_lock_probe_output\" >&2; exit 75; fi; " +
		"if [ \"$apt_lock_probe_status\" -ne 1 ] || [ -n \"$apt_lock_probe_output\" ]; then " +
		"printf '%s\\n' 'APT/DPKG repair blocked: package-manager lock probe failed.' \"$apt_lock_probe_output\" >&2; exit 76; fi"
}

func buildAptRepairAuditGuard(auditCmd string) string {
	return "{ dpkg_audit_output=$(" + auditCmd + " 2>&1); dpkg_audit_status=$?; " +
		"if [ \"$dpkg_audit_status\" -ne 0 ]; then " +
		"printf '%s\\n' 'APT/DPKG repair verification failed: dpkg audit command failed.' \"$dpkg_audit_output\" >&2; exit \"$dpkg_audit_status\"; fi; " +
		"if [ -n \"$dpkg_audit_output\" ]; then " +
		"printf '%s\\n' 'APT/DPKG repair verification failed: dpkg audit still reports package-state problems.' \"$dpkg_audit_output\" >&2; exit 77; fi; }"
}

const (
	AptNonInteractiveEnvironment  = "DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1"
	AptDpkgConffileOptions        = "-o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold"
	AptReadOnlyEnvironment        = "LC_ALL=C DEBIAN_FRONTEND=noninteractive APT_LISTCHANGES_FRONTEND=none"
	AptInteractionStrategySummary = "APT strategy: noninteractive frontend, changelogs disabled, automatic needrestart handling, existing config files kept on conflicts"

	DefaultSSHCommandTimeout = 5 * time.Minute
	MinSSHCommandTimeout     = 1 * time.Second
	MaxSSHCommandTimeout     = 30 * time.Minute

	CVELookupMaxPerPackage      = 12
	CVELookupCommandTimeout     = 20 * time.Second
	ApprovalPollInterval        = 200 * time.Millisecond
	RebootVerificationInterval  = 5 * time.Second
	RebootVerificationAttempts  = 24
	PlanDiskBaseReserveKB       = int64(1024 * 1024)
	PlanDiskPerPackageKB        = int64(64 * 1024)
	PlanDiskPerNewPackageKB     = int64(512 * 1024)
	PlanDiskExactMarginMinBytes = int64(64 * 1024 * 1024)
	PlanDiskExactMarginMaxBytes = int64(512 * 1024 * 1024)
	PlanDiskSourceExact         = "exact"
	PlanDiskSourceEstimate      = "estimate"
	PostcheckNameAptHealth      = "post_apt_health"
	PostcheckNameFailedUnits    = "failed_units"
	PostcheckNameRebootNeeded   = "reboot_required"
	PostcheckNameCustomCmd      = "custom_command"
	UpdateCompleteAction        = "update.complete"
)

type RetryPolicy struct {
	MaxAttempts int
	BaseDelay   time.Duration
	MaxDelay    time.Duration
	JitterPct   int
}

type PostUpdateCheckConfig struct {
	Enabled               bool
	BlockOnAptHealth      bool
	BlockOnFailedUnits    bool
	RebootRequiredWarning bool
	CustomCommand         string
}

type PrecheckResult = health.CheckResult

type PrecheckSummary struct {
	AllPassed   bool             `json:"all_passed"`
	FailedCheck string           `json:"failed_check,omitempty"`
	Results     []PrecheckResult `json:"results"`
}

type PlanDiskSpaceEstimate struct {
	RequiredKB              int64
	PackageCount            int
	NewPackageCount         int
	Source                  string
	ArchiveBytes            int64
	InstalledDeltaBytes     int64
	InstalledGrowthBytes    int64
	SafetyMarginBytes       int64
	OperationalReserveBytes int64
}

type PostcheckSummary struct {
	AllPassed   bool             `json:"all_passed"`
	FailedCheck string           `json:"failed_check,omitempty"`
	Warnings    int              `json:"warnings"`
	Results     []PrecheckResult `json:"results"`
}

type RetryableTaggedError struct {
	Err error
}

func (e RetryableTaggedError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e RetryableTaggedError) Unwrap() error {
	return e.Err
}

func (e RetryableTaggedError) Retryable() bool {
	return true
}

type NonRetryableTaggedError struct {
	Err                    error
	ReconciliationRequired bool
}

func (e NonRetryableTaggedError) Error() string {
	if e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e NonRetryableTaggedError) Unwrap() error {
	return e.Err
}

func (e NonRetryableTaggedError) Retryable() bool {
	return false
}

func (e NonRetryableTaggedError) Transient() bool {
	return true
}

func (e NonRetryableTaggedError) RequiresReconciliation() bool {
	return e.ReconciliationRequired
}

type SSHSessionRunner interface {
	SetStdin(io.Reader)
	SetStdout(io.Writer)
	SetStderr(io.Writer)
	Run(string) error
	Close() error
}

type SSHConnection interface {
	NewSession() (SSHSessionRunner, error)
	Close() error
}

type ServerFactsRecord = health.CollectedFacts
type HealthSnapshotRecord = health.Snapshot
type MaintenanceKind = health.MaintenanceKind

const (
	MaintenanceKindUpdate       = health.MaintenanceKindUpdate
	MaintenanceKindScheduledRun = health.MaintenanceKindScheduledRun
)

// MaintenanceCompletion contains transport-neutral facts from completed maintenance.
type MaintenanceCompletion = health.MaintenanceOutcome

type ScheduledJobBehavior struct {
	ApprovalTimeout  time.Duration
	AutoApproveScope string
}

type ScheduledJobDiscovery = PackageDiscoveryOutcome

type ScheduledJobMeta struct {
	Trigger                string                 `json:"trigger,omitempty"`
	PolicyID               int64                  `json:"policy_id,omitempty"`
	PolicyName             string                 `json:"policy_name,omitempty"`
	ScheduledFor           string                 `json:"scheduled_for,omitempty"`
	ExecutionMode          string                 `json:"execution_mode,omitempty"`
	PackageScope           string                 `json:"package_scope,omitempty"`
	UpgradeMode            string                 `json:"upgrade_mode,omitempty"`
	ApprovalTimeoutMinutes int                    `json:"approval_timeout_minutes,omitempty"`
	AutoApproveScope       string                 `json:"auto_approve_scope,omitempty"`
	Discovery              *ScheduledJobDiscovery `json:"discovery,omitempty"`
	Error                  string                 `json:"error,omitempty"`
}

type ServiceDeps struct {
	ServerState                  *servers.State
	HostMaintenanceSessions      HostMaintenanceSessionFactory
	VulnerabilityScanner         VulnerabilityScanner
	CurrentJobManager            func() *jobs.Manager
	StartJobRunner               func(string, func())
	AuditWithActor               func(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any)
	Now                          func() time.Time
	JobTimestampNow              func() string
	LoadCommandTimeout           func() time.Duration
	LoadPostUpdateCheckConfig    func() PostUpdateCheckConfig
	LoadScheduledJobBehavior     func(string) ScheduledJobBehavior
	WaitForApprovalPoll          func()
	Sleep                        func(time.Duration)
	SaveServerFacts              func(ServerFactsRecord) error
	UpdateScheduledDiscoveryMeta func(string, PackageDiscoveryOutcome)
	UpdatePolicyRun              func(int64, policies.RunUpdate) error
	IsPostcheckFailureBlocking   func(string, PostUpdateCheckConfig) bool
	SummarizeUnitNames           func([]string, int) string
	Logf                         func(string, ...any)
}

type UpdateRunRequest struct {
	Server   servers.Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type AutoremoveRunRequest struct {
	Server   servers.Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type AptRepairRunRequest struct {
	Server   servers.Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type RebootRunRequest struct {
	Server   servers.Server
	Actor    string
	ClientIP string
	Policy   RetryPolicy
	JobID    string
}

type SudoersRunRequest struct {
	Server       servers.Server
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
	Server          servers.Server
	Policy          policies.Policy
	RetryPolicy     RetryPolicy
}
