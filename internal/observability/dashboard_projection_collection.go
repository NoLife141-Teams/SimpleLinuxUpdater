package observability

import (
	"encoding/json"
	"strings"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
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
	latestUpdateJobs, err := c.collectLatestUpdateJobs()
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	loc, timezoneName := c.deps.CurrentTimezone()
	updateByServer, err := c.collectUpdateHistory(fromFormatted, toFormatted, loc, timezoneName)
	if err != nil {
		return dashboardProjectionInput{}, err
	}
	commandHistory, err := c.collectCommandHistory(fromFormatted, toFormatted, loc, timezoneName)
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
		input.servers = append(input.servers, dashboardServerProjectionInput{
			server:         server,
			status:         status,
			health:         health,
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

func (c dashboardProjectionCollector) collectLatestUpdateJobs() (map[string]jobs.Record, error) {
	result := map[string]jobs.Record{}
	db := c.deps.DB()
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

func (c dashboardProjectionCollector) collectCommandHistory(from, to string, loc *time.Location, timezoneName string) (map[string][]DashboardCommandHistoryItem, error) {
	commandHistory := map[string][]DashboardCommandHistoryItem{}
	rows, err := c.deps.DB().Query(
		`SELECT created_at, target_name, action, status, message, actor
		   FROM audit_events
		  WHERE target_type = 'server' AND created_at >= ? AND created_at <= ?
		  ORDER BY created_at DESC, id DESC
		  LIMIT 400`,
		from,
		to,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var item DashboardCommandHistoryItem
		var targetName string
		if err := rows.Scan(&item.CreatedAt, &targetName, &item.Action, &item.Status, &item.Message, &item.Actor); err != nil {
			return nil, err
		}
		if len(commandHistory[targetName]) >= 8 {
			continue
		}
		item.CreatedAtDisplay, _ = c.deps.FormatTimestamp(item.CreatedAt, loc, timezoneName)
		commandHistory[targetName] = append(commandHistory[targetName], item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return commandHistory, nil
}
