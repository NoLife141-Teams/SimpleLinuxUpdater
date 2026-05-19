package observability

import (
	"database/sql"
	"errors"
	"time"

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

type DashboardServerSummary struct {
	Name             string                        `json:"name"`
	LastUpdate       *DashboardUpdateHistory       `json:"last_update,omitempty"`
	LastFailedUpdate *DashboardUpdateHistory       `json:"last_failed_update,omitempty"`
	AvgDurationMS    float64                       `json:"avg_duration_ms"`
	DurationSamples  int                           `json:"duration_samples"`
	NextRun          DashboardScheduleInfo         `json:"next_run"`
	NoRun            DashboardNoRunInfo            `json:"no_run"`
	Health           DashboardHealthInfo           `json:"health"`
	Risk             DashboardRiskInfo             `json:"risk"`
	CommandHistory   []DashboardCommandHistoryItem `json:"command_history"`
}

type DashboardSummaryResponse struct {
	Window      string                   `json:"window"`
	From        string                   `json:"from"`
	To          string                   `json:"to"`
	GeneratedAt string                   `json:"generated_at"`
	Fleet       map[string]any           `json:"fleet"`
	Servers     []DashboardServerSummary `json:"servers"`
}

type ServiceDeps struct {
	DB                          func() *sql.DB
	DBPath                      func() string
	CurrentTimezone             func() (*time.Location, string)
	CurrentLocation             func() *time.Location
	FormatTimestamp             func(string, *time.Location, string) (string, string)
	ServerSnapshot              func() ([]servers.Server, map[string]*servers.ServerStatus)
	LoadServerFacts             func() (map[string]updates.ServerFactsRecord, error)
	ListPolicies                func() ([]policies.Policy, error)
	LoadOverrides               func() (map[int64]map[string]bool, error)
	LoadGlobalBlackouts         func() ([]policies.BlackoutWindow, error)
	ListPolicyRuns              func(int) ([]policies.Run, error)
	PolicyMatchesServer         func(policies.Policy, servers.Server, map[int64]map[string]bool) bool
	PolicyDueAt                 func(policies.Policy, time.Time) bool
	BlackoutApplies             func(time.Time, []policies.BlackoutWindow) bool
	ComparePolicyCandidates     func(policies.ScheduledCandidate, policies.ScheduledCandidate) bool
	CanonicalScheduledForUTC    func(time.Time) string
	ParseTimeLocalMinutes       func(string) (int, error)
	ParseAppTimestamp           func(string) (time.Time, error)
	HealthStatusFromResult      func(updates.PrecheckResult) string
	DiskFreeKBFromOutput        func(string) (int64, bool)
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
