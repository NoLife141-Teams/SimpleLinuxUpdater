package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"debian-updater/internal/deferrals"
	"debian-updater/internal/policies"
	"github.com/gin-gonic/gin"
)

const packageDeferralMaxRequestBytes = 16 * 1024

// Policy edits can change inherited rules. Serialize their commits with the
// final local check and protected remote mutation, just like rule edits.
func packageDeferralPolicyGate(deps AppDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method != "GET" && c.Request.Method != "HEAD" && strings.HasPrefix(c.Request.URL.Path, "/api/update-policies") {
			deps.PackageDeferralGate.Lock()
			defer deps.PackageDeferralGate.Unlock()
		}
		c.Next()
	}
}

func packageDeferralLoader(deps AppDeps) func(Server, string) (deferrals.Snapshot, error) {
	return func(server Server, jobID string) (deferrals.Snapshot, error) {
		if current, ok := deps.ServerState.FindByName(server.Name); ok {
			server = current
		}
		rules, err := deferrals.List(context.Background(), deps.DB())
		if err != nil {
			return deferrals.Snapshot{}, err
		}
		list, err := deps.PolicyRepository.ListPolicies()
		if err != nil {
			return deferrals.Snapshot{}, err
		}
		overrides, err := deps.PolicyRepository.LoadAllOverrides()
		if err != nil {
			return deferrals.Snapshot{}, err
		}
		// The manifest fixes the originating policy identity. Its live deferrals
		// remain applicable even after its matching configuration is edited.
		var meta scheduledJobMeta
		if jobID != "" {
			manager := deps.CurrentJobManager()
			if manager == nil {
				return deferrals.Snapshot{}, fmt.Errorf("job context unavailable for package deferrals")
			}
			job, e := manager.GetJob(jobID)
			if e != nil {
				return deferrals.Snapshot{}, fmt.Errorf("load package deferral job context: %w", e)
			}
			if strings.TrimSpace(job.MetaJSON) != "" {
				if e = json.Unmarshal([]byte(job.MetaJSON), &meta); e != nil {
					return deferrals.Snapshot{}, fmt.Errorf("invalid package deferral job context: %w", e)
				}
			}
		}

		matched := map[int64]string{}
		for _, p := range list {
			if p.ID == meta.PolicyID || deps.PolicyService.PolicyMatchesServer(p, server, policies.MatchContext{Overrides: overrides}) {
				matched[p.ID] = fmt.Sprintf("%s (revision %d)", p.Name, p.Revision)
			}
		}
		if meta.PolicyID > 0 {
			if _, ok := matched[meta.PolicyID]; !ok {
				matched[meta.PolicyID] = fmt.Sprintf("%s (manifest revision %d)", meta.PolicyName, meta.PolicyRevision)
			}
		}
		snapshot, err := deferrals.Resolve(rules, server.Name, matched, deps.Now())
		if err == nil && deps.ApplicationTime != nil {
			for i := range snapshot.Rules {
				snapshot.Rules[i].ExpiresAtDisplay, _ = deps.ApplicationTime.Current().Format(snapshot.Rules[i].ExpiresAt, "")
			}
		}
		if err == nil {
			err = deferrals.RecordSnapshot(context.Background(), deps.DB(), snapshot)
		}
		return snapshot, err
	}
}

func registerPackageDeferralRoutes(r *gin.Engine, deps AppDeps) {
	r.GET("/api/package-deferral-snapshots/:revision", func(c *gin.Context) {
		snapshot, err := deferrals.LoadSnapshot(c.Request.Context(), deps.DB(), c.Param("revision"))
		if err != nil {
			c.JSON(404, gin.H{"error": "Package deferral snapshot not found"})
			return
		}
		c.JSON(200, snapshot)
	})
	r.GET("/api/package-deferrals", func(c *gin.Context) {
		rules, err := deferrals.List(c.Request.Context(), deps.DB())
		if err != nil {
			c.JSON(500, gin.H{"error": "Package deferrals unavailable"})
			return
		}
		for i := range rules {
			if rules[i].ServerName != "" {
				rules[i].Origin = "Server: " + rules[i].ServerName
			} else {
				rules[i].Origin = "Policy #" + strconv.FormatInt(rules[i].PolicyID, 10)
			}
			rules[i].ExpiresAtDisplay, _ = deps.ApplicationTime.Current().Format(rules[i].ExpiresAt, "")
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(200, gin.H{"rules": rules})
	})
	r.GET("/api/servers/:name/package-deferrals", func(c *gin.Context) {
		server, ok := deps.ServerState.FindByName(c.Param("name"))
		if !ok {
			c.JSON(404, gin.H{"error": "Server not found"})
			return
		}
		jobID := ""
		if status := deps.ServerState.CurrentStatusSnapshot(server.Name); status != nil && (status.ActionRunning || status.Status == "pending_approval") {
			jobID = status.JobID
		}
		snapshot, err := packageDeferralLoader(deps)(server, jobID)
		if err != nil {
			c.JSON(500, gin.H{"error": "Package deferrals unavailable"})
			return
		}
		for i := range snapshot.Rules {
			snapshot.Rules[i].ExpiresAtDisplay, _ = deps.ApplicationTime.Current().Format(snapshot.Rules[i].ExpiresAt, "")
		}
		c.JSON(200, snapshot)
	})
	r.POST("/api/package-deferrals", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, packageDeferralMaxRequestBytes)
		var rule deferrals.Rule
		if c.ShouldBindJSON(&rule) != nil {
			c.JSON(400, gin.H{"error": "Invalid package deferral"})
			return
		}
		deps.PackageDeferralGate.Lock()
		defer deps.PackageDeferralGate.Unlock()
		if rule.ServerName != "" {
			if _, ok := deps.ServerState.FindByName(rule.ServerName); !ok {
				c.JSON(404, gin.H{"error": "Server not found"})
				return
			}
		} else {
			if _, err := deps.PolicyRepository.GetPolicy(rule.PolicyID); err != nil {
				c.JSON(404, gin.H{"error": "Policy not found"})
				return
			}
		}
		saved, err := deferrals.Create(c.Request.Context(), deps.DB(), rule, actorFromContext(c), deps.Now())
		if err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		auditWithService(deps.AuditService, c, "package_deferral.create", "package_deferral", saved.ID, "success", "Package deferred; security fixes may remain exposed", map[string]any{"rule": saved})
		deps.NotifyDashboardEvent("package_deferral.changed")
		c.JSON(201, saved)
	})
	r.DELETE("/api/package-deferrals/:id", func(c *gin.Context) {
		deps.PackageDeferralGate.Lock()
		defer deps.PackageDeferralGate.Unlock()
		rules, err := deferrals.List(c.Request.Context(), deps.DB())
		if err != nil {
			c.JSON(500, gin.H{"error": "Package deferrals unavailable"})
			return
		}
		var old deferrals.Rule
		for _, rule := range rules {
			if rule.ID == c.Param("id") {
				old = rule
				break
			}
		}
		if err = deferrals.Delete(c.Request.Context(), deps.DB(), c.Param("id")); err != nil {
			if err == sql.ErrNoRows {
				c.JSON(404, gin.H{"error": "Deferral not found"})
			} else {
				c.JSON(500, gin.H{"error": "Could not remove deferral"})
			}
			return
		}
		auditWithService(deps.AuditService, c, "package_deferral.delete", "package_deferral", old.ID, "success", "Package deferral ended early", map[string]any{"rule": old})
		deps.NotifyDashboardEvent("package_deferral.changed")
		c.JSON(200, gin.H{"message": "Package deferral ended early"})
	})
}
