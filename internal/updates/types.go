package updates

import (
	"io"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
)

var (
	AptUpdateCmd         = RootOrSudoCommand("apt-get update")
	AptUpgradeCmd        = RootOrSudoCommand("apt-get -y upgrade")
	AptFullUpgradeCmd    = RootOrSudoCommand("apt-get -y full-upgrade")
	AptAutoremoveCmd     = RootOrSudoCommand("apt-get -y autoremove")
	AptListUpgradableCmd = "LC_ALL=C apt-get -s upgrade"
	AptListMetadataCmd   = "LC_ALL=C apt list --upgradable 2>/dev/null"
	AptFullUpgradeSimCmd = "LC_ALL=C apt-get -s full-upgrade"
)

const (
	DefaultSSHCommandTimeout = 5 * time.Minute
	MinSSHCommandTimeout     = 1 * time.Second
	MaxSSHCommandTimeout     = 30 * time.Minute

	CVELookupMaxPackages      = 25
	CVELookupMaxPerPackage    = 12
	CVELookupCommandTimeout   = 20 * time.Second
	ApprovalPollInterval      = 200 * time.Millisecond
	PostcheckNameAptHealth    = "post_apt_health"
	PostcheckNameFailedUnits  = "failed_units"
	PostcheckNameRebootNeeded = "reboot_required"
	PostcheckNameCustomCmd    = "custom_command"
	UpdateCompleteAction      = "update.complete"
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

type PrecheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}

type PrecheckSummary struct {
	AllPassed   bool             `json:"all_passed"`
	FailedCheck string           `json:"failed_check,omitempty"`
	Results     []PrecheckResult `json:"results"`
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

type ServerFactsRecord struct {
	ServerName     string `json:"server_name"`
	CollectedAt    string `json:"collected_at"`
	OSPrettyName   string `json:"os_pretty_name"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	DiskStatus     string `json:"disk_status"`
	DiskFreeKB     int64  `json:"disk_free_kb"`
	DiskTotalKB    int64  `json:"disk_total_kb"`
	DiskDetails    string `json:"disk_details"`
	AptStatus      string `json:"apt_status"`
	AptDetails     string `json:"apt_details"`
	RebootRequired *bool  `json:"reboot_required"`
	RawJSON        string `json:"raw_json,omitempty"`
}

type HealthSnapshotRecord struct {
	ID               int64  `json:"id,omitempty"`
	ServerName       string `json:"server_name"`
	CapturedAt       string `json:"captured_at"`
	Source           string `json:"source"`
	PackageCount     int    `json:"package_count"`
	SecurityCount    int    `json:"security_count"`
	LastScanStatus   string `json:"last_scan_status"`
	LastUpdateStatus string `json:"last_update_status"`
	DiskStatus       string `json:"disk_status"`
	DiskFreeKB       int64  `json:"disk_free_kb"`
	DiskTotalKB      int64  `json:"disk_total_kb"`
	AptStatus        string `json:"apt_status"`
	RebootRequired   *bool  `json:"reboot_required"`
	OSPrettyName     string `json:"os_pretty_name"`
	RawJSON          string `json:"raw_json,omitempty"`
}

type MaintenanceKind string

const (
	MaintenanceKindUpdate       MaintenanceKind = "update"
	MaintenanceKindScheduledRun MaintenanceKind = "scheduled_run"
)

// MaintenanceCompletion contains transport-neutral facts from completed maintenance.
type MaintenanceCompletion struct {
	ServerName  string
	CompletedAt string
	Kind        MaintenanceKind
	Status      string
	RawJSON     string
}

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
	CurrentJobManager            func() *jobs.Manager
	StartJobRunner               func(string, func())
	AuditWithActor               func(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any)
	Now                          func() time.Time
	JobTimestampNow              func() string
	LoadCommandTimeout           func() time.Duration
	LoadPostUpdateCheckConfig    func() PostUpdateCheckConfig
	LoadScheduledJobBehavior     func(string) ScheduledJobBehavior
	WaitForApprovalPoll          func()
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
