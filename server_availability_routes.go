package main

import (
	"net/http"

	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

func registerServerAvailabilityRoutes(r *gin.Engine, deps AppDeps, commands *serverpkg.CommandService) {
	r.PUT("/api/servers/:name/availability", func(c *gin.Context) {
		var request struct {
			Disabled *bool `json:"disabled"`
		}
		if err := c.ShouldBindJSON(&request); err != nil || request.Disabled == nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "disabled must be a boolean"})
			return
		}
		result := commands.SetServerDisabled(c.Param("name"), *request.Disabled)
		writeServerInventoryCommandResult(c, result, http.StatusOK, result.Server)
		if result.Succeeded() && deps.NotifyDashboardEvent != nil {
			deps.NotifyDashboardEvent("server-availability-changed")
		}
	})
}
