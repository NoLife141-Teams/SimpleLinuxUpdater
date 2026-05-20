package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func markdownReportFilename(prefix, id string) string {
	clean := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, strings.TrimSpace(id))
	if clean == "" {
		clean = "report"
	}
	return fmt.Sprintf("%s-%s.md", prefix, clean)
}

func writeMarkdownDownload(c *gin.Context, filename string, body string) {
	c.Header("Content-Type", "text/markdown; charset=utf-8")
	c.Header("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	c.String(http.StatusOK, body)
}

func handleAuditReportWithService(c *gin.Context, service *AuditService) {
	if service == nil {
		service = NewDefaultAppDeps().AuditService
	}
	evt, err := service.LoadByID(c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "audit event not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load audit event"})
		return
	}
	writeMarkdownDownload(c, markdownReportFilename("audit", fmt.Sprintf("%d", evt.ID)), service.BuildAuditMarkdownReport(evt))
}

func handleJobReportWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	jm := deps.CurrentJobManager()
	if jm == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "job manager unavailable"})
		return
	}
	job, err := jm.GetJob(c.Param("id"))
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load job"})
		return
	}
	writeMarkdownDownload(c, markdownReportFilename("job", job.ID), deps.AuditService.BuildJobMarkdownReport(job))
}
