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
	RolloutImmediate    = "immediate"
	RolloutCanaryWaves  = "canary_waves"

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
	RunReasonRolloutGate = "rollout_gate"

	GlobalBlackoutsSetting        = "update_policy_global_blackouts"
	DefaultApprovalTimeoutMinutes = 720
	DefaultRunsLimit              = 100
	MaxRunsLimit                  = 200
	DefaultRunsPageSize           = 25
	DefaultSchedulerTickInterval  = time.Minute
	DefaultCanaryCount            = 1
	DefaultWaveSize               = 10
	DefaultWaveDelayMinutes       = 15
	DefaultTimestampLayout        = "2006-01-02T15:04:05.000000000Z"

	PreviewOccurrenceLimit = 5

	PreviewAdmissionAdmitted          = "admitted"
	PreviewAdmissionBlockedNoRun      = "blocked_no_run"
	PreviewAdmissionNoMatchingServers = "no_matching_servers"
	PreviewAdmissionPolicyDisabled    = "policy_disabled"

	PreviewDSTUnambiguous   = "unambiguous"
	PreviewDSTAmbiguous     = "ambiguous"
	PreviewDSTOffsetChanged = "offset_changed"

	PreviewCanonicalExact           = "exact"
	PreviewCanonicalEarlierFallback = "earlier_fallback_occurrence"

	PreviewConflictPartial = "partial"
	PreviewConflictFull    = "full"
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
	RolloutMode            string           `json:"rollout_mode"`
	CanaryCount            int              `json:"canary_count"`
	WaveSize               int              `json:"wave_size"`
	WaveDelayMinutes       int              `json:"wave_delay_minutes"`
	PolicyBlackouts        []BlackoutWindow `json:"policy_blackouts"`
	CreatedAt              string           `json:"created_at"`
	UpdatedAt              string           `json:"updated_at"`
	MatchedServers         []string         `json:"matched_servers,omitempty"`
}

type PreviewServer struct {
	Name         string   `json:"name"`
	Tags         []string `json:"tags,omitempty"`
	Reason       string   `json:"reason,omitempty"`
	RolloutStage string   `json:"rollout_stage,omitempty"`
	Wave         int      `json:"wave,omitempty"`
}

type RolloutBatch struct {
	Index               int      `json:"index"`
	Stage               string   `json:"stage"`
	ReleaseDelayMinutes int      `json:"release_delay_minutes"`
	Servers             []string `json:"servers"`
}

type PreviewResponse struct {
	MatchedServers      []PreviewServer     `json:"matched_servers"`
	ExcludedServers     []PreviewServer     `json:"excluded_servers"`
	DisabledByOverride  []PreviewServer     `json:"disabled_by_override"`
	UpcomingOccurrences []PreviewOccurrence `json:"upcoming_occurrences"`
	ScheduleConflicts   []PreviewConflict   `json:"schedule_conflicts"`
	ValidationErrors    []PreviewDiagnostic `json:"validation_errors"`
	OperationalWarnings []PreviewDiagnostic `json:"operational_warnings"`
	InformationalFacts  []PreviewDiagnostic `json:"informational_facts"`
	Warnings            []string            `json:"warnings,omitempty"`
}

type PreviewDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PreviewOccurrence struct {
	LocalCivilTime         string                  `json:"local_civil_time"`
	Timezone               string                  `json:"timezone"`
	Offset                 string                  `json:"offset"`
	Abbreviation           string                  `json:"abbreviation"`
	ScheduledForUTC        string                  `json:"scheduled_for_utc"`
	DSTStatus              string                  `json:"dst_status"`
	CanonicalChoice        string                  `json:"canonical_choice"`
	MatchedServerCount     int                     `json:"matched_server_count"`
	ApplicableNoRunWindows []CalendarBlockedWindow `json:"applicable_no_run_windows"`
	AdmissionOutcome       string                  `json:"admission_outcome"`
}

type PreviewConflict struct {
	PolicyID          int64                   `json:"policy_id"`
	PolicyName        string                  `json:"policy_name"`
	OverlapKind       string                  `json:"overlap_kind"`
	SharedServers     []string                `json:"shared_servers"`
	OccurrenceWindows []PreviewConflictWindow `json:"occurrence_windows"`
}

type PreviewConflictWindow struct {
	LocalCivilTime            string `json:"local_civil_time"`
	Timezone                  string `json:"timezone"`
	WindowStartUTC            string `json:"window_start_utc"`
	WindowEndUTC              string `json:"window_end_utc"`
	DraftAdmissionOutcome     string `json:"draft_admission_outcome"`
	CompetingAdmissionOutcome string `json:"competing_admission_outcome"`
	Effective                 bool   `json:"effective"`
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
	ID                   int64  `json:"id"`
	PolicyID             int64  `json:"policy_id"`
	PolicyName           string `json:"policy_name"`
	ServerName           string `json:"server_name"`
	ScheduledForUTC      string `json:"scheduled_for_utc"`
	ScheduledForDisplay  string `json:"scheduled_for_display,omitempty"`
	ExecutionMode        string `json:"execution_mode"`
	PackageScope         string `json:"package_scope"`
	UpgradeMode          string `json:"upgrade_mode"`
	Status               string `json:"status"`
	TerminalOutcome      string `json:"terminal_outcome,omitempty"`
	Reason               string `json:"reason"`
	ExactSkipReason      string `json:"exact_skip_reason,omitempty"`
	Summary              string `json:"summary"`
	JobID                string `json:"job_id"`
	ResultJSON           string `json:"result_json"`
	CreatedAt            string `json:"created_at"`
	UpdatedAt            string `json:"updated_at"`
	StartedAt            string `json:"started_at"`
	StartedAtDisplay     string `json:"started_at_display,omitempty"`
	FinishedAt           string `json:"finished_at"`
	FinishedAtDisplay    string `json:"finished_at_display,omitempty"`
	DurationMilliseconds *int64 `json:"duration_ms,omitempty"`
	JobDetailURL         string `json:"job_detail_url,omitempty"`
	ReportURL            string `json:"report_url,omitempty"`
	AuditURL             string `json:"audit_url,omitempty"`
}

type RunQuery struct {
	Policy         string
	Server         string
	Outcome        string
	FromUTC        string
	ToUTCExclusive string
	Page           int
	PageSize       int
}

type RunPage struct {
	Items      []Run
	Page       int
	PageSize   int
	Total      int
	TotalPages int
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
	TimeLocal        string   `json:"time_local"`
	ScheduledForUTC  string   `json:"scheduled_for_utc"`
	TimezoneOffset   string   `json:"timezone_offset"`
	ExecutionMode    string   `json:"execution_mode"`
	PackageScope     string   `json:"package_scope"`
	UpgradeMode      string   `json:"upgrade_mode"`
	MatchedServers   []string `json:"matched_servers"`
	RolloutMode      string   `json:"rollout_mode"`
	CanaryCount      int      `json:"canary_count,omitempty"`
	WaveSize         int      `json:"wave_size,omitempty"`
	WaveDelayMinutes int      `json:"wave_delay_minutes,omitempty"`
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
