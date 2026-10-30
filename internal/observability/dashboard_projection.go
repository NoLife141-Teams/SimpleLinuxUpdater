package observability

import (
	"debian-updater/internal/vulnerabilities"
	"sort"
	"time"

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
	vulnerability            vulnerabilities.Observation
	vulnerabilityTargetDays  map[string]int
	vulnerabilityMaxAge      time.Duration
	vulnerabilityScanEnabled bool
	now                      time.Time
	server                   servers.Server
	status                   *servers.ServerStatus
	health                   DashboardHealthInfo
	failure                  dashboardMaintenanceFailureFacts
	readiness                servers.MaintenanceReadiness
	nextRun                  DashboardScheduleInfo
	noRun                    DashboardNoRunInfo
	timeline                 DashboardTimelineInfo
	triageTime               dashboardTriageTimeFacts
	updateHistory            dashboardUpdateHistoryProjection
	commandHistory           []DashboardCommandHistoryItem
}

type dashboardMaintenanceFailureFacts struct {
	cause      string
	errorClass string
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
	if input.server.Disabled {
		input.readiness = servers.DisabledReadiness()
	}
	health := input.health
	nextRun := input.nextRun
	if nextRun.State == "" {
		nextRun = defaultScheduleInfo()
	}
	risk := projectVulnerabilityRisk(input.status, input.vulnerability, input.now, input.vulnerabilityTargetDays, input.vulnerabilityMaxAge, input.vulnerabilityScanEnabled)
	timeline := input.timeline

	lastUpdate := input.updateHistory.lastSuccess
	durationSamples := input.updateHistory.samples
	avgDurationMS := 0.0
	if durationSamples > 0 {
		avgDurationMS = input.updateHistory.durationSum / float64(durationSamples)
	}
	triage := buildApprovalTriage(input.status, health, risk, timeline, input.triageTime)
	actions := buildDashboardActions(input.server.Name, input.status, timeline, triage, health, input.readiness)
	actions["scan_vulnerabilities"] = actions[dashboardActionRefreshFacts]
	if !input.vulnerabilityScanEnabled && !input.server.Disabled {
		actions["scan_vulnerabilities"] = dashboardUnavailableAction("Vulnerability scanning is disabled", "")
	}
	triage = mirrorApprovalTriageActions(triage, actions)
	recommendedAction := buildDashboardRecommendedAction(input.status, timeline, health, input.triageTime, input.failure, actions)
	if input.server.Disabled {
		recommendedAction = DashboardRecommendedActionInfo{Key: "disabled", Label: "Server disabled", Detail: "Enable this server in Manage Servers to resume maintenance."}
	}

	return DashboardServerSummary{
		Name:              input.server.Name,
		LastUpdate:        lastUpdate,
		LastFailedUpdate:  input.updateHistory.lastFailure,
		AvgDurationMS:     avgDurationMS,
		DurationSamples:   durationSamples,
		NextRun:           nextRun,
		NoRun:             input.noRun,
		Health:            health,
		Risk:              risk,
		Timeline:          timeline,
		Actions:           actions,
		RecommendedAction: recommendedAction,
		ApprovalTriage:    triage,
		CommandHistory:    input.commandHistory,
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
	coverage := map[string]int{}
	staleScans := 0
	qualifiedHosts, confirmedHosts, reviewHosts, withdrawnHosts := 0, 0, 0, 0

	for _, server := range servers {
		coverage[server.Risk.Vulnerability.State]++
		assessment := server.Risk.Vulnerability
		if assessment.ConfirmedCVEs > 0 {
			confirmedHosts++
		}
		if assessment.NeedsReviewCVEs > 0 {
			reviewHosts++
		}
		if assessment.VendorQualifiedCVEs > 0 {
			qualifiedHosts++
		}
		if assessment.WithdrawnCVEs > 0 {
			withdrawnHosts++
		}
		if server.Risk.Vulnerability.Freshness == "stale" {
			staleScans++
		}
		if server.Health.RebootRequired != nil && *server.Health.RebootRequired {
			fleetReboot++
		}
		triage := server.ApprovalTriage
		if triage.FactsState == "stale" || triage.FactsState == "unknown" {
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

	fleet["vulnerability_scan_states"] = coverage
	fleet["stale_vulnerability_scans"] = staleScans
	fleet["pending_approval"] = fleetPendingApproval
	fleet["prechecks_running"] = fleetPrechecksRunning
	fleet["in_progress"] = fleetInProgress
	fleet["done"] = fleetDone
	fleet["pending_packages"] = fleetPendingPackages
	fleet["security_updates"] = fleetSecurityUpdates
	fleet["high_risk_cve"] = fleetHighRiskCVE // Legacy count of hosts with reported source matches.
	fleet["vendor_confirmed_cve_hosts"] = confirmedHosts
	fleet["review_cve_hosts"] = reviewHosts
	fleet["vendor_qualified_cve_hosts"] = qualifiedHosts
	fleet["withdrawn_cve_hosts"] = withdrawnHosts
	fleet["hosts_needing_reboot"] = fleetReboot
	fleet["stale_facts"] = fleetStaleFacts
	return fleet
}
