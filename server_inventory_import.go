package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	auditpkg "debian-updater/internal/audit"
	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

func registerServerInventoryImportRoutes(r *gin.Engine, deps AppDeps, commands *serverpkg.CommandService) {
	r.POST("/api/servers/import/preview", func(c *gin.Context) {
		rows, err := serverpkg.ParseInventoryImport(c.Request.Body, c.Query("format"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		preview, err := commands.PreviewImport(rows)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusOK, preview)
	})
	r.POST("/api/servers/import", func(c *gin.Context) {
		selection, err := serverpkg.DecodeImportSelection(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := commands.CommitImport(selection)
		for _, event := range inventoryImportAuditRecords(result.Audit) {
			audit(c, event.Action, event.TargetType, event.TargetName, event.Status, event.Message, event.Meta)
		}
		status := http.StatusCreated
		switch result.Outcome {
		case serverpkg.CommandOutcomeConflict:
			status = http.StatusConflict
		case serverpkg.CommandOutcomeInvalid:
			status = http.StatusBadRequest
		case serverpkg.CommandOutcomeFailed:
			status = http.StatusInternalServerError
		}
		if result.Outcome == serverpkg.CommandOutcomeSuccess {
			deps.NotifyDashboardEvent("servers-imported")
		}
		c.JSON(status, result)
	})
	// Fixed examples contain documentation-only hosts, never current inventory.
	r.GET("/api/servers/import/example/:format", func(c *gin.Context) {
		format := c.Param("format")
		var data, contentType string
		switch format {
		case "csv":
			contentType = "text/csv; charset=utf-8"
			data = "name,host,port,user,tags\nweb-example,192.0.2.10,22,updater,\"[\"\"production\"\",\"\"web\"\"]\"\nipv6-example,2001:db8::20,2222,root,\"[\"\"staging\"\"]\"\n"
		case "json":
			contentType = "application/json; charset=utf-8"
			data = "[\n  {\"name\":\"web-example\",\"host\":\"192.0.2.10\",\"port\":22,\"user\":\"updater\",\"tags\":[\"production\",\"web\"]},\n  {\"name\":\"ipv6-example\",\"host\":\"2001:db8::20\",\"port\":2222,\"user\":\"root\",\"tags\":[\"staging\"]}\n]\n"
		default:
			c.Status(http.StatusNotFound)
			return
		}
		c.Header("Content-Disposition", "attachment; filename=inventory-example."+strings.TrimSpace(format))
		c.Data(http.StatusOK, contentType, []byte(data))
	})
}

// Large imports must retain every created name as structured audit facts.
// Split only oversized records, preserving the batch identity and total counts
// in each numbered part instead of relying on the audit truncation preview.
func inventoryImportAuditRecords(event serverpkg.CommandAudit) []serverpkg.CommandAudit {
	raw, err := json.Marshal(event.Meta)
	names, ok := event.Meta["created_names"].([]string)
	if err != nil || len(raw) <= auditpkg.MetaMaxLen || !ok || len(names) == 0 {
		return []serverpkg.CommandAudit{event}
	}
	metaFor := func(names []string, part, parts int) map[string]any {
		meta := make(map[string]any, len(event.Meta)+2)
		for key, value := range event.Meta {
			meta[key] = value
		}
		meta["created_names"], meta["part"], meta["parts"] = names, part, parts
		return meta
	}
	chunks := [][]string{}
	start := 0
	for end := 1; end <= len(names); end++ {
		// Reserve the largest possible part numbers before measuring. Names
		// passed Server Inventory Command validation and each fits on its own.
		candidate, _ := json.Marshal(metaFor(names[start:end], serverpkg.MaxImportRows, serverpkg.MaxImportRows))
		if len(candidate) > auditpkg.MetaMaxLen && end-start > 1 {
			chunks = append(chunks, names[start:end-1])
			start = end - 1
		}
	}
	chunks = append(chunks, names[start:])
	records := make([]serverpkg.CommandAudit, 0, len(chunks))
	for index, chunk := range chunks {
		record := event
		record.Message = fmt.Sprintf("%s (part %d of %d)", event.Message, index+1, len(chunks))
		record.Meta = metaFor(chunk, index+1, len(chunks))
		records = append(records, record)
	}
	return records
}
