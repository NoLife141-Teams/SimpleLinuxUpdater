package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	apptimepkg "debian-updater/internal/apptime"

	"github.com/gin-gonic/gin"
)

type appTimeSQLiteStore struct{}

func (appTimeSQLiteStore) Load(context.Context) (string, error) {
	return getSettingValue(appTimezoneSetting)
}

func (appTimeSQLiteStore) Save(_ context.Context, value string) error {
	return upsertSettingValue(appTimezoneSetting, value)
}

type appTimeSystemDetector struct{}

func (appTimeSystemDetector) Detect() (*time.Location, string, error) {
	name, err := detectSystemTimezoneNameFunc()
	if err == nil {
		if offsetName, offsetLoc, ok := parseOffsetTimezoneLabel(name); ok {
			return offsetLoc, offsetName, nil
		}
		if loc, loadErr := time.LoadLocation(name); loadErr == nil {
			return loc, loc.String(), nil
		} else {
			err = fmt.Errorf("load detected system timezone %q: %w", name, loadErr)
		}
	}
	loc := defaultAppLocation()
	if loc == nil {
		loc = time.UTC
	}
	label := browserSafeTimezoneLabelForLocation(loc, time.Now())
	if strings.TrimSpace(label) == "" || strings.EqualFold(label, "Local") {
		label = "UTC"
		loc = time.UTC
	}
	return loc, label, err
}

func appTimeResponse(value apptimepkg.Interpretation) AppTimezoneResponse {
	return AppTimezoneResponse{Timezone: value.DisplayName, ResolvedTimezone: value.ResolvedName, EditableTimezone: value.EditableName}
}

func handleAppTimezoneStatusWithModule(module *apptimepkg.Module) gin.HandlerFunc {
	return func(c *gin.Context) { c.JSON(200, appTimeResponse(module.Current())) }
}

func handleAppTimezoneUpdateWithModule(module *apptimepkg.Module) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Timezone *string `json:"timezone"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "app_settings.timezone", "settings", "app_timezone", "failure", "Invalid app timezone payload", nil)
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if req.Timezone == nil {
			audit(c, "app_settings.timezone", "settings", "app_timezone", "failure", "Invalid app timezone payload", nil)
			c.JSON(400, gin.H{"error": "timezone is required"})
			return
		}
		value, err := module.Configure(c.Request.Context(), *req.Timezone)
		if err != nil {
			status := 500
			if strings.Contains(err.Error(), "invalid timezone") {
				status = 400
			}
			audit(c, "app_settings.timezone", "settings", "app_timezone", "failure", "Failed to save app timezone", map[string]any{"error": err.Error()})
			c.JSON(status, gin.H{"error": err.Error()})
			return
		}
		audit(c, "app_settings.timezone", "settings", "app_timezone", "success", "App timezone saved", map[string]any{"timezone": value.EditableName})
		c.JSON(200, appTimeResponse(value))
	}
}
