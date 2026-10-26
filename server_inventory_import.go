package main

import (
	"net/http"
	"strings"

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
		audit(c, result.Audit.Action, result.Audit.TargetType, result.Audit.TargetName, result.Audit.Status, result.Audit.Message, result.Audit.Meta)
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
