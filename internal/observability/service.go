package observability

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

type cacheEntry struct {
	summary  SummaryResponse
	cachedAt time.Time
}

type Service struct {
	deps  ServiceDeps
	mu    sync.RWMutex
	cache map[string]cacheEntry
}

func NewService(deps ServiceDeps) *Service {
	deps = deps.withDefaults()
	return &Service{
		deps:  deps,
		cache: map[string]cacheEntry{},
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
	if d.LoadServerFacts == nil {
		d.LoadServerFacts = func() (map[string]updates.ServerFactsRecord, error) {
			return map[string]updates.ServerFactsRecord{}, nil
		}
	}
	if d.ListPolicies == nil {
		d.ListPolicies = func() ([]policies.Policy, error) { return []policies.Policy{}, nil }
	}
	if d.LoadOverrides == nil {
		d.LoadOverrides = func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil }
	}
	if d.LoadGlobalBlackouts == nil {
		d.LoadGlobalBlackouts = func() ([]policies.BlackoutWindow, error) { return []policies.BlackoutWindow{}, nil }
	}
	if d.ListPolicyRuns == nil {
		d.ListPolicyRuns = func(int) ([]policies.Run, error) { return []policies.Run{}, nil }
	}
	defaultPolicy := policies.NewService(policies.ServiceDeps{})
	if d.PolicyMatchesServer == nil {
		d.PolicyMatchesServer = func(policy policies.Policy, server servers.Server, overrides map[int64]map[string]bool) bool {
			return defaultPolicy.PolicyMatchesServer(policy, server, policies.MatchContext{Overrides: overrides})
		}
	}
	if d.PolicyDueAt == nil {
		d.PolicyDueAt = defaultPolicy.PolicyDueAt
	}
	if d.BlackoutApplies == nil {
		d.BlackoutApplies = defaultPolicy.BlackoutApplies
	}
	if d.ComparePolicyCandidates == nil {
		d.ComparePolicyCandidates = defaultPolicy.ComparePolicyCandidates
	}
	if d.CanonicalScheduledForUTC == nil {
		d.CanonicalScheduledForUTC = func(slotLocal time.Time) string {
			return policies.CanonicalScheduledForUTC(slotLocal, d.JobTimestampLayout, d.CurrentLocation)
		}
	}
	if d.ParseTimeLocalMinutes == nil {
		d.ParseTimeLocalMinutes = policies.ParseTimeLocalMinutes
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

func metaStringValue(meta map[string]any, key string) string {
	if meta == nil {
		return ""
	}
	raw, ok := meta[key]
	if !ok {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func metaBoolValue(meta map[string]any, key string) (bool, bool) {
	if meta == nil {
		return false, false
	}
	raw, ok := meta[key]
	if !ok {
		return false, false
	}
	switch v := raw.(type) {
	case bool:
		return v, true
	case string:
		parsed, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return false, false
		}
		return parsed, true
	default:
		return false, false
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
	if !metaValid {
		return "unknown"
	}
	if precheck := metaStringValue(meta, "precheck_failed"); precheck != "" {
		return "precheck:" + precheck
	}
	if postcheck := metaStringValue(meta, "postcheck_failed"); postcheck != "" {
		return "postcheck:" + postcheck
	}
	if retryExhausted, ok := metaBoolValue(meta, "retry_exhausted"); ok && retryExhausted {
		return "retry_exhausted"
	}
	if errClass := strings.ToLower(metaStringValue(meta, "last_error_class")); errClass != "" && errClass != "none" {
		return "error_class:" + errClass
	}
	return "unknown"
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

	db := deps.DB()
	rows, err := db.Query(
		`SELECT status, meta_json FROM audit_events
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
		var metaJSON string
		if scanErr := rows.Scan(&status, &metaJSON); scanErr != nil {
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
		summary.FailureCauses = append(summary.FailureCauses, FailureItem{Cause: cause, Count: count})
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
	for _, result := range results {
		switch result.Name {
		case "disk_space":
			health.DiskStatus = deps.HealthStatusFromResult(result)
			if parsedFreeKB, parsedTotalKB, ok := deps.DiskFreeTotalKBFromOutput(result.Output); ok {
				health.DiskFreeKB = parsedFreeKB
				health.DiskTotalKB = parsedTotalKB
			} else if parsedFreeKB, ok := deps.DiskFreeKBFromOutput(result.Output); ok {
				health.DiskFreeKB = parsedFreeKB
			}
			health.DiskDetails = result.Details
			health.Source = source
			health.CollectedAt = collectedAt
		case "apt_health", updates.PostcheckNameAptHealth:
			health.AptStatus = deps.HealthStatusFromResult(result)
			health.AptDetails = result.Details
			health.Source = source
			health.CollectedAt = collectedAt
		case updates.PostcheckNameRebootNeeded:
			if strings.TrimSpace(result.Error) != "" {
				continue
			}
			required, known := deps.RebootResultRequiresRestart(result)
			if !known {
				continue
			}
			health.RebootRequired = &required
			health.Source = source
			health.CollectedAt = collectedAt
		}
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
		for _, cve := range update.CVEs {
			cve = strings.TrimSpace(cve)
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

func (s *Service) buildNoRunInfo(server servers.Server, policyList []policies.Policy, overrides map[int64]map[string]bool, globalBlackouts []policies.BlackoutWindow, now time.Time) DashboardNoRunInfo {
	deps := s.EnsureDeps()
	loc, timezoneName := deps.CurrentTimezone()
	localNow := now.In(loc)
	if deps.BlackoutApplies(localNow, globalBlackouts) {
		return DashboardNoRunInfo{Active: true, Scope: "global", Summary: "Global no-run window active", Timezone: timezoneName}
	}
	for _, policy := range policyList {
		if !deps.PolicyMatchesServer(policy, server, overrides) {
			continue
		}
		if deps.BlackoutApplies(localNow, policy.PolicyBlackouts) {
			return DashboardNoRunInfo{Active: true, Scope: "policy", Summary: fmt.Sprintf("%s no-run window active", policy.Name), Timezone: timezoneName}
		}
	}
	return DashboardNoRunInfo{Active: false, Summary: "No no-run window active", Timezone: timezoneName}
}

type projectedPolicyRun struct {
	policy         policies.Policy
	scheduledLocal time.Time
	scheduledUTC   string
}

func (s *Service) nextPolicyOccurrenceLocal(policy policies.Policy, fromLocal time.Time, globalBlackouts []policies.BlackoutWindow) (time.Time, bool) {
	deps := s.EnsureDeps()
	minutes, err := deps.ParseTimeLocalMinutes(policy.TimeLocal)
	if err != nil {
		return time.Time{}, false
	}
	hour := minutes / 60
	minute := minutes % 60
	loc := fromLocal.Location()
	if loc == nil {
		loc = deps.CurrentLocation()
	}
	startDay := time.Date(fromLocal.Year(), fromLocal.Month(), fromLocal.Day(), 0, 0, 0, 0, loc)
	for offset := 0; offset <= 14; offset++ {
		day := startDay.AddDate(0, 0, offset)
		slot := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if slot.Before(fromLocal) {
			continue
		}
		if !deps.PolicyDueAt(policy, slot) {
			continue
		}
		if deps.BlackoutApplies(slot, globalBlackouts) || deps.BlackoutApplies(slot, policy.PolicyBlackouts) {
			continue
		}
		return slot, true
	}
	return time.Time{}, false
}

func (s *Service) projectedPolicyRunBefore(candidate, current projectedPolicyRun) bool {
	deps := s.EnsureDeps()
	if current.scheduledUTC == "" {
		return true
	}
	candidateUTC := candidate.scheduledLocal.UTC()
	currentUTC := current.scheduledLocal.UTC()
	if !candidateUTC.Equal(currentUTC) {
		return candidateUTC.Before(currentUTC)
	}
	return deps.ComparePolicyCandidates(
		policies.ScheduledCandidate{Policy: candidate.policy, ScheduledForUTC: candidate.scheduledUTC},
		policies.ScheduledCandidate{Policy: current.policy, ScheduledForUTC: current.scheduledUTC},
	)
}

func parseScheduledUTC(raw, layout string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if parsed, err := time.Parse(layout, raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, raw)
}

func (s *Service) mergeProjectedNextRun(result map[string]DashboardScheduleInfo, serverName string, projected projectedPolicyRun, loc *time.Location, timezoneName string) {
	deps := s.EnsureDeps()
	if projected.scheduledUTC == "" {
		return
	}
	if current, exists := result[serverName]; exists {
		currentTime, err := parseScheduledUTC(current.ScheduledForUTC, deps.JobTimestampLayout)
		if err == nil && !currentTime.After(projected.scheduledLocal.UTC()) {
			return
		}
	}
	display, _ := deps.FormatTimestamp(projected.scheduledUTC, loc, timezoneName)
	result[serverName] = DashboardScheduleInfo{
		State:               "scheduled",
		PolicyName:          projected.policy.Name,
		ScheduledForUTC:     projected.scheduledUTC,
		ScheduledForDisplay: display,
		Status:              "scheduled",
		Summary:             "Scheduled run pending",
	}
}

func (s *Service) buildNextRunMap(now time.Time, serversSnapshot []servers.Server, policyList []policies.Policy, overrides map[int64]map[string]bool, globalBlackouts []policies.BlackoutWindow) (map[string]DashboardScheduleInfo, error) {
	deps := s.EnsureDeps()
	runs, err := deps.ListPolicyRuns(500)
	if err != nil {
		return nil, err
	}
	loc, timezoneName := deps.CurrentTimezone()
	result := map[string]DashboardScheduleInfo{}
	cutoff := now.UTC().Truncate(time.Minute)
	for _, run := range runs {
		scheduled, err := parseScheduledUTC(run.ScheduledForUTC, deps.JobTimestampLayout)
		if err != nil || scheduled.Before(cutoff) {
			continue
		}
		current, exists := result[run.ServerName]
		if exists {
			currentTime, currentErr := parseScheduledUTC(current.ScheduledForUTC, deps.JobTimestampLayout)
			if currentErr == nil && !scheduled.Before(currentTime) {
				continue
			}
		}
		display, _ := deps.FormatTimestamp(run.ScheduledForUTC, loc, timezoneName)
		result[run.ServerName] = DashboardScheduleInfo{
			State:               "scheduled",
			PolicyName:          run.PolicyName,
			ScheduledForUTC:     run.ScheduledForUTC,
			ScheduledForDisplay: display,
			Status:              run.Status,
			Reason:              run.Reason,
			Summary:             run.Summary,
		}
	}
	localNow := now.In(loc).Truncate(time.Minute)
	projectedByServer := map[string]projectedPolicyRun{}
	for _, server := range serversSnapshot {
		for _, policy := range policyList {
			if !deps.PolicyMatchesServer(policy, server, overrides) {
				continue
			}
			slotLocal, ok := s.nextPolicyOccurrenceLocal(policy, localNow, globalBlackouts)
			if !ok {
				continue
			}
			projected := projectedPolicyRun{
				policy:         policy,
				scheduledLocal: slotLocal,
				scheduledUTC:   deps.CanonicalScheduledForUTC(slotLocal),
			}
			if s.projectedPolicyRunBefore(projected, projectedByServer[server.Name]) {
				projectedByServer[server.Name] = projected
			}
		}
	}
	for serverName, projected := range projectedByServer {
		s.mergeProjectedNextRun(result, serverName, projected, loc, timezoneName)
	}
	return result, nil
}

func defaultScheduleInfo() DashboardScheduleInfo {
	return DashboardScheduleInfo{State: "none", Summary: "No scheduled run"}
}

var dashboardTimelinePhases = []struct {
	key      string
	label    string
	progress int
}{
	{key: "pending_approval", label: "Pending approval", progress: 12},
	{key: "prechecks", label: "Pre-checks", progress: 32},
	{key: "apt_update", label: "APT update", progress: 52},
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
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "active", "queued", "waiting":
		return true
	default:
		return false
	}
}

func runningTimelineState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "active", "queued":
		return true
	default:
		return false
	}
}

func terminalTimelineState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "done", "error":
		return true
	default:
		return false
	}
}

func timelinePhaseFromJobPhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case jobs.PhaseDial, jobs.PhasePrechecks:
		return "prechecks"
	case jobs.PhaseAptUpdate:
		return "apt_update"
	case jobs.PhaseApprovalWait:
		return "pending_approval"
	case jobs.PhaseAptUpgrade, jobs.PhaseAutoremove, jobs.PhaseApply:
		return "upgrade"
	case jobs.PhasePostchecks:
		return "postchecks"
	case jobs.PhaseComplete:
		return "done_error"
	default:
		return ""
	}
}

func timelinePhaseFromServerStatus(status string) (string, string) {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "pending_approval":
		return "pending_approval", "waiting"
	case "updating":
		return "prechecks", "active"
	case "upgrading", "autoremove":
		return "upgrade", "active"
	case "done", "success", "approved":
		return "done_error", "done"
	case "error", "failure", "failed", "cancelled":
		return "done_error", "error"
	default:
		return "", "idle"
	}
}

func timelineStateFromJob(job jobs.Record) (string, string) {
	status := strings.ToLower(strings.TrimSpace(job.Status))
	phase := timelinePhaseFromJobPhase(job.Phase)
	switch status {
	case jobs.StatusSucceeded:
		return "done_error", "done"
	case jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusInterrupted:
		return "done_error", "error"
	case jobs.StatusWaitingApproval:
		return "pending_approval", "waiting"
	case jobs.StatusQueued:
		if phase == "" {
			phase = "prechecks"
		}
		return phase, "queued"
	case jobs.StatusRunning:
		if phase == "" {
			phase = "prechecks"
		}
		return phase, "active"
	default:
		if phase != "" {
			return phase, "active"
		}
		return "", "idle"
	}
}

func dashboardTimelineJobForStatus(status *servers.ServerStatus, job *jobs.Record) *jobs.Record {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return nil
	}
	if status == nil {
		return job
	}
	_, serverState := timelinePhaseFromServerStatus(status.Status)
	_, jobState := timelineStateFromJob(*job)
	if jobState == "idle" {
		return nil
	}
	if activeTimelineState(serverState) && !activeTimelineState(jobState) {
		return nil
	}
	if terminalTimelineState(serverState) && terminalTimelineState(jobState) && serverState != jobState {
		return nil
	}
	return job
}

func (s *Service) latestUpdateJobsByServer() (map[string]jobs.Record, error) {
	deps := s.EnsureDeps()
	result := map[string]jobs.Record{}
	db := deps.DB()
	if db == nil {
		return result, nil
	}
	rows, err := db.Query(
		`SELECT id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary, logs_text,
		        error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at
		   FROM jobs
		  WHERE kind = ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 1000`,
		jobs.KindUpdate,
	)
	if err != nil {
		if strings.Contains(strings.ToLower(err.Error()), "no such table") {
			return result, nil
		}
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var record jobs.Record
		if err := rows.Scan(
			&record.ID,
			&record.Kind,
			&record.ParentJobID,
			&record.ServerName,
			&record.Actor,
			&record.ClientIP,
			&record.Status,
			&record.Phase,
			&record.Summary,
			&record.LogsText,
			&record.ErrorClass,
			&record.RetryPolicyJSON,
			&record.MetaJSON,
			&record.CreatedAt,
			&record.UpdatedAt,
			&record.StartedAt,
			&record.FinishedAt,
		); err != nil {
			return nil, err
		}
		serverName := strings.TrimSpace(record.ServerName)
		if serverName == "" {
			continue
		}
		if _, exists := result[serverName]; !exists {
			result[serverName] = record
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func formatDashboardTimestamp(raw string, deps ServiceDeps, loc *time.Location, timezoneName string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	display, _ := deps.FormatTimestamp(raw, loc, timezoneName)
	return display
}

func buildDashboardTimeline(status *servers.ServerStatus, job *jobs.Record, deps ServiceDeps, loc *time.Location, timezoneName string) DashboardTimelineInfo {
	deps = deps.withDefaults()
	currentPhase := ""
	state := "idle"
	summary := "No maintenance activity"
	startedAt := ""
	updatedAt := ""
	if job != nil && strings.TrimSpace(job.ID) != "" {
		currentPhase, state = timelineStateFromJob(*job)
		summary = strings.TrimSpace(job.Summary)
		if summary == "" {
			summary = fmt.Sprintf("Update job %s", strings.TrimSpace(job.Status))
		}
		startedAt = strings.TrimSpace(job.StartedAt)
		updatedAt = strings.TrimSpace(job.UpdatedAt)
		if updatedAt == "" {
			updatedAt = strings.TrimSpace(job.CreatedAt)
		}
	} else if status != nil {
		currentPhase, state = timelinePhaseFromServerStatus(status.Status)
		if state != "idle" {
			summary = fmt.Sprintf("Runtime status: %s", statusLabelText(status.Status))
		}
	}
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

func buildApprovalTriage(status *servers.ServerStatus, health DashboardHealthInfo, risk DashboardRiskInfo, timeline DashboardTimelineInfo, lastUpdate *DashboardUpdateHistory, now time.Time, deps ServiceDeps, loc *time.Location, timezoneName string) DashboardApprovalTriageInfo {
	deps = deps.withDefaults()
	statusValue := ""
	if status != nil {
		statusValue = strings.ToLower(strings.TrimSpace(status.Status))
	}
	lastCheckAt := strings.TrimSpace(health.CollectedAt)
	if lastCheckAt == "" && lastUpdate != nil {
		lastCheckAt = strings.TrimSpace(lastUpdate.FinishedAt)
	}
	if lastCheckAt == "" {
		lastCheckAt = strings.TrimSpace(timeline.UpdatedAt)
	}
	factsState := factsFreshnessState(health, now, deps)
	eligible := statusValue == "pending_approval" || risk.PendingPackages > 0 || risk.SecurityUpdates > 0 || len(risk.CVEs) > 0
	canActOnApproval := statusValue == "pending_approval"
	return DashboardApprovalTriageInfo{
		Eligible:                eligible,
		PendingPackages:         risk.PendingPackages,
		SecurityUpdates:         risk.SecurityUpdates,
		CVECount:                len(risk.CVEs),
		RiskLevel:               risk.Level,
		RiskLabel:               risk.Summary,
		RiskOrder:               dashboardRiskOrder(risk.Level),
		FactsState:              factsState,
		FactsCollectedAt:        health.CollectedAt,
		FactsCollectedAtDisplay: formatDashboardTimestamp(health.CollectedAt, deps, loc, timezoneName),
		LastCheckAt:             lastCheckAt,
		LastCheckDisplay:        formatDashboardTimestamp(lastCheckAt, deps, loc, timezoneName),
		CanApproveAll:           canActOnApproval,
		CanApproveSecurity:      canActOnApproval && risk.SecurityUpdates > 0,
		CanCancel:               canActOnApproval,
		CanRefreshFacts:         true,
		CanRunChecks:            !activeTimelineState(timeline.State),
	}
}

func (s *Service) BuildDashboardSummary(rawWindow string, now time.Time) (DashboardSummaryResponse, error) {
	deps := s.EnsureDeps()
	window, span, err := ParseWindow(rawWindow)
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	to := now.UTC()
	from := to.Add(-span)
	response := DashboardSummaryResponse{
		Window:      window,
		From:        from.Format(time.RFC3339),
		To:          to.Format(time.RFC3339),
		GeneratedAt: to.Format(time.RFC3339),
		Fleet:       map[string]any{},
		Servers:     []DashboardServerSummary{},
	}

	serversSnapshot, statusByName := deps.ServerSnapshot()
	facts, err := deps.LoadServerFacts()
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	policyList, err := deps.ListPolicies()
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	nextRuns, err := s.buildNextRunMap(now, serversSnapshot, policyList, overrides, globalBlackouts)
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	latestUpdateJobs, err := s.latestUpdateJobsByServer()
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	loc, timezoneName := deps.CurrentTimezone()

	type updateAgg struct {
		lastSuccess *DashboardUpdateHistory
		lastFailure *DashboardUpdateHistory
		meta        map[string]any
		metaAt      string
		durationSum float64
		samples     int
	}
	updateByServer := map[string]*updateAgg{}
	rows, err := deps.DB().Query(
		`SELECT created_at, target_name, status, message, meta_json
		   FROM audit_events
		  WHERE action = ? AND target_type = 'server' AND created_at >= ? AND created_at <= ?
		  ORDER BY created_at DESC, id DESC`,
		deps.UpdateCompleteAction,
		from.Format(time.RFC3339),
		to.Format(time.RFC3339),
	)
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	for rows.Next() {
		var createdAt, targetName, status, message, metaJSON string
		if err := rows.Scan(&createdAt, &targetName, &status, &message, &metaJSON); err != nil {
			rows.Close()
			return DashboardSummaryResponse{}, err
		}
		agg := updateByServer[targetName]
		if agg == nil {
			agg = &updateAgg{}
			updateByServer[targetName] = agg
		}
		meta := map[string]any{}
		metaValid := false
		if strings.TrimSpace(metaJSON) != "" {
			if err := json.Unmarshal([]byte(metaJSON), &meta); err == nil {
				metaValid = true
			}
		}
		duration, hasDuration := MetaDurationMS(meta)
		if hasDuration {
			agg.durationSum += duration
			agg.samples++
		}
		display, _ := deps.FormatTimestamp(createdAt, loc, timezoneName)
		item := &DashboardUpdateHistory{
			Status:            strings.ToLower(strings.TrimSpace(status)),
			FinishedAt:        createdAt,
			FinishedAtDisplay: display,
			DurationMS:        duration,
			Message:           message,
		}
		if item.Status == "failure" {
			item.FailureCause = FailureCauseFromMeta(meta, metaValid)
			if agg.lastFailure == nil {
				agg.lastFailure = item
			}
		}
		if item.Status == "success" && agg.lastSuccess == nil {
			agg.lastSuccess = item
		}
		if agg.meta == nil && metaValid {
			agg.meta = meta
			agg.metaAt = createdAt
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return DashboardSummaryResponse{}, err
	}
	rows.Close()

	commandHistory := map[string][]DashboardCommandHistoryItem{}
	commandRows, err := deps.DB().Query(
		`SELECT created_at, target_name, action, status, message, actor
		   FROM audit_events
		  WHERE target_type = 'server' AND created_at >= ? AND created_at <= ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 400`,
		from.Format(time.RFC3339),
		to.Format(time.RFC3339),
	)
	if err != nil {
		return DashboardSummaryResponse{}, err
	}
	for commandRows.Next() {
		var item DashboardCommandHistoryItem
		var targetName string
		if err := commandRows.Scan(&item.CreatedAt, &targetName, &item.Action, &item.Status, &item.Message, &item.Actor); err != nil {
			commandRows.Close()
			return DashboardSummaryResponse{}, err
		}
		if len(commandHistory[targetName]) >= 8 {
			continue
		}
		item.CreatedAtDisplay, _ = deps.FormatTimestamp(item.CreatedAt, loc, timezoneName)
		commandHistory[targetName] = append(commandHistory[targetName], item)
	}
	if err := commandRows.Err(); err != nil {
		commandRows.Close()
		return DashboardSummaryResponse{}, err
	}
	commandRows.Close()

	fleetPendingApproval := 0
	fleetPrechecksRunning := 0
	fleetInProgress := 0
	fleetDone := 0
	fleetPendingPackages := 0
	fleetSecurityUpdates := 0
	fleetHighRiskCVE := 0
	fleetReboot := 0
	fleetStaleFacts := 0
	for _, server := range serversSnapshot {
		status := statusByName[server.Name]
		fact := facts[server.Name]
		health := DashboardHealthInfo{
			DiskStatus:    "unknown",
			AptStatus:     "unknown",
			OSPrettyName:  fact.OSPrettyName,
			UptimeSeconds: fact.UptimeSeconds,
			CollectedAt:   fact.CollectedAt,
			Source:        "facts",
		}
		if fact.ServerName != "" {
			health.RebootRequired = fact.RebootRequired
			health.DiskStatus = fact.DiskStatus
			health.DiskFreeKB = fact.DiskFreeKB
			health.DiskTotalKB = fact.DiskTotalKB
			health.DiskDetails = fact.DiskDetails
			health.AptStatus = fact.AptStatus
			health.AptDetails = fact.AptDetails
		} else {
			health.Source = "unknown"
		}
		agg := updateByServer[server.Name]
		if agg != nil && agg.meta != nil {
			auditResults := PrecheckResultsFromMeta(agg.meta, "precheck_results")
			auditResults = append(auditResults, PrecheckResultsFromMeta(agg.meta, "postcheck_results")...)
			UpdateHealthFromResults(&health, auditResults, "audit", agg.metaAt, deps)
		}
		if health.RebootRequired != nil && *health.RebootRequired {
			fleetReboot++
		}
		nextRun := nextRuns[server.Name]
		if nextRun.State == "" {
			nextRun = defaultScheduleInfo()
		}
		risk := DashboardRiskFromStatus(status)
		var jobPtr *jobs.Record
		if job, ok := latestUpdateJobs[server.Name]; ok {
			jobCopy := job
			jobPtr = dashboardTimelineJobForStatus(status, &jobCopy)
		}
		timeline := buildDashboardTimeline(status, jobPtr, deps, loc, timezoneName)
		var lastUpdate *DashboardUpdateHistory
		var lastFailedUpdate *DashboardUpdateHistory
		durationSamples := 0
		avgDurationMS := 0.0
		if agg != nil {
			lastUpdate = agg.lastSuccess
			lastFailedUpdate = agg.lastFailure
			durationSamples = agg.samples
			if agg.samples > 0 {
				avgDurationMS = agg.durationSum / float64(agg.samples)
			}
		}
		triage := buildApprovalTriage(status, health, risk, timeline, lastUpdate, now, deps, loc, timezoneName)
		if triage.FactsState == "stale" {
			fleetStaleFacts++
		}
		fleetPendingPackages += triage.PendingPackages
		fleetSecurityUpdates += triage.SecurityUpdates
		if triage.CVECount > 0 {
			fleetHighRiskCVE++
		}
		if triage.CanApproveAll || timeline.CurrentPhase == "pending_approval" {
			fleetPendingApproval++
		}
		if timeline.CurrentPhase == "prechecks" || timeline.State == "queued" || timeline.State == "active" {
			fleetPrechecksRunning++
		}
		if runningTimelineState(timeline.State) {
			fleetInProgress++
		}
		if timeline.State == "done" {
			fleetDone++
		}
		serverSummary := DashboardServerSummary{
			Name:             server.Name,
			LastUpdate:       lastUpdate,
			LastFailedUpdate: lastFailedUpdate,
			AvgDurationMS:    avgDurationMS,
			DurationSamples:  durationSamples,
			NextRun:          nextRun,
			NoRun:            s.buildNoRunInfo(server, policyList, overrides, globalBlackouts, now),
			Health:           health,
			Risk:             risk,
			Timeline:         timeline,
			ApprovalTriage:   triage,
			CommandHistory:   commandHistory[server.Name],
		}
		response.Servers = append(response.Servers, serverSummary)
	}
	sort.Slice(response.Servers, func(i, j int) bool { return response.Servers[i].Name < response.Servers[j].Name })
	response.Fleet["pending_approval"] = fleetPendingApproval
	response.Fleet["prechecks_running"] = fleetPrechecksRunning
	response.Fleet["in_progress"] = fleetInProgress
	response.Fleet["done"] = fleetDone
	response.Fleet["pending_packages"] = fleetPendingPackages
	response.Fleet["security_updates"] = fleetSecurityUpdates
	response.Fleet["high_risk_cve"] = fleetHighRiskCVE
	response.Fleet["hosts_needing_reboot"] = fleetReboot
	response.Fleet["stale_facts"] = fleetStaleFacts
	return response, nil
}
