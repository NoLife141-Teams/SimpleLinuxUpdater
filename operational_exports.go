package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	auditpkg "debian-updater/internal/audit"
	serverpkg "debian-updater/internal/servers"
	"github.com/gin-gonic/gin"
)

func parseAuditQuery(values url.Values) (AuditListFilter, error) {
	f := AuditListFilter{Actor: strings.TrimSpace(values.Get("actor")), Category: strings.TrimSpace(values.Get("category")), Order: strings.TrimSpace(values.Get("order")), TargetType: strings.TrimSpace(values.Get("target_type")), TargetName: strings.TrimSpace(values.Get("target_name")), Action: strings.TrimSpace(values.Get("action")), Status: strings.TrimSpace(values.Get("status")), FailureCause: strings.TrimSpace(values.Get("failure_cause")), From: strings.TrimSpace(values.Get("from")), To: strings.TrimSpace(values.Get("to"))}
	if f.Category != "" && f.Category != AuditListCategoryAdminActivity {
		return f, errors.New("invalid audit category")
	}
	if f.Order != "" && f.Order != auditpkg.ListOrderCreatedAt {
		return f, errors.New("invalid audit order")
	}
	if f.FailureCause != "" && !auditpkg.ValidFailureCause(f.FailureCause) {
		return f, errors.New("invalid failure cause")
	}
	for _, value := range []*string{&f.From, &f.To} {
		if *value != "" {
			normalized, err := normalizeAuditFilterTimestamp(*value)
			if err != nil {
				return f, errors.New("invalid audit timestamp; expected RFC3339")
			}
			*value = normalized
		}
	}
	if f.From != "" && f.To != "" && f.From > f.To {
		return f, errors.New("from must precede to")
	}
	return f, nil
}
func decodeExportRequest(c *gin.Context, value any) bool {
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if decoder.Decode(value) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid export request"})
		return false
	}
	return true
}
func exportHeaders(c *gin.Context, filename, format string) {
	c.Header("Cache-Control", "no-store")
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("X-SLU-Export-Format", format)
	c.Header("X-SLU-Export-Version", "1")
}
func registerOperationalExportRoutes(r *gin.Engine, deps AppDeps) {
	r.POST("/api/servers/export", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request serverpkg.InventoryExportRequest
		if !decodeExportRequest(c, &request) {
			return
		}
		raw, err := deps.ServerInventoryService.ExportInventory(request)
		if err != nil {
			status := http.StatusConflict
			if errors.Is(err, serverpkg.ErrInventoryExportLimit) {
				status = http.StatusRequestEntityTooLarge
			}
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		exportHeaders(c, "inventory-v1.json", "simplelinuxupdater-inventory")
		c.Header("X-SLU-Export-Count", strconv.Itoa(request.Count))
		c.Header("Content-Length", strconv.Itoa(len(raw)))
		_ = http.NewResponseController(c.Writer).SetWriteDeadline(time.Now().Add(30 * time.Second))
		c.Data(http.StatusOK, "application/json; charset=utf-8", raw)
	})
	r.POST("/api/audit-events/export", func(c *gin.Context) {
		c.Header("Cache-Control", "no-store")
		var request struct {
			Format  string            `json:"format"`
			Filters map[string]string `json:"filters"`
		}
		if !decodeExportRequest(c, &request) {
			return
		}
		if request.Format != "json" && request.Format != "csv" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "format must be json or csv"})
			return
		}
		values := url.Values{}
		allowed := map[string]bool{"actor": true, "category": true, "order": true, "target_type": true, "target_name": true, "action": true, "status": true, "failure_cause": true, "from": true, "to": true}
		for key, value := range request.Filters {
			if !allowed[key] {
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid audit filter"})
				return
			}
			values.Set(key, value)
		}
		filter, err := parseAuditQuery(values)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		file, err := deps.AuditService.PrepareExport(c.Request.Context(), filter, request.Format)
		if err != nil {
			status := http.StatusInternalServerError
			message := "failed to prepare audit export"
			if errors.Is(err, auditpkg.ErrExportLimit) {
				status = http.StatusRequestEntityTooLarge
				message = err.Error()
			}
			if errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusRequestTimeout
				message = "audit export exceeded 15 second preparation budget; narrow the search"
			}
			if errors.Is(err, context.Canceled) {
				return
			}
			c.JSON(status, gin.H{"error": message})
			return
		}
		defer file.Close()
		stat, err := file.Stat()
		if err != nil {
			c.JSON(500, gin.H{"error": "failed to read export"})
			return
		}
		exportHeaders(c, "audit-v1."+request.Format, "simplelinuxupdater-audit")
		c.Header("Content-Length", strconv.FormatInt(stat.Size(), 10))
		c.Header("X-SLU-Export-Count", strconv.Itoa(file.Count))
		contentType := "application/json; charset=utf-8"
		if request.Format == "csv" {
			contentType = "text/csv; charset=utf-8"
		}
		c.Header("Content-Type", contentType)
		// Never retain a database cursor for a slow downloader. A failed transfer is
		// detectable by Content-Length and terminates within this write deadline.
		controller := http.NewResponseController(c.Writer)
		_ = controller.SetWriteDeadline(time.Now().Add(30 * time.Second))
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		_, _ = io.Copy(c.Writer, &exportContextReader{ctx: ctx, reader: file})
	})
}

type exportContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *exportContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
