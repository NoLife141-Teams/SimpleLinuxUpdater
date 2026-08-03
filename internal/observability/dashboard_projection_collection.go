package observability

import (
	"database/sql"
	"encoding/json"
	"strings"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

type dashboardProjectionCollector struct {
	deps ServiceDeps
}

type dashboardHealthOverlayFacts struct {
	accepted    bool
	collectedAt string
	results     []updates.PrecheckResult
}

type dashboardCollectedUpdateHistory struct {
	projection    dashboardUpdateHistoryProjection
	healthOverlay dashboardHealthOverlayFacts
}

const (
	dashboardCommandHistoryPerServer = 8
	dashboardCommandHistoryBatchSize = 500
	dashboardLatestJobBatchSize      = 500
)

func newDashboardProjectionCollector(deps ServiceDeps) dashboardProjectionCollector {
	return dashboardProjectionCollector{deps: deps.withDefaults()}
}

func (c dashboardProjectionCollector) Collect(rawWindow string, now time.Time) (dashboardProjectionInput, error) {
	window, span, err := ParseWindow(rawWindow)
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	to := now.UTC()
	from := to.Add(-span)
	fromFormatted := from.Format(time.RFC3339)
	toFormatted := to.Format(time.RFC3339)

	serversSnapshot, statusByName := c.deps.ServerSnapshot()
	serverNames := make([]string, 0, len(serversSnapshot))
	for _, server := range serversSnapshot {
		serverNames = append(serverNames, server.Name)
	}
	readinessByName := c.deps.MaintenanceReadiness(serversSnapshot)
	facts, err := c.deps.HostHealthObservation.Latest()
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	scheduleProjection, err := c.deps.ProjectPolicySchedule(policies.ScheduleProjectionRequest{
		Now:      now,
		Servers:  serversSnapshot,
		RunLimit: 500,
	})
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	latestUpdateJobs, err := c.collectLatestUpdateJobs(serverNames)
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	loc, timezoneName := c.deps.CurrentTimezone()
	updateByServer, err := c.collectUpdateHistory(fromFormatted, toFormatted, loc, timezoneName)
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	commandHistory, err := c.collectCommandHistory(serverNames, fromFormatted, toFormatted, loc, timezoneName)
	if err != nil {
		return dashboardProjectionInput{}, err
	}

	input := dashboardProjectionInput{
		window:      window,
		from:        fromFormatted,
		to:          toFormatted,
		generatedAt: toFormatted,
		servers:     make([]dashboardServerProjectionInput, 0, len(serversSnapshot)),
	}
	for _, server := range serversSnapshot {
		status := statusByName[server.Name]
		agg := updateByServer[server.Name]
		if agg == nil {
			agg = &dashboardCollectedUpdateHistory{}
		}
		schedule := scheduleProjection.Servers[server.Name]
		var latestUpdateJob *jobs.Record
		if job, ok := latestUpdateJobs[server.Name]; ok {
			jobCopy := job
			latestUpdateJob = &jobCopy
		}
		health := c.collectHealth(facts[server.Name], agg.healthOverlay)
		timelineSource := dashboardTimelineSourceFor(status, latestUpdateJob)
		timeline := buildDashboardTimeline(timelineSource, c.deps, loc, timezoneName)
		failure := c.collectMaintenanceFailure(status, latestUpdateJob, agg.projection.lastFailure)
		input.servers = append(input.servers, dashboardServerProjectionInput{
			server:         server,
			status:         status,
			health:         health,
			failure:        failure,
			readiness:      readinessByName[server.Name],
			nextRun:        dashboardScheduleInfoFromPolicy(schedule.NextRun, c.deps, loc, timezoneName),
			noRun:          dashboardNoRunInfoFromPolicy(schedule.NoRun, timezoneName),
			timeline:       timeline,
			triageTime:     c.collectTriageTime(health, agg.projection.lastSuccess, timeline, now, loc, timezoneName),
			updateHistory:  agg.projection,
			commandHistory: commandHistory[server.Name],
		})
	}
	return input, nil
}

func (c dashboardProjectionCollector) collectMaintenanceFailure(status *servers.ServerStatus, latestJob *jobs.Record, lastFailure *DashboardUpdateHistory) dashboardMaintenanceFailureFacts {
	selectedJob := dashboardTimelineJobForStatus(status, latestJob)
	if status == nil || strings.TrimSpace(status.JobID) == "" || !strings.EqualFold(strings.TrimSpace(status.Status), "error") || selectedJob == nil {
		return dashboardMaintenanceFailureFacts{}
	}
	failure := dashboardMaintenanceFailureFacts{errorClass: strings.ToLower(strings.TrimSpace(selectedJob.ErrorClass))}
	if lastFailure == nil || strings.TrimSpace(lastFailure.FailureCause) == "" {
		return failure
	}
	jobStarted, jobErr := c.deps.ParseAppTimestamp(selectedJob.CreatedAt)
	failureFinished, failureErr := c.deps.ParseAppTimestamp(lastFailure.FinishedAt)
	if jobErr == nil && failureErr == nil && !failureFinished.Before(jobStarted) {
		failure.cause = strings.ToLower(strings.TrimSpace(lastFailure.FailureCause))
	}
	return failure
}

func (c dashboardProjectionCollector) collectHealth(fact updates.ServerFactsRecord, overlay dashboardHealthOverlayFacts) DashboardHealthInfo {
	health := DashboardHealthInfo{
		DiskStatus:                   "unknown",
		AptStatus:                    "unknown",
		OSPrettyName:                 fact.OSPrettyName,
		RunningKernelVersion:         fact.RunningKernelVersion,
		LatestInstalledKernelVersion: fact.LatestInstalledKernelVersion,
		UptimeSeconds:                fact.UptimeSeconds,
		CollectedAt:                  fact.CollectedAt,
		Source:                       "facts",
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
	if overlay.accepted {
		UpdateHealthFromResults(&health, overlay.results, "audit", overlay.collectedAt, c.deps)
	}
	return health
}

func (c dashboardProjectionCollector) collectTriageTime(health DashboardHealthInfo, lastUpdate *DashboardUpdateHistory, timeline DashboardTimelineInfo, now time.Time, loc *time.Location, timezoneName string) dashboardTriageTimeFacts {
	lastCheckAt := strings.TrimSpace(health.CollectedAt)
	if lastCheckAt == "" && lastUpdate != nil {
		lastCheckAt = strings.TrimSpace(lastUpdate.FinishedAt)
	}
	if lastCheckAt == "" {
		lastCheckAt = strings.TrimSpace(timeline.UpdatedAt)
	}
	return dashboardTriageTimeFacts{
		factsState:              factsFreshnessState(health, now, c.deps),
		factsCollectedAtDisplay: formatDashboardTimestamp(health.CollectedAt, c.deps, loc, timezoneName),
		lastCheckAt:             lastCheckAt,
		lastCheckDisplay:        formatDashboardTimestamp(lastCheckAt, c.deps, loc, timezoneName),
	}
}

func (c dashboardProjectionCollector) collectLatestUpdateJobs(serverNames []string) (map[string]jobs.Record, error) {
	result := map[string]jobs.Record{}
	serverNames = uniqueDashboardServerNames(serverNames)
	if len(serverNames) == 0 {
		return result, nil
	}
	db := c.deps.DB()
	if db == nil {
		return result, nil
	}
	for start := 0; start < len(serverNames); start += dashboardLatestJobBatchSize {
		end := min(start+dashboardLatestJobBatchSize, len(serverNames))
		batch := serverNames[start:end]
		args := make([]any, 0, len(batch)+1)
		args = append(args, jobs.KindUpdate)
		for _, serverName := range batch {
			args = append(args, serverName)
		}
		rows, err := db.Query(latestUpdateJobsQuery(len(batch)), args...)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no such table") {
				return result, nil
			}
			return nil, err
		}
		if err := appendLatestUpdateJobRows(result, rows); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func latestUpdateJobsQuery(serverCount int) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", serverCount), ",")
	return `WITH ranked_jobs AS (
		SELECT id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary,
		       error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at,
		       ROW_NUMBER() OVER (PARTITION BY server_name ORDER BY created_at DESC, id DESC) AS job_rank
		  FROM jobs
		 WHERE kind = ? AND server_name IN (` + placeholders + `)
	)
	SELECT id, kind, parent_job_id, server_name, actor, client_ip, status, phase, summary,
	       error_class, retry_policy_json, meta_json, created_at, updated_at, started_at, finished_at
	  FROM ranked_jobs
	 WHERE job_rank = 1
	 ORDER BY server_name ASC`
}

func appendLatestUpdateJobRows(result map[string]jobs.Record, rows *sql.Rows) error {
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
			&record.ErrorClass,
			&record.RetryPolicyJSON,
			&record.MetaJSON,
			&record.CreatedAt,
			&record.UpdatedAt,
			&record.StartedAt,
			&record.FinishedAt,
		); err != nil {
			return err
		}
		result[record.ServerName] = record
	}
	return rows.Err()
}

func (c dashboardProjectionCollector) collectUpdateHistory(from, to string, loc *time.Location, timezoneName string) (map[string]*dashboardCollectedUpdateHistory, error) {
	updateByServer := map[string]*dashboardCollectedUpdateHistory{}
	rows, err := c.deps.DB().Query(
		`SELECT created_at, target_name, status, message, meta_json
		   FROM audit_events
		  WHERE action = ? AND target_type = 'server' AND created_at >= ? AND created_at <= ?
		  ORDER BY created_at DESC, id DESC`,
		c.deps.UpdateCompleteAction,
		from,
		to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var createdAt, targetName, status, message, metaJSON string
		if err := rows.Scan(&createdAt, &targetName, &status, &message, &metaJSON); err != nil {
			return nil, err
		}
		agg := updateByServer[targetName]
		if agg == nil {
			agg = &dashboardCollectedUpdateHistory{}
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
			agg.projection.durationSum += duration
			agg.projection.samples++
		}
		display, _ := c.deps.FormatTimestamp(createdAt, loc, timezoneName)
		item := &DashboardUpdateHistory{
			Status:            strings.ToLower(strings.TrimSpace(status)),
			FinishedAt:        createdAt,
			FinishedAtDisplay: display,
			DurationMS:        duration,
			Message:           message,
		}
		if item.Status == "failure" {
			item.FailureCause = FailureCauseFromMeta(meta, metaValid)
			if agg.projection.lastFailure == nil {
				agg.projection.lastFailure = item
			}
		}
		if item.Status == "success" && agg.projection.lastSuccess == nil {
			agg.projection.lastSuccess = item
		}
		if !agg.healthOverlay.accepted && metaValid {
			results := PrecheckResultsFromMeta(meta, "precheck_results")
			results = append(results, PrecheckResultsFromMeta(meta, "postcheck_results")...)
			agg.healthOverlay = dashboardHealthOverlayFacts{
				accepted:    true,
				collectedAt: createdAt,
				results:     results,
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return updateByServer, nil
}

func (c dashboardProjectionCollector) collectCommandHistory(serverNames []string, from, to string, loc *time.Location, timezoneName string) (map[string][]DashboardCommandHistoryItem, error) {
	commandHistory := map[string][]DashboardCommandHistoryItem{}
	serverNames = uniqueDashboardServerNames(serverNames)
	for start := 0; start < len(serverNames); start += dashboardCommandHistoryBatchSize {
		end := min(start+dashboardCommandHistoryBatchSize, len(serverNames))
		batch := serverNames[start:end]
		args := make([]any, 0, len(batch)+3)
		args = append(args, from, to)
		for _, serverName := range batch {
			args = append(args, serverName)
		}
		args = append(args, dashboardCommandHistoryPerServer)
		rows, err := c.deps.DB().Query(commandHistoryQuery(len(batch)), args...)
		if err != nil {
			return nil, err
		}
		if err := c.appendCommandHistoryRows(commandHistory, rows, loc, timezoneName); err != nil {
			return nil, err
		}
	}
	return commandHistory, nil
}

func commandHistoryQuery(serverCount int) string {
	placeholders := strings.TrimSuffix(strings.Repeat("?,", serverCount), ",")
	return `WITH ranked_commands AS (
		SELECT id, created_at, target_name, action, status, message, actor,
		       ROW_NUMBER() OVER (PARTITION BY target_name ORDER BY created_at DESC, id DESC) AS command_rank
		  FROM audit_events
		 WHERE target_type = 'server' AND created_at >= ? AND created_at <= ?
		   AND target_name IN (` + placeholders + `)
	)
	SELECT created_at, target_name, action, status, message, actor
	  FROM ranked_commands
	 WHERE command_rank <= ?
	 ORDER BY created_at DESC, id DESC`
}

func (c dashboardProjectionCollector) appendCommandHistoryRows(commandHistory map[string][]DashboardCommandHistoryItem, rows *sql.Rows, loc *time.Location, timezoneName string) error {
	defer rows.Close()
	for rows.Next() {
		var item DashboardCommandHistoryItem
		var targetName string
		if err := rows.Scan(&item.CreatedAt, &targetName, &item.Action, &item.Status, &item.Message, &item.Actor); err != nil {
			return err
		}
		item.CreatedAtDisplay, _ = c.deps.FormatTimestamp(item.CreatedAt, loc, timezoneName)
		commandHistory[targetName] = append(commandHistory[targetName], item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return nil
}

func uniqueDashboardServerNames(serverNames []string) []string {
	unique := make([]string, 0, len(serverNames))
	seen := make(map[string]struct{}, len(serverNames))
	for _, serverName := range serverNames {
		serverName = strings.TrimSpace(serverName)
		if serverName == "" {
			continue
		}
		if _, exists := seen[serverName]; exists {
			continue
		}
		seen[serverName] = struct{}{}
		unique = append(unique, serverName)
	}
	return unique
}
