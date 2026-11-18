package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

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
	Version           string                                          `json:"version"`
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
	Servers     []statusDashboardRow `json:"servers"`
	RowsVersion string               `json:"rows_version"`
}

type statusChangedHosts struct {
	Servers   []statusServerRow      `json:"servers"`
	Dashboard statusDashboardSummary `json:"dashboard"`
}

const statusChangedHostLimit = 16

func handleStatusChangedHosts(c *gin.Context, deps AppDeps) {
	c.Header("Cache-Control", "no-store")
	revision, err := strconv.ParseUint(c.Query("revision"), 10, 64)
	requested := c.QueryArray("server")
	if err != nil || revision == 0 || len(requested) == 0 || len(requested) > statusChangedHostLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Status host refresh"})
		return
	}
	names := make(map[string]struct{}, len(requested))
	for _, name := range requested {
		if name == "" || len(name) > 255 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Status host name"})
			return
		}
		names[name] = struct{}{}
	}
	stale := func() {
		c.JSON(http.StatusConflict, gin.H{"error": "Status changed during the host refresh"})
	}
	broker := deps.DashboardEventBroker
	if broker.Revision() != revision {
		stale()
		return
	}
	statuses := deps.ServerInventoryService.ListStatusesFor(names)
	if len(statuses) != len(names) {
		stale()
		return
	}
	ctx := c.Request.Context()
	var timings observabilitypkg.DashboardSummaryTimings
	summary, err := deps.ObservabilityService.BuildDashboardSummaryContextWithOptions(ctx, "7d", deps.Now(), observabilitypkg.DashboardSummaryOptions{
		OmitCommandHistory: true, OmitVulnerabilityFindings: true, Timings: &timings,
	})
	if err != nil {
		if ctx.Err() == nil {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "could not refresh Status hosts"})
		}
		return
	}
	compactStarted := time.Now()
	dashboard := projectStatusDashboardRows(summary, names)
	// Apply inventory, dashboard rows, and fleet totals together or read a
	// fresh full snapshot when another dashboard invalidation raced this read.
	if len(dashboard.Servers) != len(names) || broker.Revision() != revision {
		stale()
		return
	}
	writeStatusChangedHosts(c, statusChangedHosts{
		Servers:   projectStatusRows(statuses, contextAllows(c, "logs:read")),
		Dashboard: dashboard,
	}, timings, time.Since(compactStarted))
}

func writeStatusChangedHosts(c *gin.Context, payload statusChangedHosts, timings observabilitypkg.DashboardSummaryTimings, compact time.Duration) {
	started := time.Now()
	var body bytes.Buffer
	gzipAccepted := acceptsGzipEncoding(c.GetHeader("Accept-Encoding"))
	var err error
	if gzipAccepted {
		err = encodeGzipDashboardSummary(&body, payload)
	} else {
		err = json.NewEncoder(&body).Encode(payload)
	}
	if err != nil {
		log.Printf("encode Status host refresh: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not encode Status hosts"})
		return
	}
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	c.Header("Server-Timing", fmt.Sprintf("wait;dur=%.3f, collect;dur=%.3f, project;dur=%.3f, compact;dur=%.3f, encode;dur=%.3f",
		ms(timings.Wait), ms(timings.Collection), ms(timings.Projection), ms(compact), ms(time.Since(started))))
	c.Writer.Header().Add("Vary", "Accept-Encoding")
	if gzipAccepted {
		c.Header("Content-Encoding", "gzip")
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", body.Bytes())
}

func projectStatusDashboard(summary dashboardSummaryResponse) statusDashboardSummary {
	return projectStatusDashboardRows(summary, nil)
}

func projectStatusDashboardRows(summary dashboardSummaryResponse, names map[string]struct{}) statusDashboardSummary {
	capacity := len(summary.Servers)
	if names != nil {
		capacity = len(names)
	}
	rows := make([]statusDashboardRow, 0, capacity)
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	var rowsVersion uint64
	for _, server := range summary.Servers {
		row := statusDashboardRow{
			Name: server.Name, LastUpdate: server.LastUpdate, LastFailedUpdate: server.LastFailedUpdate,
			AvgDurationMS: server.AvgDurationMS, DurationSamples: server.DurationSamples,
			NextRun: server.NextRun, NoRun: server.NoRun, Health: server.Health,
			Risk:     statusDashboardRisk{UpdateCVEs: server.Risk.UpdateCVEs},
			Timeline: server.Timeline, Actions: server.Actions,
			RecommendedAction: server.RecommendedAction, ApprovalTriage: server.ApprovalTriage,
		}
		// Fingerprint the compact row without its version or any summary
		// timestamps. Stale facts, CVE evidence and policy windows can change
		// on an untouched host even when no dashboard event is published.
		_, selected := names[row.Name]
		selected = selected || names == nil
		encoded.Reset()
		if err := encoder.Encode(row); err == nil {
			serialized := encoded.Bytes()
			digest := sha256.Sum256(serialized[:len(serialized)-1]) // Encode appends one newline.
			version := binary.BigEndian.Uint64(digest[:8])
			rowsVersion ^= version
			if selected {
				row.Version = strconv.FormatUint(version, 16)
			}
		}
		if selected {
			rows = append(rows, row)
		}
	}
	return statusDashboardSummary{dashboardSummaryResponse: summary, Servers: rows, RowsVersion: strconv.FormatUint(rowsVersion, 16)}
}
