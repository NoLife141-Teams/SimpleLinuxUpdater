package main

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"debian-updater/internal/postchecks"
	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

const applicationPostcheckTestTimeout = 11 * time.Minute

func applicationPostcheckLoader(db func() *sql.DB, state *serverpkg.State) func(Server) (*postchecks.Snapshot, error) {
	return func(server Server) (*postchecks.Snapshot, error) {
		if state != nil {
			if current, ok := state.FindByName(server.Name); ok {
				server = current
			}
		}
		c, err := postchecks.Load(context.Background(), db())
		if err != nil {
			return nil, err
		}
		return postchecks.Resolve(c, server.Name, server.Tags)
	}
}
func registerApplicationPostcheckRoutes(r *gin.Engine, deps AppDeps) {
	r.GET("/api/postcheck-profiles", func(c *gin.Context) {
		config, err := postchecks.Load(c.Request.Context(), deps.DB())
		if err != nil {
			c.JSON(500, gin.H{"error": "Post-check profiles unavailable"})
			return
		}
		c.Header("Cache-Control", "no-store")
		c.JSON(200, config)
	})
	r.PUT("/api/postcheck-profiles", func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 256*1024)
		var config postchecks.Config
		if c.ShouldBindJSON(&config) != nil {
			c.JSON(400, gin.H{"error": "Invalid post-check profiles"})
			return
		}
		if err := postchecks.Validate(config); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		config, err := postchecks.Save(c.Request.Context(), deps.DB(), config)
		if err != nil {
			if errors.Is(err, postchecks.ErrConflict) {
				c.JSON(409, gin.H{"error": err.Error()})
			} else {
				c.JSON(500, gin.H{"error": "Could not save post-check profiles"})
			}
			return
		}
		auditWithService(deps.AuditService, c, "postchecks.configure", "settings", "application_postchecks", "success", "Application post-check profiles saved", map[string]any{"revision": config.Revision, "profile_count": len(config.Profiles)})
		c.JSON(200, config)
	})
	r.GET("/api/servers/:name/postchecks", func(c *gin.Context) {
		server, ok := deps.ServerState.FindByName(c.Param("name"))
		if !ok {
			c.JSON(404, gin.H{"error": "Server not found"})
			return
		}
		profile, err := applicationPostcheckLoader(deps.DB, deps.ServerState)(server)
		if err != nil {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"profile": profile})
	})
	r.POST("/api/servers/:name/postchecks/test", func(c *gin.Context) {
		server, ok := deps.ServerState.FindByName(c.Param("name"))
		if !ok {
			c.JSON(404, gin.H{"error": "Server not found"})
			return
		}
		if server.Disabled {
			c.JSON(409, gin.H{"error": "Enable this Server before testing its checks"})
			return
		}
		release, admitted := deps.HostFactsRefreshAdmission.TryRefresh(server.Name)
		if !admitted {
			c.JSON(409, gin.H{"error": "A host observation is already in progress"})
			return
		}
		defer release()
		server, previous, err := deps.ServerState.BeginTransientAction(server.Name, runtimepkg.StatusApplicationCheck)
		if err != nil {
			code := http.StatusConflict
			if errors.Is(err, sql.ErrNoRows) {
				code = http.StatusNotFound
			}
			c.JSON(code, gin.H{"error": err.Error()})
			return
		}
		defer func() {
			deps.ServerState.RestoreStatusSnapshot(server.Name, previous)
			if deps.NotifyDashboardEvent != nil {
				deps.NotifyDashboardEvent("postchecks.test.completed")
			}
		}()
		if deps.NotifyDashboardEvent != nil {
			deps.NotifyDashboardEvent("postchecks.test.started")
		}
		profile, err := applicationPostcheckLoader(deps.DB, deps.ServerState)(server)
		if err != nil {
			c.JSON(409, gin.H{"error": err.Error()})
			return
		}
		if profile == nil {
			c.JSON(409, gin.H{"error": "No post-check profile is assigned"})
			return
		}
		finish, ok := beginHTTPTransferBudget(c, false, true)
		if !ok {
			return
		}
		defer finish()
		ctx, cancel := context.WithTimeout(c.Request.Context(), applicationPostcheckTestTimeout)
		defer cancel()
		results, err := deps.UpdateService.TestApplicationChecks(ctx, server, profile)
		if err != nil {
			c.JSON(502, gin.H{"error": "Read-only post-check test could not complete"})
			return
		}
		auditWithService(deps.AuditService, c, "postchecks.test", "server", server.Name, "success", "Read-only application checks completed", map[string]any{"profile_id": profile.ID, "profile_revision": profile.Revision, "results": results})
		c.JSON(200, gin.H{"server_name": server.Name, "profile": profile, "results": results})
	})
}
