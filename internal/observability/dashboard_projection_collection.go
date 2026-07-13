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
		window:       window,
		from:         fromFormatted,
		to:           toFormatted,
		generatedAt:  toFormatted,
		now:          now,
		loc:          loc,
		timezoneName: timezoneName,
		servers:      make([]dashboardServerProjectionInput, 0, len(serversSnapshot)),
	}
	for _, server := range serversSnapshot {
		status := statusByName[server.Name]
		agg := updateByServer[server.Name]
		if agg == nil {
			agg = &dashboardUpdateHistoryProjection{}
		}
		schedule := scheduleProjection.Servers[server.Name]
		var latestUpdateJob *jobs.Record
		if job, ok := latestUpdateJobs[server.Name]; ok {
			jobCopy := job
			latestUpdateJob = &jobCopy
		}
		input.servers = append(input.servers, dashboardServerProjectionInput{
			server:         server,
			status:         status,
			fact:           facts[server.Name],
			nextRun:        dashboardScheduleInfoFromPolicy(schedule.NextRun, c.deps, loc, timezoneName),
			noRun:          dashboardNoRunInfoFromPolicy(schedule.NoRun, timezoneName),
			timelineSource: dashboardTimelineSourceFor(status, latestUpdateJob),
			updateHistory:  *agg,
			commandHistory: commandHistory[server.Name],
		})
	}
	return input, nil
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

func (c dashboardProjectionCollector) collectUpdateHistory(from, to string, loc *time.Location, timezoneName string) (map[string]*dashboardUpdateHistoryProjection, error) {
	updateByServer := map[string]*dashboardUpdateHistoryProjection{}
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
			agg = &dashboardUpdateHistoryProjection{}
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
			if agg.lastFailure == nil {
				agg.lastFailure = item
			}
		}
		if item.Status == "success" && agg.lastSuccess == nil {
			agg.lastSuccess = item
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
