package observability

import (
	"database/sql"
	"errors"
	"time"

	"debian-updater/internal/health"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

const (
	DefaultMetricsCacheTTL        = 45 * time.Second
	DefaultMetricsTokenSettingKey = "metrics_bearer_token_hash"
	DefaultMetricsTokenEntropy    = 32
)

var ErrInvalidWindow = errors.New("invalid observability window")

type FailureItem struct {
	Cause string `json:"cause"`
	Count int    `json:"count"`
}

type StatusItem struct {
	Status string `json:"status"`
	Count  int    `json:"count"`
}

type SummaryResponse struct {
	Window      string `json:"window"`
	From        string `json:"from"`
	FromDisplay string `json:"from_display,omitempty"`
	To          string `json:"to"`
	ToDisplay   string `json:"to_display,omitempty"`
	Totals      struct {
		UpdatesTotal   int     `json:"updates_total"`
		UpdatesSuccess int     `json:"updates_success"`
		UpdatesFailure int     `json:"updates_failure"`
		SuccessRatePct float64 `json:"success_rate_pct"`
	} `json:"totals"`
	Duration struct {
		AvgMS                  float64 `json:"avg_ms"`
		SamplesWithDuration    int     `json:"samples_with_duration"`
		SamplesWithoutDuration int     `json:"samples_without_duration"`
	} `json:"duration"`
	FailureCauses   []FailureItem `json:"failure_causes"`
	StatusBreakdown []StatusItem  `json:"status_breakdown"`
}

type DashboardUpdateHistory struct {
	Status            string  `json:"status"`
	FinishedAt        string  `json:"finished_at"`
	FinishedAtDisplay string  `json:"finished_at_display,omitempty"`
	DurationMS        float64 `json:"duration_ms"`
	Message           string  `json:"message"`
	FailureCause      string  `json:"failure_cause,omitempty"`
}

type DashboardCommandHistoryItem struct {
	CreatedAt        string `json:"created_at"`
	CreatedAtDisplay string `json:"created_at_display,omitempty"`
	Action           string `json:"action"`
	Status           string `json:"status"`
	Message          string `json:"message"`
	Actor            string `json:"actor"`
}

type DashboardScheduleInfo struct {
	State               string `json:"state"`
	PolicyName          string `json:"policy_name,omitempty"`
	ScheduledForUTC     string `json:"scheduled_for_utc,omitempty"`
	ScheduledForDisplay string `json:"scheduled_for_display,omitempty"`
	Status              string `json:"status,omitempty"`
	Reason              string `json:"reason,omitempty"`
	Summary             string `json:"summary,omitempty"`
}

type DashboardNoRunInfo struct {
	Active   bool   `json:"active"`
	Scope    string `json:"scope,omitempty"`
	Summary  string `json:"summary"`
	Timezone string `json:"timezone"`
}

type DashboardHealthInfo struct {
	RebootRequired *bool  `json:"reboot_required"`
	DiskStatus     string `json:"disk_status"`
	DiskFreeKB     int64  `json:"disk_free_kb"`
	DiskTotalKB    int64  `json:"disk_total_kb"`
	DiskDetails    string `json:"disk_details"`
	AptStatus      string `json:"apt_status"`
	AptDetails     string `json:"apt_details"`
	OSPrettyName   string `json:"os_pretty_name"`
	UptimeSeconds  int64  `json:"uptime_seconds"`
	CollectedAt    string `json:"collected_at"`
	Source         string `json:"source"`
}

type DashboardRiskInfo struct {
	Level           string   `json:"level"`
	Summary         string   `json:"summary"`
	PendingPackages int      `json:"pending_packages"`
	SecurityUpdates int      `json:"security_updates"`
	CVEs            []string `json:"cves"`
}

type DashboardTimelinePhase struct {
	Key              string `json:"key"`
	Label            string `json:"label"`
	State            string `json:"state"`
	ProgressPct      int    `json:"progress_pct"`
	Summary          string `json:"summary,omitempty"`
	UpdatedAt        string `json:"updated_at,omitempty"`
	UpdatedAtDisplay string `json:"updated_at_display,omitempty"`
}

type DashboardTimelineInfo struct {
	CurrentPhase     string                   `json:"current_phase"`
	CurrentLabel     string                   `json:"current_label"`
	State            string                   `json:"state"`
	ProgressPct      int                      `json:"progress_pct"`
	Summary          string                   `json:"summary"`
	StartedAt        string                   `json:"started_at,omitempty"`
	StartedAtDisplay string                   `json:"started_at_display,omitempty"`
	UpdatedAt        string                   `json:"updated_at,omitempty"`
	UpdatedAtDisplay string                   `json:"updated_at_display,omitempty"`
	Phases           []DashboardTimelinePhase `json:"phases"`
}

type DashboardActionInfo struct {
	Enabled        bool           `json:"enabled"`
	Reason         string         `json:"reason"`
	Readiness      string         `json:"readiness"`
	BlockingStatus string         `json:"blocking_status,omitempty"`
	Counts         map[string]int `json:"counts,omitempty"`
}

type DashboardApprovalTriageInfo struct {
	Eligible                   bool   `json:"eligible"`
	PendingPackages            int    `json:"pending_packages"`
	SecurityUpdates            int    `json:"security_updates"`
	StandardPackages           int    `json:"standard_packages"`
	KeptBackPackages           int    `json:"kept_back_packages"`
	StandardSecurityUpdates    int    `json:"standard_security_updates"`
	KeptBackSecurityUpdates    int    `json:"kept_back_security_updates"`
	CVECount                   int    `json:"cve_count"`
	RiskLevel                  string `json:"risk_level"`
	RiskLabel                  string `json:"risk_label"`
	RiskOrder                  int    `json:"risk_order"`
	FactsState                 string `json:"facts_state"`
	FactsCollectedAt           string `json:"facts_collected_at,omitempty"`
	FactsCollectedAtDisplay    string `json:"facts_collected_at_display,omitempty"`
	LastCheckAt                string `json:"last_check_at,omitempty"`
	LastCheckDisplay           string `json:"last_check_display,omitempty"`
	CanApproveAll              bool   `json:"can_approve_all"`
	CanApproveSecurity         bool   `json:"can_approve_security"`
	CanApproveKeptBackSecurity bool   `json:"can_approve_kept_back_security"`
	CanApproveFull             bool   `json:"can_approve_full"`
	CanCancel                  bool   `json:"can_cancel"`
	CanRefreshFacts            bool   `json:"can_refresh_facts"`
	CanRunChecks               bool   `json:"can_run_checks"`
}

type DashboardServerSummary struct {
	Name             string                         `json:"name"`
	LastUpdate       *DashboardUpdateHistory        `json:"last_update,omitempty"`
	LastFailedUpdate *DashboardUpdateHistory        `json:"last_failed_update,omitempty"`
	AvgDurationMS    float64                        `json:"avg_duration_ms"`
	DurationSamples  int                            `json:"duration_samples"`
	NextRun          DashboardScheduleInfo          `json:"next_run"`
	NoRun            DashboardNoRunInfo             `json:"no_run"`
	Health           DashboardHealthInfo            `json:"health"`
	Risk             DashboardRiskInfo              `json:"risk"`
	Timeline         DashboardTimelineInfo          `json:"timeline"`
	Actions          map[string]DashboardActionInfo `json:"actions"`
	ApprovalTriage   DashboardApprovalTriageInfo    `json:"approval_triage"`
	CommandHistory   []DashboardCommandHistoryItem  `json:"command_history"`
}

type DashboardSummaryResponse struct {
	Window      string                   `json:"window"`
	From        string                   `json:"from"`
	To          string                   `json:"to"`
	GeneratedAt string                   `json:"generated_at"`
	Fleet       map[string]any           `json:"fleet"`
	Servers     []DashboardServerSummary `json:"servers"`
}

type HealthTrendPoint struct {
	CapturedAt        string `json:"captured_at"`
	CapturedAtDisplay string `json:"captured_at_display,omitempty"`
	Source            string `json:"source"`
	PackageCount      int    `json:"package_count"`
	SecurityCount     int    `json:"security_count"`
	LastScanStatus    string `json:"last_scan_status,omitempty"`
	LastUpdateStatus  string `json:"last_update_status,omitempty"`
	DiskStatus        string `json:"disk_status"`
	DiskFreeKB        int64  `json:"disk_free_kb"`
	DiskTotalKB       int64  `json:"disk_total_kb"`
	AptStatus         string `json:"apt_status"`
	RebootRequired    *bool  `json:"reboot_required"`
	OSPrettyName      string `json:"os_pretty_name,omitempty"`
}

type HealthTrendServerSummary struct {
	Name               string             `json:"name"`
	Samples            int                `json:"samples"`
	Latest             *HealthTrendPoint  `json:"latest,omitempty"`
	First              *HealthTrendPoint  `json:"first,omitempty"`
	PackageDelta       int                `json:"package_delta"`
	SecurityDelta      int                `json:"security_delta"`
	DiskFreeDeltaKB    int64              `json:"disk_free_delta_kb"`
	UpdateFailures     int                `json:"update_failures"`
	ScanFailures       int                `json:"scan_failures"`
	AptProblemSamples  int                `json:"apt_problem_samples"`
	DiskProblemSamples int                `json:"disk_problem_samples"`
	RebootSeen         bool               `json:"reboot_seen"`
	Points             []HealthTrendPoint `json:"points"`
}

type HealthTrendResponse struct {
	Window        string                     `json:"window"`
	From          string                     `json:"from"`
	FromDisplay   string                     `json:"from_display,omitempty"`
	To            string                     `json:"to"`
	ToDisplay     string                     `json:"to_display,omitempty"`
	GeneratedAt   string                     `json:"generated_at"`
	RetentionDays int                        `json:"retention_days"`
	ServerFilter  string                     `json:"server_filter,omitempty"`
	Fleet         map[string]any             `json:"fleet"`
	Servers       []HealthTrendServerSummary `json:"servers"`
}

type ServiceDeps struct {
	DB                          func() *sql.DB
	DBPath                      func() string
	CurrentTimezone             func() (*time.Location, string)
	CurrentLocation             func() *time.Location
	FormatTimestamp             func(string, *time.Location, string) (string, string)
	ServerSnapshot              func() ([]servers.Server, map[string]*servers.ServerStatus)
	HostHealthObservation       health.Reader
	ProjectPolicySchedule       func(policies.ScheduleProjectionRequest) (policies.ScheduleProjection, error)
	ParseAppTimestamp           func(string) (time.Time, error)
	HealthStatusFromResult      func(updates.PrecheckResult) string
	DiskFreeKBFromOutput        func(string) (int64, bool)
	DiskFreeTotalKBFromOutput   func(string) (int64, int64, bool)
	RebootResultRequiresRestart func(updates.PrecheckResult) (bool, bool)
	UpdateCompleteAction        string
	JobTimestampLayout          string
	MetricsCacheTTL             time.Duration
	Logf                        func(string, ...any)
}

type MetricsTokenDeps struct {
	DB                     func() *sql.DB
	DBPath                 func() string
	RandomRead             func([]byte) (int, error)
	HashPassword           func(string) (string, error)
	ComparePasswordAndHash func(string, string) (bool, error)
	StateRLock             func()
	StateRUnlock           func()
	StateLock              func()
	StateUnlock            func()
	SettingKey             string
	EntropyBytes           int
	Logf                   func(string, ...any)
}
