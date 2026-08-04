package observability

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	auditpkg "debian-updater/internal/audit"
	healthpkg "debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	runtimepkg "debian-updater/internal/runtime"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

type cacheEntry struct {
	summary  SummaryResponse
	cachedAt time.Time
}

type Service struct {
	deps          ServiceDeps
	mu            sync.RWMutex
	cache         map[string]cacheEntry
	dashboardSlot chan struct{}
}

func NewService(deps ServiceDeps) *Service {
	deps = deps.withDefaults()
	return &Service{
		deps:          deps,
		cache:         map[string]cacheEntry{},
		dashboardSlot: make(chan struct{}, 1),
	}
}

func (s *Service) EnsureDeps() ServiceDeps {
	if s == nil {
		return ServiceDeps{}.withDefaults()
	}
	return s.deps.withDefaults()
}

func (d ServiceDeps) withDefaults() ServiceDeps {
	if d.CurrentTimezone == nil {
		d.CurrentTimezone = func() (*time.Location, string) { return time.UTC, "UTC" }
	}
	if d.CurrentLocation == nil {
		d.CurrentLocation = func() *time.Location { return time.UTC }
	}
	if d.FormatTimestamp == nil {
		d.FormatTimestamp = func(raw string, _ *time.Location, _ string) (string, string) { return raw, "" }
	}
	if d.ServerSnapshot == nil {
		d.ServerSnapshot = func() ([]servers.Server, map[string]*servers.ServerStatus) {
			return []servers.Server{}, map[string]*servers.ServerStatus{}
		}
	}
	if d.MaintenanceReadiness == nil {
		d.MaintenanceReadiness = func(serverList []servers.Server) map[string]servers.MaintenanceReadiness {
			result := make(map[string]servers.MaintenanceReadiness, len(serverList))
			for _, server := range serverList {
				result[server.Name] = servers.MaintenanceReadiness{Ready: true, Code: servers.MaintenanceReadinessReady, Message: "Connection readiness not configured"}
			}
			return result
		}
	}
	if d.HostHealthObservation == nil {
		d.HostHealthObservation = healthpkg.ReaderFuncs{
			LatestFunc: func() (map[string]healthpkg.CollectedFacts, error) {
				return map[string]healthpkg.CollectedFacts{}, nil
			},
			LatestObservationsFunc: func(string) (map[string]healthpkg.Snapshot, error) {
				return map[string]healthpkg.Snapshot{}, nil
			},
			HistoryFunc: func(string, string, string) ([]healthpkg.Snapshot, error) {
				return []healthpkg.Snapshot{}, nil
			},
			RetentionDaysFunc: func() (int, error) {
				return healthpkg.DefaultRetentionDays, nil
			},
		}
	}
	defaultPolicy := policies.NewService(policies.ServiceDeps{})
	if d.ProjectPolicySchedule == nil {
		d.ProjectPolicySchedule = defaultPolicy.ProjectSchedule
	}
	if d.ParseAppTimestamp == nil {
		d.ParseAppTimestamp = func(raw string) (time.Time, error) { return time.Parse(time.RFC3339, raw) }
	}
	if d.HealthStatusFromResult == nil {
		d.HealthStatusFromResult = func(result updates.PrecheckResult) string {
			if result.Passed {
				return "ok"
			}
			return "failed"
		}
	}
	if d.DiskFreeKBFromOutput == nil {
		d.DiskFreeKBFromOutput = func(string) (int64, bool) { return 0, false }
	}
	if d.DiskFreeTotalKBFromOutput == nil {
		d.DiskFreeTotalKBFromOutput = func(string) (int64, int64, bool) { return 0, 0, false }
	}
	if d.RebootResultRequiresRestart == nil {
		d.RebootResultRequiresRestart = func(updates.PrecheckResult) (bool, bool) { return false, false }
	}
	if strings.TrimSpace(d.UpdateCompleteAction) == "" {
		d.UpdateCompleteAction = "update.complete"
	}
	if strings.TrimSpace(d.JobTimestampLayout) == "" {
		d.JobTimestampLayout = policies.DefaultTimestampLayout
	}
	if d.MetricsCacheTTL <= 0 {
		d.MetricsCacheTTL = DefaultMetricsCacheTTL
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return d
}

func ParseHealthTrendWindow(raw string) (string, time.Duration, error) {
	return ParseWindow(raw)
}

func ParseWindow(raw string) (string, time.Duration, error) {
	window := strings.TrimSpace(strings.ToLower(raw))
	if window == "" {
		window = "7d"
	}
	switch window {
	case "24h":
		return window, 24 * time.Hour, nil
	case "7d":
		return window, 7 * 24 * time.Hour, nil
	case "30d":
		return window, 30 * 24 * time.Hour, nil
	default:
		return "", 0, fmt.Errorf("%w: %q", ErrInvalidWindow, raw)
	}
}

func MetaDurationMS(meta map[string]any) (float64, bool) {
	if meta == nil {
		return 0, false
	}
	raw, ok := meta["execution_duration_ms"]
	if !ok {
		raw, ok = meta["duration_ms"]
	}
	if !ok {
		return 0, false
	}
	switch v := raw.(type) {
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return 0, false
		}
		return v, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed < 0 {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func FailureCauseFromMeta(meta map[string]any, metaValid bool) string {
	return auditpkg.FailureCauseFromMeta(meta, metaValid)
}

func (s *Service) BuildSummary(rawWindow string, now time.Time) (SummaryResponse, error) {
	deps := s.EnsureDeps()
	window, span, err := ParseWindow(rawWindow)
	if err != nil {
		return SummaryResponse{}, err
	}
	to := now.UTC()
	from := to.Add(-span)
	summary := SummaryResponse{
		Window: window,
		From:   from.Format(time.RFC3339),
		To:     to.Format(time.RFC3339),
	}
	loc, timezoneName := deps.CurrentTimezone()
	summary.FromDisplay, _ = deps.FormatTimestamp(summary.From, loc, timezoneName)
	summary.ToDisplay, _ = deps.FormatTimestamp(summary.To, loc, timezoneName)
	summary.StatusBreakdown = []StatusItem{
		{Status: "success", Count: 0},
		{Status: "failure", Count: 0},
	}
	failureCauseCounts := map[string]int{}
	failureCauseServers := map[string]map[string]struct{}{}

	db := deps.DB()
	rows, err := db.Query(
		`SELECT status, target_name, meta_json FROM audit_events
		WHERE action = ? AND created_at >= ? AND created_at <= ?`,
		deps.UpdateCompleteAction,
		from.Format(time.RFC3339),
		to.Format(time.RFC3339),
	)
	if err != nil {
		return SummaryResponse{}, err
	}
	defer rows.Close()

	var durationTotal float64
	for rows.Next() {
		var status string
		var targetName string
		var metaJSON string
		if scanErr := rows.Scan(&status, &targetName, &metaJSON); scanErr != nil {
			return SummaryResponse{}, scanErr
		}
		normalizedStatus := strings.ToLower(strings.TrimSpace(status))
		if normalizedStatus != "success" && normalizedStatus != "failure" {
			continue
		}
		summary.Totals.UpdatesTotal++
		if normalizedStatus == "success" {
			summary.Totals.UpdatesSuccess++
		} else {
			summary.Totals.UpdatesFailure++
		}
		for i := range summary.StatusBreakdown {
			if summary.StatusBreakdown[i].Status == normalizedStatus {
				summary.StatusBreakdown[i].Count++
				break
			}
		}

		meta := map[string]any{}
		metaValid := false
		if trimmed := strings.TrimSpace(metaJSON); trimmed != "" {
			if unmarshalErr := json.Unmarshal([]byte(trimmed), &meta); unmarshalErr == nil {
				metaValid = true
			}
		}
		if durationMS, ok := MetaDurationMS(meta); ok {
			durationTotal += durationMS
			summary.Duration.SamplesWithDuration++
		} else {
			summary.Duration.SamplesWithoutDuration++
		}
		if normalizedStatus == "failure" {
			cause := FailureCauseFromMeta(meta, metaValid)
			failureCauseCounts[cause]++
			targetName = strings.TrimSpace(targetName)
			if targetName != "" {
				if failureCauseServers[cause] == nil {
					failureCauseServers[cause] = map[string]struct{}{}
				}
				failureCauseServers[cause][targetName] = struct{}{}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return SummaryResponse{}, err
	}
	if summary.Totals.UpdatesTotal > 0 {
		summary.Totals.SuccessRatePct = (float64(summary.Totals.UpdatesSuccess) / float64(summary.Totals.UpdatesTotal)) * 100
	}
	if summary.Duration.SamplesWithDuration > 0 {
		summary.Duration.AvgMS = durationTotal / float64(summary.Duration.SamplesWithDuration)
	}
	summary.FailureCauses = make([]FailureItem, 0, len(failureCauseCounts))
	for cause, count := range failureCauseCounts {
		serverNames := make([]string, 0, len(failureCauseServers[cause]))
		for serverName := range failureCauseServers[cause] {
			serverNames = append(serverNames, serverName)
		}
		sort.Strings(serverNames)
		summary.FailureCauses = append(summary.FailureCauses, FailureItem{Cause: cause, Count: count, Servers: serverNames})
	}
	sort.Slice(summary.FailureCauses, func(i, j int) bool {
		if summary.FailureCauses[i].Count == summary.FailureCauses[j].Count {
			return summary.FailureCauses[i].Cause < summary.FailureCauses[j].Cause
		}
		return summary.FailureCauses[i].Count > summary.FailureCauses[j].Count
	})
	return summary, nil
}

func (s *Service) metricsSummary(window string, now time.Time) (SummaryResponse, error) {
	deps := s.EnsureDeps()
	cacheKey := deps.DBPath() + "|" + window
	s.mu.RLock()
	if entry, ok := s.cache[cacheKey]; ok && now.Sub(entry.cachedAt) < deps.MetricsCacheTTL {
		s.mu.RUnlock()
		return entry.summary, nil
	}
	s.mu.RUnlock()

	summary, err := s.BuildSummary(window, now)
	if err != nil {
		return SummaryResponse{}, err
	}
	s.mu.Lock()
	s.cache[cacheKey] = cacheEntry{summary: summary, cachedAt: now}
	s.mu.Unlock()
	return summary, nil
}

func PrometheusEscapeLabel(v string) string {
	value := strings.ReplaceAll(v, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return value
}

func (s *Service) BuildMetrics(now time.Time) (string, error) {
	deps := s.EnsureDeps()
	windows := []string{"24h", "7d", "30d"}
	summaries := make([]SummaryResponse, 0, len(windows))
	for _, window := range windows {
		summary, err := s.metricsSummary(window, now)
		if err != nil {
			deps.Logf("handleMetrics: failed to build summary for window=%q: %v", window, err)
			return "", err
		}
		summaries = append(summaries, summary)
	}

	var b strings.Builder
	b.WriteString("# HELP simplelinuxupdater_update_runs Number of completed update runs by status in the selected window.\n")
	b.WriteString("# TYPE simplelinuxupdater_update_runs gauge\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "simplelinuxupdater_update_runs{window=%q,status=%q} %d\n", summary.Window, "success", summary.Totals.UpdatesSuccess)
		fmt.Fprintf(&b, "simplelinuxupdater_update_runs{window=%q,status=%q} %d\n", summary.Window, "failure", summary.Totals.UpdatesFailure)
	}
	b.WriteString("# HELP simplelinuxupdater_update_success_rate_percent Update success rate percentage in the selected window.\n")
	b.WriteString("# TYPE simplelinuxupdater_update_success_rate_percent gauge\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "simplelinuxupdater_update_success_rate_percent{window=%q} %.4f\n", summary.Window, summary.Totals.SuccessRatePct)
	}
	b.WriteString("# HELP simplelinuxupdater_update_duration_avg_milliseconds Average update duration in milliseconds for samples with duration data.\n")
	b.WriteString("# TYPE simplelinuxupdater_update_duration_avg_milliseconds gauge\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "simplelinuxupdater_update_duration_avg_milliseconds{window=%q} %.4f\n", summary.Window, summary.Duration.AvgMS)
	}
	b.WriteString("# HELP simplelinuxupdater_update_duration_samples Number of update samples with/without duration metadata.\n")
	b.WriteString("# TYPE simplelinuxupdater_update_duration_samples gauge\n")
	for _, summary := range summaries {
		fmt.Fprintf(&b, "simplelinuxupdater_update_duration_samples{window=%q,kind=%q} %d\n", summary.Window, "with_duration", summary.Duration.SamplesWithDuration)
		fmt.Fprintf(&b, "simplelinuxupdater_update_duration_samples{window=%q,kind=%q} %d\n", summary.Window, "without_duration", summary.Duration.SamplesWithoutDuration)
	}
	b.WriteString("# HELP simplelinuxupdater_update_failures_by_cause Number of failed update runs grouped by failure cause.\n")
	b.WriteString("# TYPE simplelinuxupdater_update_failures_by_cause gauge\n")
	for _, summary := range summaries {
		for _, failure := range summary.FailureCauses {
			fmt.Fprintf(&b, "simplelinuxupdater_update_failures_by_cause{window=%q,cause=\"%s\"} %d\n", summary.Window, PrometheusEscapeLabel(failure.Cause), failure.Count)
		}
	}
	return b.String(), nil
}

func UpdateHealthFromResults(health *DashboardHealthInfo, results []updates.PrecheckResult, source, collectedAt string, deps ServiceDeps) {
	deps = deps.withDefaults()
	if health == nil {
		return
	}
	if !HealthUpdateIsNewer(health.CollectedAt, collectedAt, deps.ParseAppTimestamp) {
		return
	}
	observation := healthpkg.HealthObservation{
		DiskStatus: health.DiskStatus, DiskFreeKB: health.DiskFreeKB, DiskTotalKB: health.DiskTotalKB,
		DiskDetails: health.DiskDetails, AptStatus: health.AptStatus, AptDetails: health.AptDetails,
		RebootRequired: health.RebootRequired,
	}
	applied := healthpkg.ApplyHealthResults(&observation, results, healthpkg.HealthResultInterpreter{
		Status: deps.HealthStatusFromResult, DiskFree: deps.DiskFreeKBFromOutput,
		DiskFreeTotal: deps.DiskFreeTotalKBFromOutput, RebootRequired: deps.RebootResultRequiresRestart,
	})
	health.DiskStatus, health.DiskFreeKB, health.DiskTotalKB = observation.DiskStatus, observation.DiskFreeKB, observation.DiskTotalKB
	health.DiskDetails, health.AptStatus, health.AptDetails = observation.DiskDetails, observation.AptStatus, observation.AptDetails
	health.RebootRequired = observation.RebootRequired
	if applied {
		health.Source = source
		health.CollectedAt = collectedAt
	}
}

func HealthUpdateIsNewer(currentAt, candidateAt string, parse func(string) (time.Time, error)) bool {
	candidateAt = strings.TrimSpace(candidateAt)
	if candidateAt == "" {
		return false
	}
	currentAt = strings.TrimSpace(currentAt)
	if currentAt == "" {
		return true
	}
	if parse == nil {
		parse = func(raw string) (time.Time, error) { return time.Parse(time.RFC3339, raw) }
	}
	candidate, err := parse(candidateAt)
	if err != nil {
		return false
	}
	current, err := parse(currentAt)
	if err != nil {
		return true
	}
	return candidate.After(current)
}

func PrecheckResultsFromMeta(meta map[string]any, key string) []updates.PrecheckResult {
	raw, ok := meta[key]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var results []updates.PrecheckResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil
	}
	return results
}

func DashboardRiskFromStatus(status *servers.ServerStatus) DashboardRiskInfo {
	risk := DashboardRiskInfo{Level: "unknown", Summary: "No package data", CVEs: []string{}}
	if status == nil {
		return risk
	}
	pending := status.PendingUpdates
	risk.PendingPackages = len(pending)
	if risk.PendingPackages == 0 && len(status.Upgradable) > 0 {
		risk.PendingPackages = len(status.Upgradable)
	}
	seenCVEs := map[string]struct{}{}
	for _, update := range pending {
		if update.Security {
			risk.SecurityUpdates++
		}
		for _, finding := range update.CVEFindings {
			cve := strings.TrimSpace(finding.ID)
			if cve == "" {
				continue
			}
			if _, ok := seenCVEs[cve]; ok {
				continue
			}
			seenCVEs[cve] = struct{}{}
			risk.CVEs = append(risk.CVEs, cve)
		}
	}
	sort.Strings(risk.CVEs)
	switch {
	case len(risk.CVEs) > 0:
		risk.Level = "critical"
		risk.Summary = fmt.Sprintf("%d CVE", len(risk.CVEs))
	case risk.SecurityUpdates > 0:
		risk.Level = "elevated"
		risk.Summary = fmt.Sprintf("%d security", risk.SecurityUpdates)
	case risk.PendingPackages > 0:
		risk.Level = "normal"
		risk.Summary = fmt.Sprintf("%d package", risk.PendingPackages)
	default:
		risk.Level = "normal"
		risk.Summary = "No CVE exposure"
	}
	return risk
}

func dashboardScheduleInfoFromPolicy(next policies.ProjectedScheduleRun, deps ServiceDeps, loc *time.Location, timezoneName string) DashboardScheduleInfo {
	if strings.TrimSpace(next.State) == "" && strings.TrimSpace(next.ScheduledForUTC) == "" {
		return defaultScheduleInfo()
	}
	display, _ := deps.FormatTimestamp(next.ScheduledForUTC, loc, timezoneName)
	return DashboardScheduleInfo{
		State:               next.State,
		PolicyName:          next.PolicyName,
		ScheduledForUTC:     next.ScheduledForUTC,
		ScheduledForDisplay: display,
		Status:              next.Status,
		Reason:              next.Reason,
		Summary:             next.Summary,
	}
}

func dashboardNoRunInfoFromPolicy(noRun policies.NoRunWindow, timezoneName string) DashboardNoRunInfo {
	if !noRun.Active {
		return DashboardNoRunInfo{Active: false, Summary: "No no-run window active", Timezone: timezoneName}
	}
	switch noRun.Scope {
	case policies.NoRunScopeGlobal:
		return DashboardNoRunInfo{Active: true, Scope: noRun.Scope, Summary: "Global no-run window active", Timezone: timezoneName}
	case policies.NoRunScopePolicy:
		summary := "Policy no-run window active"
		if strings.TrimSpace(noRun.PolicyName) != "" {
			summary = fmt.Sprintf("%s no-run window active", noRun.PolicyName)
		}
		return DashboardNoRunInfo{Active: true, Scope: noRun.Scope, Summary: summary, Timezone: timezoneName}
	default:
		return DashboardNoRunInfo{Active: true, Scope: noRun.Scope, Summary: "No-run window active", Timezone: timezoneName}
	}
}

func defaultScheduleInfo() DashboardScheduleInfo {
	return DashboardScheduleInfo{State: "none", Summary: "No scheduled run"}
}

var dashboardTimelinePhases = []struct {
	key      string
	label    string
	progress int
}{
	{key: "prechecks", label: "Pre-checks", progress: 32},
	{key: "apt_update", label: "APT update", progress: 52},
	{key: "pending_approval", label: "Pending approval", progress: 60},
	{key: "upgrade", label: "Upgrade", progress: 72},
	{key: "postchecks", label: "Post-checks", progress: 88},
	{key: "done_error", label: "Done / Error", progress: 100},
}

func timelinePhaseIndex(key string) int {
	for i, phase := range dashboardTimelinePhases {
		if phase.key == key {
			return i
		}
	}
	return -1
}

func timelinePhaseLabel(key string) string {
	if index := timelinePhaseIndex(key); index >= 0 {
		return dashboardTimelinePhases[index].label
	}
	return "Idle"
}

func timelinePhaseProgress(key string) int {
	if index := timelinePhaseIndex(key); index >= 0 {
		return dashboardTimelinePhases[index].progress
	}
	return 0
}

func activeTimelineState(state string) bool {
	return runtimepkg.ActiveTimelineState(state)
}

func statusBlocksTransientAction(status string) bool {
	return runtimepkg.BlocksTransientAction(status)
}

func runningTimelineState(state string) bool {
	return runtimepkg.RunningTimelineState(state)
}

func terminalTimelineState(state string) bool {
	return runtimepkg.TerminalTimelineState(state)
}

func timelinePhaseFromServerStatus(status string) (string, string) {
	projection := runtimepkg.TimelineProjectionFromServerStatus(status)
	return projection.CurrentPhase, projection.State
}

func timelineStateFromJob(job jobs.Record) (string, string) {
	projection := runtimepkg.TimelineProjectionFromJob(job)
	return projection.CurrentPhase, projection.State
}

func dashboardTimelineJobForStatus(status *servers.ServerStatus, job *jobs.Record) *jobs.Record {
	statusValue := ""
	if status == nil {
		return runtimepkg.DashboardTimelineJobForStatus(statusValue, job)
	}
	if strings.TrimSpace(status.JobID) != "" && (job == nil || strings.TrimSpace(job.ID) != strings.TrimSpace(status.JobID)) {
		return nil
	}
	statusValue = status.Status
	return runtimepkg.DashboardTimelineJobForStatus(statusValue, job)
}

func dashboardTimelineSourceFor(status *servers.ServerStatus, job *jobs.Record) dashboardTimelineSourceFacts {
	selectedJob := dashboardTimelineJobForStatus(status, job)
	if selectedJob != nil && strings.TrimSpace(selectedJob.ID) != "" {
		currentPhase, state := timelineStateFromJob(*selectedJob)
		summary := strings.TrimSpace(selectedJob.Summary)
		if summary == "" {
			summary = fmt.Sprintf("Update job %s", strings.TrimSpace(selectedJob.Status))
		}
		updatedAt := strings.TrimSpace(selectedJob.UpdatedAt)
		if updatedAt == "" {
			updatedAt = strings.TrimSpace(selectedJob.CreatedAt)
		}
		return dashboardTimelineSourceFacts{
			currentPhase: currentPhase,
			state:        state,
			summary:      summary,
			startedAt:    strings.TrimSpace(selectedJob.StartedAt),
			updatedAt:    updatedAt,
		}
	}
	if status == nil {
		return dashboardTimelineSourceFacts{state: "idle"}
	}
	currentPhase, state := timelinePhaseFromServerStatus(status.Status)
	summary := "No maintenance activity"
	if state != "idle" {
		summary = fmt.Sprintf("Runtime status: %s", statusLabelText(status.Status))
	}
	return dashboardTimelineSourceFacts{currentPhase: currentPhase, state: state, summary: summary}
}

func formatDashboardTimestamp(raw string, deps ServiceDeps, loc *time.Location, timezoneName string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	display, _ := deps.FormatTimestamp(raw, loc, timezoneName)
	return display
}

func buildDashboardTimeline(source dashboardTimelineSourceFacts, deps ServiceDeps, loc *time.Location, timezoneName string) DashboardTimelineInfo {
	deps = deps.withDefaults()
	currentPhase := source.currentPhase
	state := source.state
	if strings.TrimSpace(state) == "" {
		state = "idle"
	}
	summary := source.summary
	startedAt := source.startedAt
	updatedAt := source.updatedAt
	currentLabel := timelinePhaseLabel(currentPhase)
	progress := timelinePhaseProgress(currentPhase)
	if currentPhase == "" || state == "idle" {
		currentLabel = "Idle"
		progress = 0
	}
	if terminalTimelineState(state) {
		progress = 100
	}
	if strings.TrimSpace(summary) == "" {
		summary = "No maintenance activity"
	}

	currentIndex := timelinePhaseIndex(currentPhase)
	phases := make([]DashboardTimelinePhase, 0, len(dashboardTimelinePhases))
	for index, phase := range dashboardTimelinePhases {
		phaseState := "pending"
		switch {
		case currentIndex < 0:
			phaseState = "pending"
		case state == "done":
			phaseState = "done"
		case state == "error":
			if index < currentIndex {
				phaseState = "done"
			} else if index == currentIndex {
				phaseState = "error"
			}
		default:
			if index < currentIndex {
				phaseState = "done"
			} else if index == currentIndex {
				phaseState = state
			}
		}
		phaseSummary := ""
		phaseUpdatedAt := ""
		phaseUpdatedDisplay := ""
		if index == currentIndex {
			phaseSummary = summary
			phaseUpdatedAt = updatedAt
			phaseUpdatedDisplay = formatDashboardTimestamp(updatedAt, deps, loc, timezoneName)
		}
		phases = append(phases, DashboardTimelinePhase{
			Key:              phase.key,
			Label:            phase.label,
			State:            phaseState,
			ProgressPct:      phase.progress,
			Summary:          phaseSummary,
			UpdatedAt:        phaseUpdatedAt,
			UpdatedAtDisplay: phaseUpdatedDisplay,
		})
	}
	return DashboardTimelineInfo{
		CurrentPhase:     currentPhase,
		CurrentLabel:     currentLabel,
		State:            state,
		ProgressPct:      progress,
		Summary:          summary,
		StartedAt:        startedAt,
		StartedAtDisplay: formatDashboardTimestamp(startedAt, deps, loc, timezoneName),
		UpdatedAt:        updatedAt,
		UpdatedAtDisplay: formatDashboardTimestamp(updatedAt, deps, loc, timezoneName),
		Phases:           phases,
	}
}

func statusLabelText(value string) string {
	return strings.ReplaceAll(strings.TrimSpace(value), "_", " ")
}

func dashboardRiskOrder(level string) int {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "critical":
		return 4
	case "elevated":
		return 3
	case "normal":
		return 2
	default:
		return 1
	}
}

func factsFreshnessState(health DashboardHealthInfo, now time.Time, deps ServiceDeps) string {
	if strings.TrimSpace(health.Source) == "" || strings.EqualFold(strings.TrimSpace(health.Source), "unknown") || strings.TrimSpace(health.CollectedAt) == "" {
		return "stale"
	}
	collected, err := deps.ParseAppTimestamp(health.CollectedAt)
	if err != nil {
		return "stale"
	}
	if now.UTC().Sub(collected.UTC()) > 24*time.Hour {
		return "stale"
	}
	return "fresh"
}

const (
	dashboardActionUpdate                  = "update"
	dashboardActionApproveAll              = "approve_all"
	dashboardActionApproveSecurity         = "approve_security"
	dashboardActionApproveSecurityKeptBack = "approve_security_kept_back"
	dashboardActionApproveFull             = "approve_full"
	dashboardActionCancel                  = "cancel"
	dashboardActionAutoremove              = "autoremove"
	dashboardActionRefreshFacts            = "refresh_facts"
	dashboardActionEnableApt               = "enable_apt"
	dashboardActionDisableApt              = "disable_apt"
	dashboardActionRepairApt               = "repair_apt"
	dashboardActionReboot                  = "reboot"

	dashboardActionReadinessReady       = "ready"
	dashboardActionReadinessBlocked     = "blocked"
	dashboardActionReadinessUnavailable = "unavailable"
	dashboardActionReadinessInProgress  = "in_progress"
)

func buildDashboardActions(serverName string, status *servers.ServerStatus, timeline DashboardTimelineInfo, triage DashboardApprovalTriageInfo, health DashboardHealthInfo, readiness servers.MaintenanceReadiness) map[string]DashboardActionInfo {
	if strings.TrimSpace(readiness.Code) == "" {
		readiness = servers.MaintenanceReadiness{Ready: true, Code: servers.MaintenanceReadinessReady}
	}
	statusValue := ""
	plan := servers.UpgradePlan{}
	if status != nil {
		statusValue = strings.ToLower(strings.TrimSpace(status.Status))
		plan = status.UpgradePlan
	}
	fullUpgradeCount := plan.FullUpgradePackageCount
	if fullUpgradeCount <= 0 {
		fullUpgradeCount = triage.PendingPackages
	}
	fullUpgradeCandidates := triage.KeptBackPackages + len(plan.FullUpgradeNewPackages) + len(plan.FullUpgradeRemovedPackages)
	if fullUpgradeCount > fullUpgradeCandidates {
		fullUpgradeCandidates = fullUpgradeCount
	}
	actions := map[string]DashboardActionInfo{
		dashboardActionUpdate:                  dashboardPackageMutationAction(serverName, statusValue, timeline, "Ready to start update checks.", "update checks"),
		dashboardActionApproveAll:              dashboardApprovalAction(statusValue, triage.CanApproveAll, triage.StandardPackages, map[string]int{"updates": triage.StandardPackages}, "standard updates", "No standard updates eligible"),
		dashboardActionApproveSecurity:         dashboardApprovalAction(statusValue, triage.CanApproveSecurity, triage.StandardSecurityUpdates, map[string]int{"security_updates": triage.StandardSecurityUpdates}, "standard security updates", "No standard security updates eligible"),
		dashboardActionApproveSecurityKeptBack: dashboardKeptBackSecurityApprovalAction(statusValue, triage.CanApproveKeptBackSecurity, triage.KeptBackSecurityUpdates, plan),
		dashboardActionApproveFull:             dashboardFullApprovalAction(statusValue, triage.CanApproveFull, fullUpgradeCandidates, fullUpgradeCount, triage.KeptBackPackages, plan),
		dashboardActionCancel:                  dashboardCancelAction(statusValue),
		dashboardActionAutoremove:              dashboardPackageMutationAction(serverName, statusValue, timeline, "Ready to run apt autoremove.", "autoremove"),
		dashboardActionRefreshFacts:            dashboardTransientAction(serverName, statusValue, timeline, "Ready to refresh host facts.", "facts refresh"),
		dashboardActionEnableApt:               dashboardTransientAction(serverName, statusValue, timeline, "Ready to enable passwordless apt.", "passwordless apt change"),
		dashboardActionDisableApt:              dashboardTransientAction(serverName, statusValue, timeline, "Ready to disable passwordless apt.", "passwordless apt change"),
		dashboardActionRepairApt:               dashboardAptRepairAction(serverName, statusValue),
		dashboardActionReboot:                  dashboardRebootAction(serverName, statusValue, timeline, health),
	}
	if !readiness.Ready {
		for _, key := range []string{
			dashboardActionUpdate,
			dashboardActionAutoremove,
			dashboardActionRefreshFacts,
			dashboardActionEnableApt,
			dashboardActionDisableApt,
			dashboardActionRepairApt,
			dashboardActionReboot,
		} {
			action := actions[key]
			if action.Enabled {
				actions[key] = dashboardBlockedAction(dashboardActionReadinessUnavailable, readiness.Message, readiness.Code)
			}
		}
	}
	return actions
}

func dashboardRebootAction(serverName, statusValue string, timeline DashboardTimelineInfo, health DashboardHealthInfo) DashboardActionInfo {
	if strings.TrimSpace(serverName) == "" {
		return dashboardUnavailableAction("Server identity is unavailable", "")
	}
	if health.RebootRequired == nil || !*health.RebootRequired {
		return dashboardUnavailableAction("Host facts do not report a required reboot", statusValue)
	}
	if statusValue == runtimepkg.StatusNeedsReconciliation {
		return dashboardBlockedAction(dashboardActionReadinessBlocked, "Repair APT/DPKG state before rebooting", statusValue)
	}
	return dashboardTransientAction(serverName, statusValue, timeline, "Ready for a controlled reboot and verification.", "controlled reboot")
}

func dashboardAptRepairAction(serverName, statusValue string) DashboardActionInfo {
	if strings.TrimSpace(serverName) == "" {
		return dashboardUnavailableAction("Server identity is unavailable", "")
	}
	if statusValue == runtimepkg.StatusNeedsReconciliation {
		return dashboardReadyAction("Ready to inspect and repair APT/DPKG state.", nil)
	}
	if runtimepkg.StatusInProgress(statusValue) {
		return dashboardBlockedAction(dashboardActionReadinessInProgress, "Another maintenance action is active", statusValue)
	}
	return dashboardUnavailableAction("APT repair is only available after an uncertain or failed package operation", statusValue)
}

func dashboardPackageMutationAction(serverName, statusValue string, timeline DashboardTimelineInfo, readyReason, actionLabel string) DashboardActionInfo {
	if statusValue == runtimepkg.StatusNeedsReconciliation {
		return dashboardBlockedAction(dashboardActionReadinessBlocked, "APT reconciliation is required before another package mutation", statusValue)
	}
	return dashboardTransientAction(serverName, statusValue, timeline, readyReason, actionLabel)
}

func buildDashboardRecommendedAction(status *servers.ServerStatus, timeline DashboardTimelineInfo, health DashboardHealthInfo, triageTime dashboardTriageTimeFacts, failure dashboardMaintenanceFailureFacts, actions map[string]DashboardActionInfo) DashboardRecommendedActionInfo {
	statusValue := ""
	if status != nil {
		statusValue = strings.ToLower(strings.TrimSpace(status.Status))
	}
	monitoringStatus := statusValue == runtimepkg.StatusUpdating || statusValue == runtimepkg.StatusUpgrading || statusValue == runtimepkg.StatusAutoremove || statusValue == runtimepkg.StatusRepairing || statusValue == runtimepkg.StatusRebooting || statusValue == runtimepkg.StatusSudoers || statusValue == runtimepkg.StatusFactsRefresh
	if monitoringStatus || runningTimelineState(timeline.State) {
		return DashboardRecommendedActionInfo{Key: "monitor_apt", Label: "Monitor APT", Detail: "A maintenance command is still active. Follow its live output before taking another action."}
	}
	if statusValue == runtimepkg.StatusNeedsReconciliation {
		return DashboardRecommendedActionInfo{Key: "repair_package_state", Label: "Repair package state", Detail: "The last APT outcome was uncertain. Inspect locks, finish pending dpkg configuration, and verify package health before retrying.", Action: dashboardActionRepairApt}
	}
	if statusValue == runtimepkg.StatusPendingApproval {
		return DashboardRecommendedActionInfo{Key: "review_approval", Label: "Review approval", Detail: "Review the package plan and approve the safest eligible scope, or cancel the pending run."}
	}
	if statusValue == runtimepkg.StatusError && strings.EqualFold(strings.TrimSpace(failure.errorClass), "sudo_policy") {
		return dashboardRecommendationForFailure(failure, actions)
	}
	if triageTime.factsState != "fresh" || dashboardHealthFactsIncomplete(health) {
		action := ""
		if dashboardActionEnabled(actions, dashboardActionRefreshFacts) {
			action = dashboardActionRefreshFacts
		}
		return DashboardRecommendedActionInfo{Key: "refresh_host_facts", Label: "Refresh host facts", Detail: "Health facts are missing, incomplete, or older than 24 hours. Refresh them before choosing a maintenance action.", Action: action}
	}
	diskStatus := strings.ToLower(strings.TrimSpace(health.DiskStatus))
	if diskStatus != "ok" {
		return DashboardRecommendedActionInfo{Key: "review_disk_capacity", Label: "Review disk capacity", Detail: "Disk capacity checks are not passing. Review the planned upgrade space requirements and free capacity before continuing."}
	}
	if statusValue == runtimepkg.StatusError {
		return dashboardRecommendationForFailure(failure, actions)
	}
	aptStatus := strings.ToLower(strings.TrimSpace(health.AptStatus))
	if aptStatus != "ok" {
		action := ""
		if dashboardActionEnabled(actions, dashboardActionRepairApt) {
			action = dashboardActionRepairApt
		}
		return DashboardRecommendedActionInfo{Key: "repair_package_state", Label: "Repair package state", Detail: "APT health checks are not passing. Inspect package-manager state before the next upgrade.", Action: action}
	}
	if health.RebootRequired != nil && *health.RebootRequired {
		action := ""
		if dashboardActionEnabled(actions, dashboardActionReboot) {
			action = dashboardActionReboot
		}
		return DashboardRecommendedActionInfo{Key: "reboot_and_verify", Label: "Reboot and verify", Detail: "A reboot is required to activate installed updates. The controlled workflow verifies SSH recovery, uptime reset, and reboot-required state.", Action: action}
	}
	return DashboardRecommendedActionInfo{Key: "healthy", Label: "Healthy", Detail: "No immediate maintenance action is required."}
}

func dashboardHealthFactsIncomplete(health DashboardHealthInfo) bool {
	for _, value := range []string{health.DiskStatus, health.AptStatus} {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "unknown" {
			return true
		}
	}
	return false
}

func dashboardRecommendationForFailure(failure dashboardMaintenanceFailureFacts, actions map[string]DashboardActionInfo) DashboardRecommendedActionInfo {
	cause := strings.ToLower(strings.TrimSpace(failure.cause))
	errorClass := strings.ToLower(strings.TrimSpace(failure.errorClass))
	if errorClass == "sudo_policy" {
		action := ""
		if dashboardActionEnabled(actions, dashboardActionEnableApt) {
			action = dashboardActionEnableApt
		}
		return DashboardRecommendedActionInfo{Key: "enable_apt_access", Label: "Re-enable APT access", Detail: "The host rejected the managed passwordless sudo command. Run Enable apt with this host's sudo password, then retry the update.", Action: action}
	}
	if cause == "precheck:disk_space" || cause == "precheck:disk_space_plan" {
		return DashboardRecommendedActionInfo{Key: "review_disk_capacity", Label: "Review disk capacity", Detail: "The update was blocked by its disk-space plan. Review required and available capacity before retrying."}
	}
	aptFailure := cause == "precheck:apt_health" || cause == "precheck:apt_locks" || cause == "postcheck:post_apt_health" || errorClass == "reconciliation_required"
	if aptFailure {
		action := ""
		if dashboardActionEnabled(actions, dashboardActionRepairApt) {
			action = dashboardActionRepairApt
		}
		return DashboardRecommendedActionInfo{Key: "repair_package_state", Label: "Repair package state", Detail: "Typed failure evidence points to APT/DPKG state. Inspect package-manager state and use the repair workflow when it is eligible.", Action: action}
	}
	return DashboardRecommendedActionInfo{Key: "review_failure", Label: "Review failure", Detail: "Review the latest job logs and typed failure details before deciding whether to retry or repair."}
}

func dashboardTransientAction(serverName, statusValue string, timeline DashboardTimelineInfo, readyReason, actionLabel string) DashboardActionInfo {
	if strings.TrimSpace(serverName) == "" {
		return dashboardUnavailableAction("Server identity is unavailable", "")
	}
	if statusBlocksTransientAction(statusValue) {
		return dashboardBlockedAction(dashboardActionReadinessInProgress, fmt.Sprintf("Current status %s blocks %s", statusLabelText(statusValue), actionLabel), statusValue)
	}
	if activeTimelineState(timeline.State) {
		blockingStatus := strings.TrimSpace(timeline.State)
		return dashboardBlockedAction(dashboardActionReadinessInProgress, fmt.Sprintf("Timeline state %s blocks %s", statusLabelText(blockingStatus), actionLabel), blockingStatus)
	}
	return dashboardReadyAction(readyReason, nil)
}

func dashboardApprovalAction(statusValue string, enabled bool, count int, counts map[string]int, label, noUpdatesReason string) DashboardActionInfo {
	if enabled {
		return dashboardReadyAction(fmt.Sprintf("%d %s ready for approval.", count, label), counts)
	}
	if statusValue != runtimepkg.StatusPendingApproval {
		return dashboardUnavailableAction("Not waiting for approval", statusValue)
	}
	if count <= 0 {
		return dashboardUnavailableAction(noUpdatesReason, statusValue)
	}
	return dashboardBlockedAction(dashboardActionReadinessBlocked, fmt.Sprintf("Cannot approve %s now", label), statusValue)
}

func dashboardKeptBackSecurityApprovalAction(statusValue string, enabled bool, count int, plan servers.UpgradePlan) DashboardActionInfo {
	counts := map[string]int{"kept_back_security_updates": count}
	if enabled {
		return dashboardReadyAction(fmt.Sprintf("%d kept-back security updates ready for targeted approval.", count), counts)
	}
	if statusValue != runtimepkg.StatusPendingApproval {
		return dashboardUnavailableAction("Not waiting for approval", statusValue)
	}
	if count <= 0 {
		return dashboardUnavailableAction("No kept-back security updates eligible", statusValue)
	}
	if !plan.KeptBackSecurityPlanAvailable {
		return dashboardBlockedAction(dashboardActionReadinessBlocked, "Needs a fresh package scan", statusValue)
	}
	return dashboardBlockedAction(dashboardActionReadinessBlocked, "Cannot approve kept-back security updates now", statusValue)
}

func dashboardFullApprovalAction(statusValue string, enabled bool, candidateCount, fullUpgradeCount, keptBackCount int, plan servers.UpgradePlan) DashboardActionInfo {
	counts := map[string]int{
		"updates":            fullUpgradeCount,
		"kept_back_packages": keptBackCount,
		"new_packages":       len(plan.FullUpgradeNewPackages),
		"removed_packages":   len(plan.FullUpgradeRemovedPackages),
	}
	if enabled {
		return dashboardReadyAction(fmt.Sprintf("%d full-upgrade changes ready for approval.", candidateCount), counts)
	}
	if statusValue != runtimepkg.StatusPendingApproval {
		return dashboardUnavailableAction("Not waiting for approval", statusValue)
	}
	if candidateCount <= 0 {
		return dashboardUnavailableAction("No full-upgrade changes eligible", statusValue)
	}
	if !plan.FullUpgradePlanAvailable {
		return dashboardBlockedAction(dashboardActionReadinessBlocked, "Needs a fresh package scan", statusValue)
	}
	return dashboardBlockedAction(dashboardActionReadinessBlocked, "Cannot approve full upgrade now", statusValue)
}

func dashboardCancelAction(statusValue string) DashboardActionInfo {
	if statusValue == runtimepkg.StatusPendingApproval {
		return dashboardReadyAction("Ready to cancel pending approval.", nil)
	}
	return dashboardUnavailableAction("Not waiting for approval", statusValue)
}

func dashboardReadyAction(reason string, counts map[string]int) DashboardActionInfo {
	return DashboardActionInfo{
		Enabled:   true,
		Reason:    reason,
		Readiness: dashboardActionReadinessReady,
		Counts:    counts,
	}
}

func dashboardUnavailableAction(reason, blockingStatus string) DashboardActionInfo {
	return DashboardActionInfo{
		Enabled:        false,
		Reason:         reason,
		Readiness:      dashboardActionReadinessUnavailable,
		BlockingStatus: strings.TrimSpace(blockingStatus),
	}
}

func dashboardBlockedAction(readiness, reason, blockingStatus string) DashboardActionInfo {
	return DashboardActionInfo{
		Enabled:        false,
		Reason:         reason,
		Readiness:      readiness,
		BlockingStatus: strings.TrimSpace(blockingStatus),
	}
}

func mirrorApprovalTriageActions(triage DashboardApprovalTriageInfo, actions map[string]DashboardActionInfo) DashboardApprovalTriageInfo {
	triage.CanApproveAll = dashboardActionEnabled(actions, dashboardActionApproveAll)
	triage.CanApproveSecurity = dashboardActionEnabled(actions, dashboardActionApproveSecurity)
	triage.CanApproveKeptBackSecurity = dashboardActionEnabled(actions, dashboardActionApproveSecurityKeptBack)
	triage.CanApproveFull = dashboardActionEnabled(actions, dashboardActionApproveFull)
	triage.CanCancel = dashboardActionEnabled(actions, dashboardActionCancel)
	triage.CanRefreshFacts = dashboardActionEnabled(actions, dashboardActionRefreshFacts)
	triage.CanRunChecks = dashboardActionEnabled(actions, dashboardActionUpdate)
	return triage
}

func dashboardActionEnabled(actions map[string]DashboardActionInfo, key string) bool {
	action, ok := actions[key]
	return ok && action.Enabled
}

func buildApprovalTriage(status *servers.ServerStatus, health DashboardHealthInfo, risk DashboardRiskInfo, timeline DashboardTimelineInfo, timeFacts dashboardTriageTimeFacts) DashboardApprovalTriageInfo {
	statusValue := ""
	if status != nil {
		statusValue = strings.ToLower(strings.TrimSpace(status.Status))
	}
	eligible := statusValue == "pending_approval" || risk.PendingPackages > 0 || risk.SecurityUpdates > 0 || len(risk.CVEs) > 0
	canActOnApproval := statusValue == "pending_approval"
	standardPackages := risk.PendingPackages
	keptBackPackages := 0
	standardSecurityUpdates := risk.SecurityUpdates
	keptBackSecurityUpdates := 0
	if status != nil {
		plan := status.UpgradePlan
		if plan.StandardPackageCount > 0 || plan.KeptBackPackageCount > 0 || plan.FullUpgradePackageCount > 0 {
			standardPackages = plan.StandardPackageCount
			keptBackPackages = plan.KeptBackPackageCount
			standardSecurityUpdates = plan.StandardSecurityCount
			keptBackSecurityUpdates = plan.TotalSecurityCount - plan.StandardSecurityCount
			if keptBackSecurityUpdates < 0 {
				keptBackSecurityUpdates = 0
			}
		} else if canActOnApproval {
			for _, update := range status.PendingUpdates {
				if update.RequiresFull || update.KeptBack {
					keptBackPackages++
				}
				if update.Security && (update.RequiresFull || update.KeptBack) {
					keptBackSecurityUpdates++
				}
			}
			standardSecurityUpdates -= keptBackSecurityUpdates
			if standardSecurityUpdates < 0 {
				standardSecurityUpdates = 0
			}
			standardPackages -= keptBackPackages
			if standardPackages < 0 {
				standardPackages = 0
			}
		}
	}
	canRunTransientAction := !activeTimelineState(timeline.State) && !statusBlocksTransientAction(statusValue)
	availability := updates.ApprovalAvailability(status)
	return DashboardApprovalTriageInfo{
		Eligible:                   eligible,
		PendingPackages:            risk.PendingPackages,
		SecurityUpdates:            risk.SecurityUpdates,
		StandardPackages:           standardPackages,
		KeptBackPackages:           keptBackPackages,
		StandardSecurityUpdates:    standardSecurityUpdates,
		KeptBackSecurityUpdates:    keptBackSecurityUpdates,
		CVECount:                   len(risk.CVEs),
		RiskLevel:                  risk.Level,
		RiskLabel:                  risk.Summary,
		RiskOrder:                  dashboardRiskOrder(risk.Level),
		FactsState:                 timeFacts.factsState,
		FactsCollectedAt:           health.CollectedAt,
		FactsCollectedAtDisplay:    timeFacts.factsCollectedAtDisplay,
		LastCheckAt:                timeFacts.lastCheckAt,
		LastCheckDisplay:           timeFacts.lastCheckDisplay,
		CanApproveAll:              availability.CanApproveAll,
		CanApproveSecurity:         availability.CanApproveSecurity,
		CanApproveKeptBackSecurity: availability.CanApproveKeptBackSecurity,
		CanApproveFull:             availability.CanApproveFull,
		CanCancel:                  canActOnApproval,
		CanRefreshFacts:            canRunTransientAction,
		CanRunChecks:               canRunTransientAction,
	}
}

func (s *Service) BuildDashboardSummary(rawWindow string, now time.Time) (DashboardSummaryResponse, error) {
	return s.BuildDashboardSummaryContext(context.Background(), rawWindow, now)
}

func (s *Service) BuildDashboardSummaryContext(ctx context.Context, rawWindow string, now time.Time) (DashboardSummaryResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return DashboardSummaryResponse{}, err
	}
	if s != nil {
		if err := s.acquireDashboardProjection(ctx); err != nil {
			return DashboardSummaryResponse{}, err
		}
		defer s.releaseDashboardProjection()
	}
	deps := s.EnsureDeps()
	collector := newDashboardProjectionCollector(deps)
	projectionInput, err := collector.CollectContext(ctx, rawWindow, now)
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	if err := ctx.Err(); err != nil {
		return DashboardSummaryResponse{}, err
	}
	projection := newDashboardProjection()
	return projection.Project(projectionInput), nil
}

func (s *Service) acquireDashboardProjection(ctx context.Context) error {
	select {
	case s.dashboardSlot <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) releaseDashboardProjection() {
	<-s.dashboardSlot
}

func healthTrendPointFromSnapshot(record updates.HealthSnapshotRecord, deps ServiceDeps, loc *time.Location, timezoneName string) HealthTrendPoint {
	display, _ := deps.FormatTimestamp(record.CapturedAt, loc, timezoneName)
	return HealthTrendPoint{
		CapturedAt:        record.CapturedAt,
		CapturedAtDisplay: display,
		Source:            record.Source,
		PackageCount:      record.PackageCount,
		SecurityCount:     record.SecurityCount,
		LastScanStatus:    record.LastScanStatus,
		LastUpdateStatus:  record.LastUpdateStatus,
		DiskStatus:        record.DiskStatus,
		DiskFreeKB:        record.DiskFreeKB,
		DiskTotalKB:       record.DiskTotalKB,
		AptStatus:         record.AptStatus,
		RebootRequired:    record.RebootRequired,
		OSPrettyName:      record.OSPrettyName,
	}
}

func trendStatusProblem(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "", "ok", "success", "none", "unknown":
		return false
	default:
		return true
	}
}

func trendFailure(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failure", "failed", "error", "cancelled", "interrupted":
		return true
	default:
		return false
	}
}

func (s *Service) BuildHealthTrends(rawWindow, serverFilter string, now time.Time) (HealthTrendResponse, error) {
	deps := s.EnsureDeps()
	window, span, err := ParseHealthTrendWindow(rawWindow)
	if err != nil {
		return HealthTrendResponse{}, err
	}
	to := now.UTC()
	from := to.Add(-span)
	loc, timezoneName := deps.CurrentTimezone()
	fromRaw := from.Format(time.RFC3339)
	toRaw := to.Format(time.RFC3339)
	retentionDays, err := deps.HostHealthObservation.RetentionDays()
	if err != nil {
		return HealthTrendResponse{}, err
	}
	response := HealthTrendResponse{
		Window:        window,
		From:          fromRaw,
		To:            toRaw,
		GeneratedAt:   toRaw,
		RetentionDays: retentionDays,
		ServerFilter:  strings.TrimSpace(serverFilter),
		Fleet:         map[string]any{},
		Servers:       []HealthTrendServerSummary{},
	}
	response.FromDisplay, _ = deps.FormatTimestamp(response.From, loc, timezoneName)
	response.ToDisplay, _ = deps.FormatTimestamp(response.To, loc, timezoneName)

	serversSnapshot, _ := deps.ServerSnapshot()
	activeServers := map[string]struct{}{}
	for _, server := range serversSnapshot {
		activeServers[server.Name] = struct{}{}
	}
	filter := strings.TrimSpace(serverFilter)
	if filter != "" {
		if _, ok := activeServers[filter]; !ok {
			response.Fleet["servers_with_samples"] = 0
			response.Fleet["samples"] = 0
			response.Fleet["update_failures"] = 0
			response.Fleet["scan_failures"] = 0
			response.Fleet["apt_problem_samples"] = 0
			response.Fleet["disk_problem_samples"] = 0
			response.Fleet["reboot_seen"] = 0
			return response, nil
		}
	}

	snapshots, err := deps.HostHealthObservation.History(fromRaw, toRaw, filter)
	if err != nil {
		return HealthTrendResponse{}, err
	}
	latestObservations, latestErr := deps.HostHealthObservation.LatestObservations(filter)
	if latestErr != nil {
		deps.Logf("observability: failed to load last host health observations: %v", latestErr)
		latestObservations = map[string]healthpkg.Snapshot{}
	}
	byServer := map[string][]HealthTrendPoint{}
	for _, snapshot := range snapshots {
		if _, ok := activeServers[snapshot.ServerName]; !ok {
			continue
		}
		point := healthTrendPointFromSnapshot(snapshot, deps, loc, timezoneName)
		byServer[snapshot.ServerName] = append(byServer[snapshot.ServerName], point)
	}

	fleetSamples := 0
	fleetUpdateFailures := 0
	fleetScanFailures := 0
	fleetAptProblems := 0
	fleetDiskProblems := 0
	fleetRebootSeen := 0
	fleetServersWithSamples := 0
	for serverName := range activeServers {
		if filter != "" && serverName != filter {
			continue
		}
		points := byServer[serverName]
		var lastObservation *HealthTrendPoint
		if snapshot, ok := latestObservations[serverName]; ok && strings.TrimSpace(snapshot.CapturedAt) != "" {
			point := healthTrendPointFromSnapshot(snapshot, deps, loc, timezoneName)
			lastObservation = &point
		}
		if len(points) == 0 {
			response.Servers = append(response.Servers, HealthTrendServerSummary{
				Name:            serverName,
				LastObservation: lastObservation,
				Points:          []HealthTrendPoint{},
			})
			continue
		}
		fleetServersWithSamples++
		summary := HealthTrendServerSummary{
			Name:    serverName,
			Samples: len(points),
			Points:  append([]HealthTrendPoint(nil), points...),
		}
		first := points[0]
		latest := points[len(points)-1]
		summary.First = &first
		summary.Latest = &latest
		summary.LastObservation = lastObservation
		summary.PackageDelta = latest.PackageCount - first.PackageCount
		summary.SecurityDelta = latest.SecurityCount - first.SecurityCount
		if latest.DiskFreeKB > 0 && first.DiskFreeKB > 0 {
			summary.DiskFreeDeltaKB = latest.DiskFreeKB - first.DiskFreeKB
		}
		for _, point := range points {
			if trendFailure(point.LastUpdateStatus) {
				summary.UpdateFailures++
			}
			if trendFailure(point.LastScanStatus) {
				summary.ScanFailures++
			}
			if trendStatusProblem(point.AptStatus) {
				summary.AptProblemSamples++
			}
			if trendStatusProblem(point.DiskStatus) {
				summary.DiskProblemSamples++
			}
			if point.RebootRequired != nil && *point.RebootRequired {
				summary.RebootSeen = true
			}
		}
		fleetSamples += summary.Samples
		fleetUpdateFailures += summary.UpdateFailures
		fleetScanFailures += summary.ScanFailures
		fleetAptProblems += summary.AptProblemSamples
		fleetDiskProblems += summary.DiskProblemSamples
		if summary.RebootSeen {
			fleetRebootSeen++
		}
		response.Servers = append(response.Servers, summary)
	}
	sort.Slice(response.Servers, func(i, j int) bool { return response.Servers[i].Name < response.Servers[j].Name })
	response.Fleet["servers_with_samples"] = fleetServersWithSamples
	response.Fleet["samples"] = fleetSamples
	response.Fleet["update_failures"] = fleetUpdateFailures
	response.Fleet["scan_failures"] = fleetScanFailures
	response.Fleet["apt_problem_samples"] = fleetAptProblems
	response.Fleet["disk_problem_samples"] = fleetDiskProblems
	response.Fleet["reboot_seen"] = fleetRebootSeen
	return response, nil
}
