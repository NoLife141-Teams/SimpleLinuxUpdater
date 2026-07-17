package health

// CollectedFacts is the transport-neutral health knowledge collected from a Server.
type CollectedFacts struct {
	ServerName                   string `json:"server_name"`
	CollectedAt                  string `json:"collected_at"`
	OSPrettyName                 string `json:"os_pretty_name"`
	RunningKernelVersion         string `json:"running_kernel_version"`
	LatestInstalledKernelVersion string `json:"latest_installed_kernel_version"`
	UptimeSeconds                int64  `json:"uptime_seconds"`
	DiskStatus                   string `json:"disk_status"`
	DiskFreeKB                   int64  `json:"disk_free_kb"`
	DiskTotalKB                  int64  `json:"disk_total_kb"`
	DiskDetails                  string `json:"disk_details"`
	AptStatus                    string `json:"apt_status"`
	AptDetails                   string `json:"apt_details"`
	RebootRequired               *bool  `json:"reboot_required"`
	RawJSON                      string `json:"raw_json,omitempty"`
}

// Snapshot is one accepted, time-ordered health observation.
type Snapshot struct {
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
	PostcheckNameAptHealth                      = "post_apt_health"
	PostcheckNameRebootNeeded                   = "reboot_required"
)

// MaintenanceOutcome contains transport-neutral facts from completed maintenance.
type MaintenanceOutcome struct {
	ServerName  string
	CompletedAt string
	Kind        MaintenanceKind
	Status      string
	RawJSON     string
}

// CheckResult is one transport-neutral health-check result embedded in maintenance facts.
type CheckResult struct {
	Name    string `json:"name"`
	Passed  bool   `json:"passed"`
	Details string `json:"details"`
	Output  string `json:"output,omitempty"`
	Error   string `json:"error,omitempty"`
}
