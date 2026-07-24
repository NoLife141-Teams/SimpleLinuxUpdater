package main

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
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
	job, err := jm.GetJobWithLogs(c.Param("id"))
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

func handleJobLogsWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	jm := deps.CurrentJobManager()
	if jm == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "job manager unavailable"})
		return
	}
	afterSequence := int64(0)
	if raw := strings.TrimSpace(c.Query("after_seq")); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || value < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "after_seq must be a non-negative integer"})
			return
		}
		afterSequence = value
	}
	limit := 100
	if raw := strings.TrimSpace(c.Query("limit")); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 500 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be an integer in [1,500]"})
			return
		}
		limit = value
	}
	page, err := jm.ReadLogPage(c.Param("id"), afterSequence, limit)
	if err != nil {
		if err == sql.ErrNoRows {
			c.JSON(http.StatusNotFound, gin.H{"error": "job not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load job logs"})
		return
	}
	c.JSON(http.StatusOK, page)
}

func handleJobDetailWithDeps(c *gin.Context, deps AppDeps) {
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
	c.JSON(http.StatusOK, gin.H{
		"job":        job,
		"report_url": fmt.Sprintf("/api/reports/jobs/%s", url.PathEscape(job.ID)),
	})
}
