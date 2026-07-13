package observability

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func testDashboardProjection(time.Time) dashboardProjection {
	return newDashboardProjection()
}

func testCollectedHealth(serverName, collectedAt, diskStatus, aptStatus string) DashboardHealthInfo {
	source := "facts"
	if serverName == "" {
		source = "unknown"
	}
	return DashboardHealthInfo{CollectedAt: collectedAt, DiskStatus: diskStatus, AptStatus: aptStatus, Source: source}
}

func testCollectedTimeline(currentPhase, state, summary string) DashboardTimelineInfo {
	progress := timelinePhaseProgress(currentPhase)
	if terminalTimelineState(state) {
		progress = 100
	}
	return DashboardTimelineInfo{CurrentPhase: currentPhase, State: state, ProgressPct: progress, Summary: summary}
}

func testCollectedTriageTime(factsState, collectedAt string) dashboardTriageTimeFacts {
	display := ""
	if collectedAt != "" {
		display = "display:" + collectedAt
	}
	return dashboardTriageTimeFacts{
		factsState:              factsState,
		factsCollectedAtDisplay: display,
		lastCheckAt:             collectedAt,
		lastCheckDisplay:        display,
	}
}

func requireDashboardAction(t *testing.T, server DashboardServerSummary, key string) DashboardActionInfo {
	t.Helper()
	action, ok := server.Actions[key]
	if !ok {
		t.Fatalf("%s action %q missing from %+v", server.Name, key, server.Actions)
	}
	return action
}

func TestDashboardProjectionUsesCollectedRuntimeSourceFacts(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{{
			server:     servers.Server{Name: "srv-active"},
			status:     &servers.ServerStatus{Name: "srv-active", Status: "updating"},
			health:     testCollectedHealth("", "", "unknown", "unknown"),
			timeline:   testCollectedTimeline("prechecks", "active", "Runtime status: Updating"),
			triageTime: testCollectedTriageTime("stale", ""),
		}},
	})

	got := summary.Servers[0].Timeline
	if got.CurrentPhase != "prechecks" || got.State != "active" || got.ProgressPct != 32 {
		t.Fatalf("timeline = %+v, want runtime prechecks active instead of stale terminal job", got)
	}
	action := requireDashboardAction(t, summary.Servers[0], dashboardActionUpdate)
	if action.Enabled || action.Readiness != dashboardActionReadinessInProgress || action.BlockingStatus != "updating" {
		t.Fatalf("update action = %+v, want in-progress block from runtime status", action)
	}
}

func TestDashboardProjectionFleetCountersRollUpProjectedSummaries(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rebootRequired := true

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{
			{
				server: servers.Server{Name: "srv-z"},
				status: &servers.ServerStatus{
					Name:   "srv-z",
					Status: "pending_approval",
					PendingUpdates: []servers.PendingUpdate{
						{Package: "openssl", Security: true, CVEs: []string{"CVE-2026-1"}},
					},
				},
				health:     testCollectedHealth("srv-z", now.Add(-time.Hour).Format(time.RFC3339), "ok", "ok"),
				timeline:   testCollectedTimeline("pending_approval", "waiting", "Runtime status: Pending approval"),
				triageTime: testCollectedTriageTime("fresh", now.Add(-time.Hour).Format(time.RFC3339)),
			},
			{
				server: servers.Server{Name: "srv-a"},
				status: &servers.ServerStatus{Name: "srv-a", Status: "done"},
				health: func() DashboardHealthInfo {
					health := testCollectedHealth("srv-a", now.Add(-49*time.Hour).Format(time.RFC3339), "ok", "ok")
					health.RebootRequired = &rebootRequired
					return health
				}(),
				timeline:   testCollectedTimeline("done_error", "done", "Runtime status: Done"),
				triageTime: testCollectedTriageTime("stale", now.Add(-49*time.Hour).Format(time.RFC3339)),
			},
		},
	})

	if summary.Servers[0].Name != "srv-a" || summary.Servers[1].Name != "srv-z" {
		t.Fatalf("server order = %q, %q; want sorted by name", summary.Servers[0].Name, summary.Servers[1].Name)
	}
	want := map[string]any{
		"pending_approval":     1,
		"prechecks_running":    0,
		"in_progress":          0,
		"done":                 1,
		"pending_packages":     1,
		"security_updates":     1,
		"high_risk_cve":        1,
		"hosts_needing_reboot": 1,
		"stale_facts":          1,
	}
	for key, value := range want {
		if summary.Fleet[key] != value {
			t.Fatalf("fleet[%s] = %v, want %v in %+v", key, summary.Fleet[key], value, summary.Fleet)
		}
	}
}

func TestDashboardProjectionMissingFactsDefaultsRemainDashboardSafe(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{{
			server:     servers.Server{Name: "srv-missing"},
			health:     testCollectedHealth("", "", "unknown", "unknown"),
			timeline:   testCollectedTimeline("", "idle", "No maintenance activity"),
			triageTime: testCollectedTriageTime("stale", ""),
		}},
	})

	got := summary.Servers[0]
	if got.Health.Source != "unknown" || got.Health.DiskStatus != "unknown" || got.Health.AptStatus != "unknown" {
		t.Fatalf("health defaults = %+v, want unknown source/disk/apt", got.Health)
	}
	if got.NextRun.State != "none" || got.NextRun.Summary != "No scheduled run" {
		t.Fatalf("next run = %+v, want default schedule info", got.NextRun)
	}
	if got.ApprovalTriage.FactsState != "stale" || !got.ApprovalTriage.CanRefreshFacts || !got.ApprovalTriage.CanRunChecks {
		t.Fatalf("approval triage = %+v, want stale facts with transient actions available", got.ApprovalTriage)
	}
	if action := requireDashboardAction(t, got, dashboardActionRefreshFacts); !action.Enabled || action.Readiness != dashboardActionReadinessReady {
		t.Fatalf("refresh facts action = %+v, want stale facts refreshable", action)
	}
}

func TestDashboardProjectionActionContractDelegatesApprovalScopeAndOwnsTransientGating(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{
			{
				server: servers.Server{Name: "srv-done-risk"},
				status: &servers.ServerStatus{
					Name:           "srv-done-risk",
					Status:         "done",
					PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true}},
				},
				health:     testCollectedHealth("srv-done-risk", now.Format(time.RFC3339), "ok", "ok"),
				timeline:   testCollectedTimeline("done_error", "done", "Runtime status: Done"),
				triageTime: testCollectedTriageTime("fresh", now.Format(time.RFC3339)),
			},
			{
				server: servers.Server{Name: "srv-pending"},
				status: &servers.ServerStatus{
					Name:   "srv-pending",
					Status: "pending_approval",
					UpgradePlan: servers.UpgradePlan{
						StandardPackageCount:          1,
						StandardSecurityCount:         1,
						KeptBackPackageCount:          1,
						TotalSecurityCount:            2,
						KeptBackSecurityPlanAvailable: true,
						FullUpgradePlanAvailable:      true,
						FullUpgradeNewPackages:        []string{"new-lib"},
					},
				},
				health:     testCollectedHealth("srv-pending", now.Format(time.RFC3339), "ok", "ok"),
				timeline:   testCollectedTimeline("pending_approval", "waiting", "Runtime status: Pending approval"),
				triageTime: testCollectedTriageTime("fresh", now.Format(time.RFC3339)),
			},
			{
				server: servers.Server{Name: "srv-needs-scan"},
				status: &servers.ServerStatus{
					Name:   "srv-needs-scan",
					Status: "pending_approval",
					PendingUpdates: []servers.PendingUpdate{
						{Package: "kernel", Security: true, KeptBack: true},
					},
				},
				health:     testCollectedHealth("srv-needs-scan", now.Format(time.RFC3339), "ok", "ok"),
				timeline:   testCollectedTimeline("pending_approval", "waiting", "Runtime status: Pending approval"),
				triageTime: testCollectedTriageTime("fresh", now.Format(time.RFC3339)),
			},
		},
	})

	byName := map[string]DashboardServerSummary{}
	for _, server := range summary.Servers {
		byName[server.Name] = server
	}
	doneRisk := byName["srv-done-risk"].ApprovalTriage
	if !doneRisk.Eligible || doneRisk.CanApproveAll || doneRisk.CanApproveSecurity || !doneRisk.CanRefreshFacts || !doneRisk.CanRunChecks {
		t.Fatalf("done-risk triage = %+v, want eligible risk with approval unavailable and transient actions available", doneRisk)
	}
	doneRiskUpdate := requireDashboardAction(t, byName["srv-done-risk"], dashboardActionUpdate)
	doneRiskApprove := requireDashboardAction(t, byName["srv-done-risk"], dashboardActionApproveAll)
	if !doneRiskUpdate.Enabled || doneRiskApprove.Enabled || doneRiskApprove.Readiness != dashboardActionReadinessUnavailable {
		t.Fatalf("done-risk actions update=%+v approve=%+v, want update ready and approval unavailable", doneRiskUpdate, doneRiskApprove)
	}
	pendingServer := byName["srv-pending"]
	pending := pendingServer.ApprovalTriage
	if !pending.CanApproveAll || !pending.CanApproveSecurity || !pending.CanApproveKeptBackSecurity || !pending.CanApproveFull {
		t.Fatalf("pending approval availability = %+v, want delegated approval scope actions enabled", pending)
	}
	if pending.CanRefreshFacts || pending.CanRunChecks {
		t.Fatalf("pending approval transient actions = %+v, want locked while waiting for approval", pending)
	}
	actionKeys := []string{
		dashboardActionUpdate,
		dashboardActionApproveAll,
		dashboardActionApproveSecurity,
		dashboardActionApproveSecurityKeptBack,
		dashboardActionApproveFull,
		dashboardActionCancel,
		dashboardActionAutoremove,
		dashboardActionRefreshFacts,
		dashboardActionEnableApt,
		dashboardActionDisableApt,
	}
	for _, key := range actionKeys {
		requireDashboardAction(t, pendingServer, key)
	}
	if action := requireDashboardAction(t, pendingServer, dashboardActionApproveAll); !action.Enabled || action.Counts["updates"] != 1 {
		t.Fatalf("approve all action = %+v, want one standard update enabled", action)
	}
	if action := requireDashboardAction(t, pendingServer, dashboardActionApproveSecurityKeptBack); !action.Enabled || action.Counts["kept_back_security_updates"] != 1 {
		t.Fatalf("kept-back security action = %+v, want one kept-back security update enabled", action)
	}
	if action := requireDashboardAction(t, pendingServer, dashboardActionCancel); !action.Enabled || action.Readiness != dashboardActionReadinessReady {
		t.Fatalf("cancel action = %+v, want pending approval cancellable", action)
	}
	if action := requireDashboardAction(t, pendingServer, dashboardActionRefreshFacts); action.Enabled || action.Readiness != dashboardActionReadinessInProgress || action.BlockingStatus != "pending_approval" {
		t.Fatalf("refresh facts action = %+v, want pending approval to block transient actions", action)
	}
	needsScan := requireDashboardAction(t, byName["srv-needs-scan"], dashboardActionApproveSecurityKeptBack)
	if needsScan.Enabled || needsScan.Readiness != dashboardActionReadinessBlocked || needsScan.Reason != "Needs a fresh package scan" {
		t.Fatalf("needs-scan kept-back security action = %+v, want blocked fresh-scan reason", needsScan)
	}
	if byName["srv-needs-scan"].ApprovalTriage.CanApproveKeptBackSecurity {
		t.Fatalf("needs-scan compatibility triage = %+v, want kept-back security approval disabled", byName["srv-needs-scan"].ApprovalTriage)
	}
}

func TestDashboardProjectionUsesCollectedHealthAndFreshnessFacts(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rebootRequired := true

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{
			{
				server: servers.Server{Name: "srv-overlay"},
				health: DashboardHealthInfo{
					Source:         "audit",
					CollectedAt:    now.Add(-time.Hour).Format(time.RFC3339),
					DiskStatus:     "ok",
					AptStatus:      "failed",
					RebootRequired: &rebootRequired,
				},
				timeline:   testCollectedTimeline("", "idle", "No maintenance activity"),
				triageTime: testCollectedTriageTime("fresh", now.Add(-time.Hour).Format(time.RFC3339)),
			},
			{
				server: servers.Server{Name: "srv-malformed"},
				health: DashboardHealthInfo{
					Source:      "facts",
					CollectedAt: "not-a-time",
					DiskStatus:  "ok",
					AptStatus:   "ok",
				},
				timeline:   testCollectedTimeline("", "idle", "No maintenance activity"),
				triageTime: testCollectedTriageTime("stale", "not-a-time"),
			},
		},
	})

	byName := map[string]DashboardServerSummary{}
	for _, server := range summary.Servers {
		byName[server.Name] = server
	}
	overlay := byName["srv-overlay"]
	if overlay.Health.Source != "audit" || overlay.Health.AptStatus != "failed" || overlay.Health.RebootRequired == nil || !*overlay.Health.RebootRequired {
		t.Fatalf("overlay health = %+v, want audit apt failure and reboot-required overlay", overlay.Health)
	}
	if overlay.ApprovalTriage.FactsState != "fresh" {
		t.Fatalf("overlay facts state = %q, want fresh", overlay.ApprovalTriage.FactsState)
	}
	malformed := byName["srv-malformed"]
	if malformed.Health.Source != "facts" || malformed.Health.AptStatus != "ok" {
		t.Fatalf("malformed health = %+v, want original facts preserved", malformed.Health)
	}
	if malformed.ApprovalTriage.FactsState != "stale" {
		t.Fatalf("malformed facts state = %q, want stale", malformed.ApprovalTriage.FactsState)
	}
}
