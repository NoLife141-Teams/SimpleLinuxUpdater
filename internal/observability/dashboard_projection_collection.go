package observability

import (
	"encoding/json"
	"strings"
	"time"

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
