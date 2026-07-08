package observability

import (
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

func testDashboardProjection(now time.Time) dashboardProjection {
	deps := ServiceDeps{
		CurrentTimezone: func() (*time.Location, string) { return time.UTC, "UTC" },
		CurrentLocation: func() *time.Location { return time.UTC },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			if raw == "" {
				return "", "UTC"
			}
			return "display:" + raw, "UTC"
		},
		ParseAppTimestamp: func(raw string) (time.Time, error) {
			return time.Parse(time.RFC3339, raw)
		},
		HealthStatusFromResult: func(result updates.PrecheckResult) string {
			if result.Passed {
				return "ok"
			}
			return "failed"
		},
		RebootResultRequiresRestart: func(updates.PrecheckResult) (bool, bool) { return true, true },
	}
	return newDashboardProjection(dashboardProjectionContext{
		now:          now,
		deps:         deps,
		loc:          time.UTC,
		timezoneName: "UTC",
	})
}

func requireDashboardAction(t *testing.T, server DashboardServerSummary, key string) DashboardActionInfo {
	t.Helper()
	action, ok := server.Actions[key]
	if !ok {
		t.Fatalf("%s action %q missing from %+v", server.Name, key, server.Actions)
	}
	return action
}

func TestDashboardProjectionRuntimeActiveStatusBeatsStaleTerminalJob(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	staleJob := jobs.Record{
		ID:         "job-stale",
		Status:     jobs.StatusFailed,
		Phase:      jobs.PhasePostchecks,
		Summary:    "Old post-check failed",
		CreatedAt:  now.Add(-10 * time.Minute).Format(time.RFC3339),
		UpdatedAt:  now.Add(-10 * time.Minute).Format(time.RFC3339),
		StartedAt:  now.Add(-11 * time.Minute).Format(time.RFC3339),
		ServerName: "srv-active",
	}

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{{
			server:          servers.Server{Name: "srv-active"},
			status:          &servers.ServerStatus{Name: "srv-active", Status: "updating"},
			latestUpdateJob: &staleJob,
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
				fact: updates.ServerFactsRecord{
					ServerName:  "srv-z",
					CollectedAt: now.Add(-time.Hour).Format(time.RFC3339),
					DiskStatus:  "ok",
					AptStatus:   "ok",
				},
			},
			{
				server: servers.Server{Name: "srv-a"},
				status: &servers.ServerStatus{Name: "srv-a", Status: "done"},
				fact: updates.ServerFactsRecord{
					ServerName:     "srv-a",
					CollectedAt:    now.Add(-49 * time.Hour).Format(time.RFC3339),
					DiskStatus:     "ok",
					AptStatus:      "ok",
					RebootRequired: &rebootRequired,
				},
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
			server: servers.Server{Name: "srv-missing"},
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
				fact: updates.ServerFactsRecord{ServerName: "srv-done-risk", CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
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
				fact: updates.ServerFactsRecord{ServerName: "srv-pending", CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
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
				fact: updates.ServerFactsRecord{ServerName: "srv-needs-scan", CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
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

func TestDashboardProjectionAuditMetadataOverlaysFactsAndKeepsMalformedStale(t *testing.T) {
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rebootRequired := false

	summary := testDashboardProjection(now).Project(dashboardProjectionInput{
		window:      "7d",
		from:        now.Add(-7 * 24 * time.Hour).Format(time.RFC3339),
		to:          now.Format(time.RFC3339),
		generatedAt: now.Format(time.RFC3339),
		servers: []dashboardServerProjectionInput{
			{
				server: servers.Server{Name: "srv-overlay"},
				fact: updates.ServerFactsRecord{
					ServerName:     "srv-overlay",
					CollectedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
					DiskStatus:     "ok",
					AptStatus:      "ok",
					RebootRequired: &rebootRequired,
				},
				updateHistory: dashboardUpdateHistoryProjection{
					metaAt: now.Add(-time.Hour).Format(time.RFC3339),
					meta: map[string]any{
						"postcheck_results": []updates.PrecheckResult{
							{Name: updates.PostcheckNameAptHealth, Passed: false, Details: "apt is unhealthy"},
							{Name: updates.PostcheckNameRebootNeeded, Passed: true, Details: "reboot required"},
						},
					},
				},
			},
			{
				server: servers.Server{Name: "srv-malformed"},
				fact: updates.ServerFactsRecord{
					ServerName:  "srv-malformed",
					CollectedAt: "not-a-time",
					DiskStatus:  "ok",
					AptStatus:   "ok",
				},
				updateHistory: dashboardUpdateHistoryProjection{
					metaAt: "also-not-a-time",
					meta: map[string]any{
						"postcheck_results": []updates.PrecheckResult{
							{Name: updates.PostcheckNameAptHealth, Passed: false, Details: "ignored"},
						},
					},
				},
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
