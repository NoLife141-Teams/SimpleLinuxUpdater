package main

import (
	"log"
	"net/http"

	"debian-updater/internal/observability"
	"github.com/gin-gonic/gin"
)

func handleSecurityActions(c *gin.Context, deps AppDeps) {
	query, err := observability.ParseSecurityActionQuery(c.Request.URL.Query())
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid security action filters or pagination"})
		return
	}
	summary, err := deps.ObservabilityService.BuildDashboardSummaryContextWithOptions(c.Request.Context(), "7d", deps.Now(), observability.DashboardSummaryOptions{OmitCommandHistory: true})
	if err != nil {
		if c.Request.Context().Err() == nil {
			log.Printf("Build security actions: %v", err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Security evidence unavailable"})
		}
		return
	}
	setNoStoreHeaders(c)
	c.JSON(http.StatusOK, observability.BuildSecurityActions(summary, deps.SecurityIntel.Snapshot(), query, deps.Now()))
}
