package observability

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			meta_json TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			client_ip TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("create jobs schema: %v", err)
	}
	return db, path
}

func testHealthReader(latest func() (map[string]health.CollectedFacts, error)) health.Reader {
	return health.ReaderFuncs{
		LatestFunc: latest,
		HistoryFunc: func(string, string, string) ([]health.Snapshot, error) {
			return []health.Snapshot{}, nil
		},
		RetentionDaysFunc: func() (int, error) { return health.DefaultRetentionDays, nil },
	}
}

func insertAudit(t *testing.T, db *sql.DB, createdAt, action, status, targetType, targetName, message string, meta map[string]any) {
	t.Helper()
	metaJSON := ""
	if meta != nil {
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		metaJSON = string(raw)
	}
	if _, err := db.Exec(
		`INSERT INTO audit_events (created_at, action, status, target_type, target_name, message, meta_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		createdAt, action, status, targetType, targetName, message, metaJSON,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
}

func insertDashboardJob(t *testing.T, db *sql.DB, id, serverName, status, phase, summary string, createdAt time.Time) {
	t.Helper()
	timestamp := jobs.FormatTimestamp(createdAt)
	if _, err := db.Exec(
		`INSERT INTO jobs (
			id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
			error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at
		) VALUES (?, ?, '', ?, 'tester', '', ?, ?, ?, '', '', '{}', '{}', ?, ?, ?, '')`,
		id,
		jobs.KindUpdate,
		serverName,
		status,
		phase,
		summary,
		timestamp,
		timestamp,
		timestamp,
	); err != nil {
		t.Fatalf("insert dashboard job: %v", err)
	}
}

func insertHealthSnapshot(t *testing.T, db *sql.DB, record updates.HealthSnapshotRecord) {
	t.Helper()
	var reboot any
	if record.RebootRequired != nil {
		if *record.RebootRequired {
			reboot = 1
		} else {
			reboot = 0
		}
	}
	_, err := db.Exec(`INSERT INTO server_health_snapshots (
		server_name, captured_at, source, package_count, security_count,
		last_scan_status, last_update_status, disk_status, disk_free_kb, disk_total_kb,
		apt_status, reboot_required, os_pretty_name, raw_json
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		record.ServerName, record.CapturedAt, record.Source, record.PackageCount, record.SecurityCount,
		record.LastScanStatus, record.LastUpdateStatus, record.DiskStatus, record.DiskFreeKB, record.DiskTotalKB,
		record.AptStatus, reboot, record.OSPrettyName, record.RawJSON,
	)
	if err != nil {
		t.Fatalf("insert health snapshot fixture: %v", err)
	}
}

func testService(db *sql.DB, path string) *Service {
	nowLoc := time.UTC
	return NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return nowLoc, "UTC" },
		CurrentLocation: func() *time.Location { return nowLoc },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return "display:" + raw, "UTC"
		},
		UpdateCompleteAction: "update.complete",
		JobTimestampLayout:   policies.DefaultTimestampLayout,
	})
}

func TestServiceBuildSummaryAggregatesAndSorts(t *testing.T) {
	db, path := newTestDB(t, "summary.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "ok", map[string]any{"duration_ms": 1000})
	insertAudit(t, db, now.Add(-2*time.Hour).Format(time.RFC3339), "update.complete", "failure", "server", "srv-b", "pre", map[string]any{"precheck_failed": "apt_health"})
	insertAudit(t, db, now.Add(-3*time.Hour).Format(time.RFC3339), "update.complete", "failure", "server", "srv-c", "retry", map[string]any{"retry_exhausted": true, "execution_duration_ms": "500"})
	insertAudit(t, db, now.Add(-40*24*time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "old", "old", map[string]any{"duration_ms": 999})

	summary, err := testService(db, path).BuildSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	if summary.Window != "7d" || !strings.HasPrefix(summary.FromDisplay, "display:") || !strings.HasPrefix(summary.ToDisplay, "display:") {
		t.Fatalf("summary window/display = %q/%q/%q", summary.Window, summary.FromDisplay, summary.ToDisplay)
	}
	if summary.Totals.UpdatesTotal != 3 || summary.Totals.UpdatesSuccess != 1 || summary.Totals.UpdatesFailure != 2 {
		t.Fatalf("totals = %+v, want 3 total, 1 success, 2 failure", summary.Totals)
	}
	if summary.Duration.AvgMS != 750 || summary.Duration.SamplesWithDuration != 2 || summary.Duration.SamplesWithoutDuration != 1 {
		t.Fatalf("duration = %+v, want avg 750 with 2 samples and 1 missing", summary.Duration)
	}
	if len(summary.FailureCauses) != 2 || summary.FailureCauses[0].Cause != "precheck:apt_health" || summary.FailureCauses[1].Cause != "retry_exhausted" {
		t.Fatalf("failure causes = %+v, want deterministic causes", summary.FailureCauses)
	}
}

func TestServiceBuildDashboardSummaryUsesInjectedState(t *testing.T) {
	db, path := newTestDB(t, "dashboard.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rebootRequired := true
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "done", map[string]any{
		"execution_duration_ms": 300,
		"postcheck_results": []updates.PrecheckResult{
			{Name: updates.PostcheckNameRebootNeeded, Details: "reboot required"},
		},
	})
	insertAudit(t, db, now.Add(-30*time.Minute).Format(time.RFC3339), "server.facts.refresh", "success", "server", "srv-a", "facts", nil)
	insertDashboardJob(t, db, "job-srv-a", "srv-a", jobs.StatusWaitingApproval, jobs.PhaseApprovalWait, "Waiting for approval", now.Add(-20*time.Minute))

	service := NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return time.UTC, "UTC" },
		CurrentLocation: func() *time.Location { return time.UTC },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return raw, "UTC"
		},
		ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
			return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}}, map[string]*servers.ServerStatus{
				"srv-a": {Name: "srv-a", Status: "pending_approval", PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true, CVEs: []string{"CVE-2026-1"}}}},
			}
		},
		HostHealthObservation: testHealthReader(func() (map[string]health.CollectedFacts, error) {
			return map[string]health.CollectedFacts{
				"srv-a": {
					ServerName:     "srv-a",
					CollectedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
					OSPrettyName:   "Ubuntu",
					DiskStatus:     "ok",
					AptStatus:      "ok",
					RebootRequired: &rebootRequired,
				},
			}, nil
		}),
		ParseAppTimestamp: func(raw string) (time.Time, error) {
			return time.Parse(time.RFC3339, raw)
		},
		HealthStatusFromResult: func(result updates.PrecheckResult) string {
			if result.Passed {
				return "ok"
			}
			return "critical"
		},
		RebootResultRequiresRestart: func(updates.PrecheckResult) (bool, bool) { return true, true },
		UpdateCompleteAction:        "update.complete",
	})

	summary, err := service.BuildDashboardSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildDashboardSummary() error = %v", err)
	}
	if len(summary.Servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(summary.Servers))
	}
	got := summary.Servers[0]
	if got.Risk.Level != "critical" || len(got.Risk.CVEs) != 1 {
		t.Fatalf("risk = %+v, want critical CVE risk", got.Risk)
	}
	if got.LastUpdate == nil || got.LastUpdate.DurationMS != 300 {
		t.Fatalf("last update = %+v, want duration 300", got.LastUpdate)
	}
	if got.Health.RebootRequired == nil || !*got.Health.RebootRequired {
		t.Fatalf("reboot required = %v, want true", got.Health.RebootRequired)
	}
	if got.NextRun.State != "none" || got.NoRun.Active {
		t.Fatalf("schedule/no-run = %+v/%+v, want no scheduled run and inactive blackout", got.NextRun, got.NoRun)
	}
	if got.Timeline.CurrentPhase != "pending_approval" || got.Timeline.State != "waiting" || got.Timeline.ProgressPct != 12 {
		t.Fatalf("timeline = %+v, want pending approval waiting at default progress", got.Timeline)
	}
	if len(got.Timeline.Phases) != 6 || got.Timeline.Phases[0].Key != "pending_approval" || got.Timeline.Phases[0].State != "waiting" {
		t.Fatalf("timeline phases = %+v, want fixed pending approval first phase", got.Timeline.Phases)
	}
	if !got.ApprovalTriage.Eligible || got.ApprovalTriage.PendingPackages != 1 || got.ApprovalTriage.SecurityUpdates != 1 || got.ApprovalTriage.CVECount != 1 {
		t.Fatalf("approval triage = %+v, want eligible one-package critical queue", got.ApprovalTriage)
	}
	if !got.ApprovalTriage.CanApproveAll || !got.ApprovalTriage.CanApproveSecurity || !got.ApprovalTriage.CanCancel {
		t.Fatalf("approval actions = %+v, want approval controls enabled", got.ApprovalTriage)
	}
	if got.ApprovalTriage.CanRefreshFacts || got.ApprovalTriage.CanRunChecks {
		t.Fatalf("approval transient actions = %+v, want locked while waiting for approval", got.ApprovalTriage)
	}
	if got.ApprovalTriage.FactsState != "fresh" || got.ApprovalTriage.RiskOrder != 4 {
		t.Fatalf("approval freshness/risk = %+v, want fresh critical ordering", got.ApprovalTriage)
	}
	if summary.Fleet["pending_approval"] != 1 || summary.Fleet["high_risk_cve"] != 1 || summary.Fleet["pending_packages"] != 1 || summary.Fleet["security_updates"] != 1 {
		t.Fatalf("fleet counts = %+v, want pending approval/high risk/package/security counts", summary.Fleet)
	}
}

func TestServiceBuildDashboardSummaryUsesPolicyScheduleProjection(t *testing.T) {
	db, path := newTestDB(t, "dashboard-schedule-projection.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	scheduledFor := now.Add(2 * time.Hour).Format(time.RFC3339)

	service := NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return time.UTC, "UTC" },
		CurrentLocation: func() *time.Location { return time.UTC },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return "display:" + raw, "UTC"
		},
		ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
			return []servers.Server{{Name: "srv-a"}}, map[string]*servers.ServerStatus{
				"srv-a": {Name: "srv-a", Status: "online"},
			}
		},
		HostHealthObservation: testHealthReader(func() (map[string]health.CollectedFacts, error) {
			return map[string]health.CollectedFacts{}, nil
		}),
		ProjectPolicySchedule: func(req policies.ScheduleProjectionRequest) (policies.ScheduleProjection, error) {
			if req.RunLimit != 500 || len(req.Servers) != 1 || req.Servers[0].Name != "srv-a" {
				t.Fatalf("schedule projection request = %+v, want dashboard server and run limit", req)
			}
			return policies.ScheduleProjection{Servers: map[string]policies.ServerScheduleProjection{
				"srv-a": {
					NextRun: policies.ProjectedScheduleRun{
						State:           policies.ScheduleProjectionStateScheduled,
						PolicyName:      "nightly",
						ScheduledForUTC: scheduledFor,
						Status:          "scheduled",
						Summary:         "Scheduled run pending",
					},
					NoRun: policies.NoRunWindow{
						Active:     true,
						Scope:      policies.NoRunScopePolicy,
						Reason:     policies.RunReasonBlackout,
						PolicyName: "nightly",
					},
				},
			}}, nil
		},
		ParseAppTimestamp: func(raw string) (time.Time, error) {
			return time.Parse(time.RFC3339, raw)
		},
		UpdateCompleteAction: "update.complete",
	})

	summary, err := service.BuildDashboardSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildDashboardSummary() error = %v", err)
	}
	if len(summary.Servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(summary.Servers))
	}
	got := summary.Servers[0]
	if got.NextRun.State != policies.ScheduleProjectionStateScheduled || got.NextRun.PolicyName != "nightly" || got.NextRun.ScheduledForDisplay != "display:"+scheduledFor {
		t.Fatalf("next run = %+v, want injected policy projection", got.NextRun)
	}
	if !got.NoRun.Active || got.NoRun.Scope != policies.NoRunScopePolicy || got.NoRun.Summary != "nightly no-run window active" {
		t.Fatalf("no-run = %+v, want injected policy no-run window", got.NoRun)
	}
}

func TestServiceBuildDashboardSummaryMapsTimelineAndStaleFacts(t *testing.T) {
	db, path := newTestDB(t, "dashboard-timeline.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	insertDashboardJob(t, db, "job-active", "srv-active", jobs.StatusRunning, jobs.PhaseAptUpdate, "Running apt update", now.Add(-12*time.Minute))
	insertDashboardJob(t, db, "job-stale-terminal-active", "srv-stale-terminal-active", jobs.StatusFailed, jobs.PhasePostchecks, "Old post-check failed", now.Add(-8*time.Minute))
	insertDashboardJob(t, db, "job-failed", "srv-failed", jobs.StatusFailed, jobs.PhasePostchecks, "Post-check failed", now.Add(-9*time.Minute))
	insertDashboardJob(t, db, "job-done", "srv-done", jobs.StatusSucceeded, jobs.PhaseComplete, "Update completed", now.Add(-6*time.Minute))

	service := NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return time.UTC, "UTC" },
		CurrentLocation: func() *time.Location { return time.UTC },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return raw, "UTC"
		},
		ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
			snapshot := []servers.Server{
				{Name: "srv-active"},
				{Name: "srv-done"},
				{Name: "srv-failed"},
				{Name: "srv-facts-refresh"},
				{Name: "srv-stale-terminal-active"},
				{Name: "srv-sudoers"},
			}
			return snapshot, map[string]*servers.ServerStatus{
				"srv-active":                {Name: "srv-active", Status: "updating"},
				"srv-done":                  {Name: "srv-done", Status: "done"},
				"srv-failed":                {Name: "srv-failed", Status: "error"},
				"srv-facts-refresh":         {Name: "srv-facts-refresh", Status: "facts_refresh"},
				"srv-stale-terminal-active": {Name: "srv-stale-terminal-active", Status: "updating"},
				"srv-sudoers":               {Name: "srv-sudoers", Status: "sudoers"},
			}
		},
		HostHealthObservation: testHealthReader(func() (map[string]health.CollectedFacts, error) {
			return map[string]health.CollectedFacts{
				"srv-active":                {ServerName: "srv-active", CollectedAt: now.Add(-49 * time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
				"srv-done":                  {ServerName: "srv-done", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
				"srv-failed":                {ServerName: "srv-failed", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
				"srv-facts-refresh":         {ServerName: "srv-facts-refresh", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
				"srv-stale-terminal-active": {ServerName: "srv-stale-terminal-active", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
				"srv-sudoers":               {ServerName: "srv-sudoers", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
			}, nil
		}),
		ParseAppTimestamp: func(raw string) (time.Time, error) {
			return time.Parse(time.RFC3339, raw)
		},
		UpdateCompleteAction: "update.complete",
	})

	summary, err := service.BuildDashboardSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildDashboardSummary() error = %v", err)
	}
	byName := map[string]DashboardServerSummary{}
	for _, item := range summary.Servers {
		byName[item.Name] = item
	}
	if got := byName["srv-active"].Timeline; got.CurrentPhase != "apt_update" || got.State != "active" || got.ProgressPct != 52 {
		t.Fatalf("srv-active timeline = %+v, want active apt update", got)
	}
	if got := byName["srv-active"].ApprovalTriage.FactsState; got != "stale" {
		t.Fatalf("srv-active facts state = %q, want stale", got)
	}
	if byName["srv-active"].ApprovalTriage.CanRefreshFacts {
		t.Fatalf("srv-active CanRefreshFacts = true, want false while action is active")
	}
	if !byName["srv-done"].ApprovalTriage.CanRefreshFacts {
		t.Fatalf("srv-done CanRefreshFacts = false, want true after terminal state")
	}
	if got := byName["srv-stale-terminal-active"].Timeline; got.CurrentPhase != "prechecks" || got.State != "active" {
		t.Fatalf("srv-stale-terminal-active timeline = %+v, want live updating state instead of stale failed job", got)
	}
	if got := byName["srv-facts-refresh"].Timeline; got.CurrentPhase != "prechecks" || got.State != "active" {
		t.Fatalf("srv-facts-refresh timeline = %+v, want active facts refresh", got)
	}
	if got := byName["srv-sudoers"].Timeline; got.CurrentPhase != "prechecks" || got.State != "active" {
		t.Fatalf("srv-sudoers timeline = %+v, want active sudoers action", got)
	}
	if got := byName["srv-failed"].Timeline; got.CurrentPhase != "done_error" || got.State != "error" {
		t.Fatalf("srv-failed timeline = %+v, want terminal error", got)
	}
	if got := byName["srv-done"].Timeline; got.CurrentPhase != "done_error" || got.State != "done" {
		t.Fatalf("srv-done timeline = %+v, want terminal done", got)
	}
	if summary.Fleet["in_progress"] != 4 || summary.Fleet["prechecks_running"] != 4 || summary.Fleet["done"] != 1 || summary.Fleet["stale_facts"] != 1 {
		t.Fatalf("fleet counts = %+v, want active/done/stale facts counts", summary.Fleet)
	}
}

func TestServiceBuildHealthTrendsAggregatesActiveServers(t *testing.T) {
	db, path := newTestDB(t, "health-trends.db")
	if err := health.EnsureServerFactsSchema(db); err != nil {
		t.Fatalf("EnsureServerFactsSchema() error = %v", err)
	}
	now := time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)
	rebootRequired := true
	insertHealthSnapshot(t, db, updates.HealthSnapshotRecord{
		ServerName:       "srv-a",
		CapturedAt:       now.Add(-6 * 24 * time.Hour).Format(time.RFC3339),
		Source:           "facts",
		PackageCount:     5,
		SecurityCount:    2,
		DiskStatus:       "ok",
		DiskFreeKB:       4096,
		DiskTotalKB:      8192,
		AptStatus:        "ok",
		LastUpdateStatus: "success",
		OSPrettyName:     "Ubuntu",
	})
	insertHealthSnapshot(t, db, updates.HealthSnapshotRecord{
		ServerName:       "srv-a",
		CapturedAt:       now.Add(-time.Hour).Format(time.RFC3339),
		Source:           "audit",
		PackageCount:     2,
		SecurityCount:    1,
		DiskStatus:       "critical",
		DiskFreeKB:       1024,
		DiskTotalKB:      8192,
		AptStatus:        "critical",
		LastUpdateStatus: "failure",
		RebootRequired:   &rebootRequired,
		OSPrettyName:     "Ubuntu",
	})
	insertHealthSnapshot(t, db, updates.HealthSnapshotRecord{
		ServerName:     "srv-b",
		CapturedAt:     now.Add(-2 * time.Hour).Format(time.RFC3339),
		Source:         "audit",
		PackageCount:   3,
		SecurityCount:  3,
		DiskStatus:     "ok",
		AptStatus:      "ok",
		LastScanStatus: "failure",
	})
	insertHealthSnapshot(t, db, updates.HealthSnapshotRecord{
		ServerName:   "deleted",
		CapturedAt:   now.Add(-time.Hour).Format(time.RFC3339),
		Source:       "audit",
		PackageCount: 99,
		DiskStatus:   "critical",
		AptStatus:    "critical",
	})
	insertHealthSnapshot(t, db, updates.HealthSnapshotRecord{
		ServerName:   "srv-a",
		CapturedAt:   now.Add(-40 * 24 * time.Hour).Format(time.RFC3339),
		Source:       "audit",
		PackageCount: 99,
	})

	repo := health.SQLiteObservation{DB: func() *sql.DB { return db }}
	service := NewService(ServiceDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return path },
		CurrentTimezone: func() (*time.Location, string) {
			return time.UTC, "UTC"
		},
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return "display:" + raw, "UTC"
		},
		ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
			return []servers.Server{{Name: "srv-a"}, {Name: "srv-b"}}, nil
		},
		HostHealthObservation: repo,
	})

	trends, err := service.BuildHealthTrends("7d", "", now)
	if err != nil {
		t.Fatalf("BuildHealthTrends() error = %v", err)
	}
	if trends.RetentionDays != health.DefaultRetentionDays || !strings.HasPrefix(trends.FromDisplay, "display:") {
		t.Fatalf("retention/display = %d/%q, want default retention and display", trends.RetentionDays, trends.FromDisplay)
	}
	if len(trends.Servers) != 2 {
		t.Fatalf("server trends count = %d, want 2: %+v", len(trends.Servers), trends.Servers)
	}
	byName := map[string]HealthTrendServerSummary{}
	for _, item := range trends.Servers {
		byName[item.Name] = item
	}
	srvA := byName["srv-a"]
	if srvA.Samples != 2 || srvA.PackageDelta != -3 || srvA.SecurityDelta != -1 || srvA.DiskFreeDeltaKB != -3072 {
		t.Fatalf("srv-a trend = %+v, want sample deltas", srvA)
	}
	if srvA.UpdateFailures != 1 || srvA.AptProblemSamples != 1 || srvA.DiskProblemSamples != 1 || !srvA.RebootSeen {
		t.Fatalf("srv-a problem counts = %+v, want update/health/reboot signals", srvA)
	}
	if srvA.Latest == nil || srvA.Latest.CapturedAt != now.Add(-time.Hour).Format(time.RFC3339) {
		t.Fatalf("srv-a latest = %+v, want newest point", srvA.Latest)
	}
	if byName["srv-b"].ScanFailures != 1 {
		t.Fatalf("srv-b scan failures = %+v, want 1", byName["srv-b"])
	}
	if trends.Fleet["servers_with_samples"] != 2 || trends.Fleet["samples"] != 3 || trends.Fleet["update_failures"] != 1 || trends.Fleet["scan_failures"] != 1 || trends.Fleet["reboot_seen"] != 1 {
		t.Fatalf("fleet = %+v, want aggregate health trend counts", trends.Fleet)
	}

	filtered, err := service.BuildHealthTrends("30d", "srv-a", now)
	if err != nil {
		t.Fatalf("BuildHealthTrends(filtered) error = %v", err)
	}
	if len(filtered.Servers) != 1 || filtered.Servers[0].Name != "srv-a" {
		t.Fatalf("filtered servers = %+v, want only srv-a", filtered.Servers)
	}
	missing, err := service.BuildHealthTrends("7d", "deleted", now)
	if err != nil {
		t.Fatalf("BuildHealthTrends(deleted) error = %v", err)
	}
	if len(missing.Servers) != 0 || missing.Fleet["samples"] != 0 {
		t.Fatalf("deleted server trends = %+v fleet=%+v, want empty", missing.Servers, missing.Fleet)
	}
	if _, _, err := ParseHealthTrendWindow("24h"); !errors.Is(err, ErrInvalidWindow) {
		t.Fatalf("ParseHealthTrendWindow(24h) error = %v, want ErrInvalidWindow", err)
	}
}

func TestServiceBuildMetricsCachesPerDBPathAndWindow(t *testing.T) {
	db, path := newTestDB(t, "metrics-cache.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service := testService(db, path)

	first, err := service.BuildMetrics(now)
	if err != nil {
		t.Fatalf("BuildMetrics(first) error = %v", err)
	}
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "ok", nil)
	second, err := service.BuildMetrics(now.Add(time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("cached metrics changed within TTL")
	}

	third, err := service.BuildMetrics(now.Add(DefaultMetricsCacheTTL + time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(third) error = %v", err)
	}
	if !strings.Contains(third, `simplelinuxupdater_update_runs{window="24h",status="success"} 1`) {
		t.Fatalf("metrics after TTL missing new success count:\n%s", third)
	}

	otherDB, otherPath := newTestDB(t, "metrics-cache-other.db")
	service.deps.DB = func() *sql.DB { return otherDB }
	service.deps.DBPath = func() string { return otherPath }
	other, err := service.BuildMetrics(now.Add(2 * time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(other DB) error = %v", err)
	}
	if strings.Contains(other, `status="success"} 1`) {
		t.Fatalf("other DB metrics reused cached success count:\n%s", other)
	}
}

func TestMetricsTokenServiceLifecycleAndFallback(t *testing.T) {
	db, path := newTestDB(t, "token.db")
	random := byte(1)
	service := NewMetricsTokenService(MetricsTokenDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return path },
		RandomRead: func(buf []byte) (int, error) {
			for i := range buf {
				buf[i] = random
			}
			random++
			return len(buf), nil
		},
		HashPassword: func(token string) (string, error) {
			raw, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
			return string(raw), err
		},
		ComparePasswordAndHash: func(token, hash string) (bool, error) {
			return bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil, nil
		},
	})

	if service.Status() {
		t.Fatalf("Status() = true before token creation")
	}
	token, err := service.Rotate()
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if token == "" || !service.Status() {
		t.Fatalf("Rotate() token/status = %q/%t, want token and enabled", token, service.Status())
	}
	ok, err := service.VerifyBearerToken(token)
	if err != nil || !ok {
		t.Fatalf("VerifyBearerToken(valid) = %t/%v, want true/nil", ok, err)
	}
	ok, err = service.VerifyBearerToken("wrong")
	if err != nil || ok {
		t.Fatalf("VerifyBearerToken(wrong) = %t/%v, want false/nil", ok, err)
	}
	if err := service.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if service.Status() {
		t.Fatalf("Status() = true after clear")
	}

	cachedHash := "$2a$04$jW5I0PMb7s8eyxZswzruCOx2Unio5jXScWp55MSfS.KMtMucHEVKq"
	closedDB, closedPath := newTestDB(t, "token-closed.db")
	service.deps.DB = func() *sql.DB { return closedDB }
	service.deps.DBPath = func() string { return closedPath }
	service.RestoreCache(cachedHash, false, closedPath)
	_ = closedDB.Close()
	if got := service.Hash(); got != cachedHash {
		t.Fatalf("Hash() locked fallback = %q, want cached hash", got)
	}
}

func TestMetricsTokenServiceUnavailableRandom(t *testing.T) {
	db, path := newTestDB(t, "token-random.db")
	service := NewMetricsTokenService(MetricsTokenDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return path },
		RandomRead: func([]byte) (int, error) {
			return 0, errors.New("entropy unavailable")
		},
		HashPassword: func(string) (string, error) { return "", nil },
	})
	if _, err := service.Rotate(); err == nil {
		t.Fatalf("Rotate() error = nil, want entropy error")
	}
}
