package main

import (
	"log"
	"net/http"

	notificationpkg "debian-updater/internal/notifications"

	"github.com/gin-gonic/gin"
)

type NotificationService = notificationpkg.Service
type NotificationServiceDeps = notificationpkg.ServiceDeps
type NotificationSettings = notificationpkg.Settings
type NotificationSettingsResponse = notificationpkg.SettingsResponse
type NotificationDeliveryStatus = notificationpkg.DeliveryStatus

func NewNotificationService(deps NotificationServiceDeps) *NotificationService {
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return notificationpkg.NewService(deps)
}

func defaultNotificationService() *NotificationService {
	return NewNotificationService(NotificationServiceDeps{})
}

func handleNotificationSettingsStatus(c *gin.Context, service *NotificationService) {
	if service == nil {
		service = defaultNotificationService()
	}
	settings, err := service.Settings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load notification settings"})
		return
	}
	c.JSON(http.StatusOK, settings)
}

func handleNotificationSettingsUpdate(c *gin.Context, service *NotificationService) {
	if service == nil {
		service = defaultNotificationService()
	}
	var req NotificationSettings
	if err := c.ShouldBindJSON(&req); err != nil {
		audit(c, "notifications.settings", "settings", "notifications", "failure", "Invalid notification settings payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	settings, err := service.SaveSettings(req)
	if err != nil {
		audit(c, "notifications.settings", "settings", "notifications", "failure", "Failed to save notification settings", map[string]any{"error": err.Error()})
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	audit(c, "notifications.settings", "settings", "notifications", "success", "Notification settings saved", map[string]any{
		"enabled":     settings.Enabled,
		"event_count": len(settings.EventTypes),
	})
	c.JSON(http.StatusOK, settings)
}

func handleNotificationTest(c *gin.Context, service *NotificationService) {
	if service == nil {
		service = defaultNotificationService()
	}
	testCtx := c.Request.Context()
	status, err := service.TestDelivery(testCtx)
	if err != nil {
		audit(c, "notifications.test", "settings", "notifications", "failure", "Notification test failed", map[string]any{"error": err.Error()})
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "notification test failed",
			"last_delivery": status,
		})
		return
	}
	audit(c, "notifications.test", "settings", "notifications", "success", "Notification test delivered", map[string]any{
		"attempts":    status.Attempts,
		"status_code": status.StatusCode,
	})
	c.JSON(http.StatusOK, gin.H{"last_delivery": status})
}
