package observability

import (
	"sort"

	"debian-updater/internal/servers"
)

type dashboardProjection struct{}

type dashboardProjectionInput struct {
	window      string
	from        string
	to          string
	generatedAt string
	servers     []dashboardServerProjectionInput
}

type dashboardServerProjectionInput struct {
	server         servers.Server
	status         *servers.ServerStatus
	health         DashboardHealthInfo
	nextRun        DashboardScheduleInfo
	noRun          DashboardNoRunInfo
	timeline       DashboardTimelineInfo
	triageTime     dashboardTriageTimeFacts
	updateHistory  dashboardUpdateHistoryProjection
	commandHistory []DashboardCommandHistoryItem
}

type dashboardTimelineSourceFacts struct {
	currentPhase string
	state        string
	summary      string
	startedAt    string
	updatedAt    string
}

type dashboardTriageTimeFacts struct {
	factsState              string
	factsCollectedAtDisplay string
	lastCheckAt             string
	lastCheckDisplay        string
}

type dashboardUpdateHistoryProjection struct {
	lastSuccess *DashboardUpdateHistory
	lastFailure *DashboardUpdateHistory
	durationSum float64
	samples     int
}

func newDashboardProjection() dashboardProjection {
	return dashboardProjection{}
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
	health := input.health
	nextRun := input.nextRun
	if nextRun.State == "" {
		nextRun = defaultScheduleInfo()
	}
	risk := DashboardRiskFromStatus(input.status)
	timeline := input.timeline

	lastUpdate := input.updateHistory.lastSuccess
	durationSamples := input.updateHistory.samples
	avgDurationMS := 0.0
	if durationSamples > 0 {
		avgDurationMS = input.updateHistory.durationSum / float64(durationSamples)
	}
	triage := buildApprovalTriage(input.status, health, risk, timeline, input.triageTime)
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
