package observability

import (
	"sort"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

type dashboardProjection struct {
	ctx dashboardProjectionContext
}

type dashboardProjectionContext struct {
	now          time.Time
	deps         ServiceDeps
	loc          *time.Location
	timezoneName string
}

type dashboardProjectionInput struct {
	window      string
	from        string
	to          string
	generatedAt string
	servers     []dashboardServerProjectionInput
}

type dashboardServerProjectionInput struct {
	server          servers.Server
	status          *servers.ServerStatus
	fact            updates.ServerFactsRecord
	nextRun         DashboardScheduleInfo
	noRun           DashboardNoRunInfo
	latestUpdateJob *jobs.Record
	updateHistory   dashboardUpdateHistoryProjection
	commandHistory  []DashboardCommandHistoryItem
}

type dashboardUpdateHistoryProjection struct {
	lastSuccess *DashboardUpdateHistory
	lastFailure *DashboardUpdateHistory
	meta        map[string]any
	metaAt      string
	durationSum float64
	samples     int
}

func newDashboardProjection(ctx dashboardProjectionContext) dashboardProjection {
	ctx.deps = ctx.deps.withDefaults()
	return dashboardProjection{ctx: ctx}
}

func (p dashboardProjection) Project(input dashboardProjectionInput) DashboardSummaryResponse {
	response := DashboardSummaryResponse{
		Window:      input.window,
		From:        input.from,
		To:          input.to,
		GeneratedAt: input.generatedAt,
		Fleet:       map[string]any{},
		Servers:     []DashboardServerSummary{},
	}

	for _, serverInput := range input.servers {
		response.Servers = append(response.Servers, p.projectServer(serverInput))
	}
	sort.Slice(response.Servers, func(i, j int) bool { return response.Servers[i].Name < response.Servers[j].Name })
	response.Fleet = dashboardFleetRollup(response.Servers)
	return response
}

func (p dashboardProjection) projectServer(input dashboardServerProjectionInput) DashboardServerSummary {
	health := p.projectHealth(input.fact, input.updateHistory)
	nextRun := input.nextRun
	if nextRun.State == "" {
		nextRun = defaultScheduleInfo()
	}
	risk := DashboardRiskFromStatus(input.status)
	job := dashboardTimelineJobForStatus(input.status, input.latestUpdateJob)
	timeline := buildDashboardTimeline(input.status, job, p.ctx.deps, p.ctx.loc, p.ctx.timezoneName)

	lastUpdate := input.updateHistory.lastSuccess
	durationSamples := input.updateHistory.samples
	avgDurationMS := 0.0
	if durationSamples > 0 {
		avgDurationMS = input.updateHistory.durationSum / float64(durationSamples)
	}
	triage := buildApprovalTriage(input.status, health, risk, timeline, lastUpdate, p.ctx.now, p.ctx.deps, p.ctx.loc, p.ctx.timezoneName)
	actions := buildDashboardActions(input.server.Name, input.status, timeline, triage)
	triage = mirrorApprovalTriageActions(triage, actions)

	return DashboardServerSummary{
		Name:             input.server.Name,
		LastUpdate:       lastUpdate,
		LastFailedUpdate: input.updateHistory.lastFailure,
		AvgDurationMS:    avgDurationMS,
		DurationSamples:  durationSamples,
		NextRun:          nextRun,
		NoRun:            input.noRun,
		Health:           health,
		Risk:             risk,
		Timeline:         timeline,
		Actions:          actions,
		ApprovalTriage:   triage,
		CommandHistory:   input.commandHistory,
	}
}

func (p dashboardProjection) projectHealth(fact updates.ServerFactsRecord, history dashboardUpdateHistoryProjection) DashboardHealthInfo {
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
	if history.meta != nil {
		auditResults := PrecheckResultsFromMeta(history.meta, "precheck_results")
		auditResults = append(auditResults, PrecheckResultsFromMeta(history.meta, "postcheck_results")...)
		UpdateHealthFromResults(&health, auditResults, "audit", history.metaAt, p.ctx.deps)
	}
	return health
}

func dashboardFleetRollup(servers []DashboardServerSummary) map[string]any {
	fleet := map[string]any{}
	fleetPendingApproval := 0
	fleetPrechecksRunning := 0
	fleetInProgress := 0
	fleetDone := 0
	fleetPendingPackages := 0
	fleetSecurityUpdates := 0
	fleetHighRiskCVE := 0
	fleetReboot := 0
	fleetStaleFacts := 0

	for _, server := range servers {
		if server.Health.RebootRequired != nil && *server.Health.RebootRequired {
			fleetReboot++
		}
		triage := server.ApprovalTriage
		if triage.FactsState == "stale" {
			fleetStaleFacts++
		}
		fleetPendingPackages += triage.PendingPackages
		fleetSecurityUpdates += triage.SecurityUpdates
		if triage.CVECount > 0 {
			fleetHighRiskCVE++
		}
		if triage.CanApproveAll || server.Timeline.CurrentPhase == "pending_approval" {
			fleetPendingApproval++
		}
		if server.Timeline.CurrentPhase == "prechecks" || server.Timeline.State == "queued" || server.Timeline.State == "active" {
			fleetPrechecksRunning++
		}
		if runningTimelineState(server.Timeline.State) {
			fleetInProgress++
		}
		if server.Timeline.State == "done" {
			fleetDone++
		}
	}

	fleet["pending_approval"] = fleetPendingApproval
	fleet["prechecks_running"] = fleetPrechecksRunning
	fleet["in_progress"] = fleetInProgress
	fleet["done"] = fleetDone
	fleet["pending_packages"] = fleetPendingPackages
	fleet["security_updates"] = fleetSecurityUpdates
	fleet["high_risk_cve"] = fleetHighRiskCVE
	fleet["hosts_needing_reboot"] = fleetReboot
	fleet["stale_facts"] = fleetStaleFacts
	return fleet
}
