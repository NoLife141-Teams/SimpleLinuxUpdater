package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"

	maintenancepkg "debian-updater/internal/maintenance"

	"github.com/gin-gonic/gin"
)

func publicMaintenanceSnapshotPayload(state maintenancepkg.Snapshot) gin.H {
	return gin.H{
		"active":     state.Active,
		"kind":       state.Kind,
		"started_at": state.StartedAt,
		"message":    state.Message,
	}
}

func writeMaintenanceBlockedSnapshotResponse(c *gin.Context, state maintenancepkg.Snapshot) {
	if c == nil {
		return
	}
	payload := publicMaintenanceSnapshotPayload(state)
	payload["error"] = "maintenance mode active"
	payload["maintenance"] = true
	if c.Request != nil {
		path := c.Request.URL.Path
		if strings.HasPrefix(path, "/api/") || path == "/metrics" {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, payload)
			return
		}
	}
	c.Header("Content-Type", "text/html; charset=utf-8")
	c.Status(http.StatusServiceUnavailable)
	_, _ = c.Writer.WriteString(maintenancePageHTMLFromSnapshot(state))
	c.Abort()
}

func maintenancePageHTMLFromSnapshot(state maintenancepkg.Snapshot) string {
	message := "Maintenance is in progress. Please wait while the updater finishes a backup operation."
	if strings.TrimSpace(state.Message) != "" {
		message = state.Message
	}
	kind := strings.ReplaceAll(strings.TrimSpace(state.Kind), "_", " ")
	if kind == "" {
		kind = "maintenance"
	}
	startedAtDisplay, timezoneLabel := formatTimestampForAppDisplayWithTimezone(state.StartedAt, defaultAppLocation(), appTimezoneLocalDisplayLabel)
	return fmt.Sprintf(`<!DOCTYPE html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Maintenance Mode</title>
  <link rel="stylesheet" href="/static/css/maintenance.css?v=vscode-dark-20260601b">
</head>
<body>
  <main>
    <div class="pulse" aria-hidden="true"></div>
    <h1>Maintenance Mode</h1>
    <p>%s</p>
    <div class="meta">
      <div class="label">Current Operation</div>
      <div class="value">%s</div>
    </div>
    <div class="meta">
      <div class="label">Started</div>
      <div class="value">%s</div>
    </div>
    <div class="meta">
      <div class="label">Timezone</div>
      <div class="value">%s</div>
    </div>
  </main>
  <script src="/static/js/maintenance.js"></script>
</body>
</html>`,
		html.EscapeString(message),
		html.EscapeString(kind),
		html.EscapeString(startedAtDisplay),
		html.EscapeString(timezoneLabel),
	)
}

func maintenanceBypassPath(path string) bool {
	switch {
	case path == "/api/maintenance":
		return true
	case strings.HasPrefix(path, "/static/"):
		return true
	default:
		return false
	}
}

func maintenanceExclusivePath(path string) bool {
	switch path {
	case "/api/backup/export", "/api/backup/restore":
		return true
	default:
		return false
	}
}
