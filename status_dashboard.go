package main

import (
	"net/http"
	"strconv"

	jobsPkg "debian-updater/internal/jobs"
	observabilitypkg "debian-updater/internal/observability"
	"debian-updater/internal/postchecks"
	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

const statusProjectionHeader = "X-SLU-Status-Projection"

type statusPackageCounts struct {
	Pending      int `json:"pending"`
	Total        int `json:"total"`
	Security     int `json:"security"`
	KeptBack     int `json:"kept_back"`
	KeptSecurity int `json:"kept_security"`
	CVEs         int `json:"cves"`
}

type statusDeferralSummary struct {
	Revision string `json:"revision,omitempty"`
	HasRules bool   `json:"has_rules,omitempty"`
}

type statusPlanSummary struct {
	StandardPackageCount          int                   `json:"standard_package_count"`
	KeptBackPackageCount          int                   `json:"kept_back_package_count"`
	StandardSecurityCount         int                   `json:"standard_security_count"`
	TotalSecurityCount            int                   `json:"total_security_count"`
	FullUpgradePackageCount       int                   `json:"full_upgrade_package_count"`
	FullUpgradePlanAvailable      bool                  `json:"full_upgrade_plan_available"`
	KeptBackSecurityPackageCount  int                   `json:"kept_back_security_package_count"`
	KeptBackSecurityPlanAvailable bool                  `json:"kept_back_security_plan_available"`
	KeptBackSecurityHasRemovals   bool                  `json:"kept_back_security_has_removals,omitempty"`
	PackageDeferrals              statusDeferralSummary `json:"package_deferrals,omitzero"`
}

type statusServerRow struct {
	Name                     string                  `json:"name"`
	Host                     string                  `json:"host"`
	Port                     int                     `json:"port"`
	User                     string                  `json:"user"`
	Status                   string                  `json:"status"`
	Disabled                 bool                    `json:"disabled"`
	HasPassword              bool                    `json:"has_password"`
	HasKey                   bool                    `json:"has_key"`
	HostKeyStatus            serverpkg.HostKeyStatus `json:"host_key_status"`
	Tags                     []string                `json:"tags"`
	ServerIdentity           string                  `json:"server_identity,omitempty"`
	JobID                    string                  `json:"job_id,omitempty"`
	ApprovalGeneration       uint64                  `json:"approval_generation,omitempty"`
	RemediationDigest        string                  `json:"remediation_digest,omitempty"`
	HasApplicationPostchecks bool                    `json:"has_application_postchecks,omitempty"`
	PackageCounts            statusPackageCounts     `json:"package_counts"`
	UpgradePlan              statusPlanSummary       `json:"upgrade_plan"`
}

func projectStatusRows(statuses []serverpkg.ServerStatus, canReadLogs bool) []statusServerRow {
	rows := make([]statusServerRow, 0, len(statuses))
	for _, status := range statuses {
		counts := statusPackageCounts{
			Pending: len(status.PendingUpdates),
			Total:   len(status.Upgradable),
		}
		if counts.Pending > 0 {
			counts.Total = counts.Pending
		}
		for _, update := range status.PendingUpdates {
			if update.Security {
				counts.Security++
			}
			if update.KeptBack || update.RequiresFull {
				counts.KeptBack++
				if update.Security {
					counts.KeptSecurity++
				}
			}
			counts.CVEs += len(update.CVEs)
		}
		plan := status.UpgradePlan
		rows = append(rows, statusServerRow{
			Name: status.Name, Host: status.Host, Port: status.Port, User: status.User,
			Status: status.Status, Disabled: status.Disabled, Tags: status.Tags,
			HasPassword: status.HasPassword, HasKey: status.HasKey, HostKeyStatus: status.HostKeyStatus,
			ServerIdentity: status.ServerIdentity, JobID: status.JobID,
			ApprovalGeneration: status.ApprovalGeneration, RemediationDigest: status.RemediationDigest,
			HasApplicationPostchecks: canReadLogs && status.ApplicationPostcheckProfile != nil,
			PackageCounts:            counts,
			UpgradePlan: statusPlanSummary{
				StandardPackageCount: plan.StandardPackageCount, KeptBackPackageCount: plan.KeptBackPackageCount,
				StandardSecurityCount: plan.StandardSecurityCount, TotalSecurityCount: plan.TotalSecurityCount,
				FullUpgradePlanAvailable: plan.FullUpgradePlanAvailable, FullUpgradePackageCount: plan.FullUpgradePackageCount,
				KeptBackSecurityPlanAvailable: plan.KeptBackSecurityPlanAvailable, KeptBackSecurityPackageCount: plan.KeptBackSecurityPackageCount,
				KeptBackSecurityHasRemovals: len(plan.KeptBackSecurityRemovedPackages) > 0,
				PackageDeferrals: statusDeferralSummary{
					Revision: plan.PackageDeferrals.Revision,
					HasRules: len(plan.PackageDeferrals.Rules) > 0,
				},
			},
		})
	}
	return rows
}

type statusHostDetail struct {
	PendingUpdates              []serverpkg.PendingUpdate `json:"pending_updates,omitempty"`
	UpgradePlan                 *serverpkg.UpgradePlan    `json:"upgrade_plan,omitempty"`
	Logs                        string                    `json:"logs,omitempty"`
	ApplicationPostcheckProfile *postchecks.Snapshot      `json:"application_postcheck_profile,omitempty"`
}

func handleStatusHostDetail(c *gin.Context, state *serverpkg.State) {
	c.Header("Cache-Control", "no-store")
	generation, err := strconv.ParseUint(c.Query("approval_generation"), 10, 64)
	name := c.Query("server_name")
	identity := c.Query("server_identity")
	view := c.Query("view")
	if err != nil || name == "" || identity == "" || (view != "packages" && view != "logs" && view != "overview") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status detail request"})
		return
	}

	status := state.InventoryStatusSnapshot(name)
	if status == nil || status.ServerIdentity != identity ||
		(c.Query("require_pending_approval") == "true" && status.Status != "pending_approval") ||
		status.JobID != c.Query("job_id") || status.ApprovalGeneration != generation ||
		status.UpgradePlan.PackageDeferrals.Revision != c.Query("deferral_revision") ||
		status.RemediationDigest != c.Query("remediation_digest") {
		c.JSON(http.StatusConflict, gin.H{"error": "Host status changed. Refresh Status and review the current plan."})
		return
	}

	var detail statusHostDetail
	if view == "packages" {
		detail.PendingUpdates = status.PendingUpdates
		detail.UpgradePlan = &status.UpgradePlan
	}
	if contextAllows(c, "logs:read") {
		detail.ApplicationPostcheckProfile = status.ApplicationPostcheckProfile
		if view == "logs" {
			detail.Logs = jobsPkg.BoundedLogPreview(status.Logs)
		}
	}
	c.JSON(http.StatusOK, detail)
}

type statusDashboardRisk struct {
	UpdateCVEs observabilitypkg.UpdateCVEInfo `json:"update_cves"`
}

type statusDashboardRow struct {
	Name              string                                          `json:"name"`
	LastUpdate        *observabilitypkg.DashboardUpdateHistory        `json:"last_update,omitempty"`
	LastFailedUpdate  *observabilitypkg.DashboardUpdateHistory        `json:"last_failed_update,omitempty"`
	AvgDurationMS     float64                                         `json:"avg_duration_ms,omitempty"`
	DurationSamples   int                                             `json:"duration_samples,omitempty"`
	NextRun           observabilitypkg.DashboardScheduleInfo          `json:"next_run"`
	NoRun             observabilitypkg.DashboardNoRunInfo             `json:"no_run"`
	Health            observabilitypkg.DashboardHealthInfo            `json:"health"`
	Risk              statusDashboardRisk                             `json:"risk"`
	Timeline          observabilitypkg.DashboardTimelineInfo          `json:"timeline"`
	Actions           map[string]observabilitypkg.DashboardActionInfo `json:"actions"`
	RecommendedAction observabilitypkg.DashboardRecommendedActionInfo `json:"recommended_action"`
	ApprovalTriage    observabilitypkg.DashboardApprovalTriageInfo    `json:"approval_triage"`
}

type statusDashboardSummary struct {
	dashboardSummaryResponse
	Servers []statusDashboardRow `json:"servers"`
}

func projectStatusDashboard(summary dashboardSummaryResponse) statusDashboardSummary {
	rows := make([]statusDashboardRow, 0, len(summary.Servers))
	for _, server := range summary.Servers {
		rows = append(rows, statusDashboardRow{
			Name: server.Name, LastUpdate: server.LastUpdate, LastFailedUpdate: server.LastFailedUpdate,
			AvgDurationMS: server.AvgDurationMS, DurationSamples: server.DurationSamples,
			NextRun: server.NextRun, NoRun: server.NoRun, Health: server.Health,
			Risk:     statusDashboardRisk{UpdateCVEs: server.Risk.UpdateCVEs},
			Timeline: server.Timeline, Actions: server.Actions,
			RecommendedAction: server.RecommendedAction, ApprovalTriage: server.ApprovalTriage,
		})
	}
	return statusDashboardSummary{dashboardSummaryResponse: summary, Servers: rows}
}
