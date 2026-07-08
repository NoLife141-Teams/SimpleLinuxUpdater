package policies

import (
	"time"

	"debian-updater/internal/servers"
)

const (
	ExecutionScanOnly         = "scan_only"
	ExecutionApprovalRequired = "approval_required"
	ExecutionAutoApply        = "auto_apply"

	PackageScopeSecurity = "security"
	PackageScopeFull     = "full"

	UpgradeModeStandard = "standard"
	UpgradeModeFull     = "full"

	CadenceDaily  = "daily"
	CadenceWeekly = "weekly"

	RunQueued          = "queued"
	RunRunning         = "running"
	RunWaitingApproval = "waiting_approval"
	RunSucceeded       = "succeeded"
	RunFailed          = "failed"
	RunSkipped         = "skipped"
	RunCancelled       = "cancelled"
	RunInterrupted     = "interrupted"

	RunReasonBlackout    = "blackout"
	RunReasonBusy        = "busy"
	RunReasonSuperseded  = "superseded"
	RunReasonRestart     = "restart"
	RunReasonNoMatch     = "no_match"
	RunReasonMissing     = "missing"
	RunReasonMaintenance = "maintenance"
	RunReasonPersistence = "persistence"

	GlobalBlackoutsSetting        = "update_policy_global_blackouts"
	DefaultApprovalTimeoutMinutes = 720
	DefaultRunsLimit              = 100
	MaxRunsLimit                  = 200
	DefaultSchedulerTickInterval  = time.Minute
	DefaultTimestampLayout        = "2006-01-02T15:04:05.000000000Z"
)

type BlackoutWindow struct {
	Weekdays  []string `json:"weekdays"`
	StartTime string   `json:"start_time"`
	EndTime   string   `json:"end_time"`
}

type Policy struct {
	ID                     int64            `json:"id"`
	Name                   string           `json:"name"`
	Enabled                bool             `json:"enabled"`
	TargetTag              string           `json:"target_tag"`
	IncludeTags            []string         `json:"include_tags"`
	ExcludeTags            []string         `json:"exclude_tags"`
	TargetServers          []string         `json:"target_servers"`
	PackageScope           string           `json:"package_scope"`
	UpgradeMode            string           `json:"upgrade_mode"`
	ExecutionMode          string           `json:"execution_mode"`
	CadenceKind            string           `json:"cadence_kind"`
	TimeLocal              string           `json:"time_local"`
	Weekdays               []string         `json:"weekdays"`
	ApprovalTimeoutMinutes int              `json:"approval_timeout_minutes"`
	PolicyBlackouts        []BlackoutWindow `json:"policy_blackouts"`
	CreatedAt              string           `json:"created_at"`
	UpdatedAt              string           `json:"updated_at"`
	MatchedServers         []string         `json:"matched_servers,omitempty"`
}

type PreviewServer struct {
	Name   string   `json:"name"`
	Tags   []string `json:"tags,omitempty"`
	Reason string   `json:"reason,omitempty"`
}

type PreviewResponse struct {
	MatchedServers     []PreviewServer `json:"matched_servers"`
	ExcludedServers    []PreviewServer `json:"excluded_servers"`
	DisabledByOverride []PreviewServer `json:"disabled_by_override"`
	Warnings           []string        `json:"warnings"`
}

type Override struct {
	PolicyID    int64  `json:"policy_id"`
	ServerName  string `json:"server_name"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	PolicyName  string `json:"policy_name,omitempty"`
	TargetTag   string `json:"target_tag,omitempty"`
	ServerMatch bool   `json:"server_match,omitempty"`
}

type Run struct {
	ID                  int64  `json:"id"`
	PolicyID            int64  `json:"policy_id"`
	PolicyName          string `json:"policy_name"`
	ServerName          string `json:"server_name"`
	ScheduledForUTC     string `json:"scheduled_for_utc"`
	ScheduledForDisplay string `json:"scheduled_for_display,omitempty"`
	ExecutionMode       string `json:"execution_mode"`
	PackageScope        string `json:"package_scope"`
	UpgradeMode         string `json:"upgrade_mode"`
	Status              string `json:"status"`
	Reason              string `json:"reason"`
	Summary             string `json:"summary"`
	JobID               string `json:"job_id"`
	ResultJSON          string `json:"result_json"`
	CreatedAt           string `json:"created_at"`
	UpdatedAt           string `json:"updated_at"`
	StartedAt           string `json:"started_at"`
	FinishedAt          string `json:"finished_at"`
}

type ScheduleProjectionRequest struct {
	Now      time.Time
	Servers  []servers.Server
	RunLimit int
}

type ScheduleProjection struct {
	Servers map[string]ServerScheduleProjection
}

type ServerScheduleProjection struct {
	NextRun ProjectedScheduleRun
	NoRun   NoRunWindow
}

type ProjectedScheduleRun struct {
	State           string
	PolicyName      string
	ScheduledForUTC string
	Status          string
	Reason          string
	Summary         string
}

type NoRunWindow struct {
	Active     bool
	Scope      string
	Reason     string
	PolicyName string
}

type SettingsResponse struct {
	Timezone         string           `json:"timezone"`
	ResolvedTimezone string           `json:"resolved_timezone"`
	GlobalBlackouts  []BlackoutWindow `json:"global_blackouts"`
}

type CalendarOptions struct {
	Start    time.Time
	Days     int
	PolicyID int64
}

type CalendarResponse struct {
	Days        int              `json:"days"`
	StartDate   string           `json:"start_date"`
	EndDate     string           `json:"end_date"`
	GeneratedAt string           `json:"generated_at"`
	Policies    []CalendarPolicy `json:"policies"`
}

type CalendarPolicy struct {
	ID             int64         `json:"id"`
	Name           string        `json:"name"`
	Enabled        bool          `json:"enabled"`
	CadenceKind    string        `json:"cadence_kind"`
	TimeLocal      string        `json:"time_local"`
	Weekdays       []string      `json:"weekdays"`
	MatchedServers []string      `json:"matched_servers"`
	Days           []CalendarDay `json:"days"`
}

type CalendarDay struct {
	Date           string                  `json:"date"`
	Weekday        string                  `json:"weekday"`
	TimezoneOffset string                  `json:"timezone_offset"`
	AllowedSlots   []CalendarSlot          `json:"allowed_slots"`
	BlockedWindows []CalendarBlockedWindow `json:"blocked_windows"`
	BlockedReasons []string                `json:"blocked_reasons,omitempty"`
}

type CalendarSlot struct {
	TimeLocal       string   `json:"time_local"`
	ScheduledForUTC string   `json:"scheduled_for_utc"`
	TimezoneOffset  string   `json:"timezone_offset"`
	ExecutionMode   string   `json:"execution_mode"`
	PackageScope    string   `json:"package_scope"`
	UpgradeMode     string   `json:"upgrade_mode"`
	MatchedServers  []string `json:"matched_servers"`
}

type CalendarBlockedWindow struct {
	Source        string   `json:"source"`
	Weekdays      []string `json:"weekdays"`
	StartTime     string   `json:"start_time"`
	EndTime       string   `json:"end_time"`
	Overnight     bool     `json:"overnight"`
	AppliesToSlot bool     `json:"applies_to_slot"`
}

type RunUpdate struct {
	Status     *string
	Reason     *string
	Summary    *string
	JobID      *string
	ResultJSON *string
	StartedAt  *string
	FinishedAt *string
}
