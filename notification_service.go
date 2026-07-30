package main

import (
	"context"
	"errors"
	"log"
	"net/http"

	notificationpkg "debian-updater/internal/notifications"

	"github.com/gin-gonic/gin"
)

type NotificationService = notificationpkg.Service
type NotificationDeliveryLifecycle = notificationpkg.Lifecycle
type NotificationServiceDeps = notificationpkg.ServiceDeps
type NotificationSettings = notificationpkg.Settings
type NotificationSettingsUpdate = notificationpkg.SettingsUpdate
type NotificationSettingsResponse = notificationpkg.SettingsResponse
type NotificationSettingsValidationError = notificationpkg.ValidationError
type NotificationDeliveryStatus = notificationpkg.DeliveryStatus
type NotificationDeliveryDiagnostics = notificationpkg.DeliveryDiagnostics

func NewNotificationService(deps NotificationServiceDeps) *NotificationService {
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	if deps.EncryptSecret == nil {
		deps.EncryptSecret = encryptSecret
	}
	if deps.DecryptSecret == nil {
		deps.DecryptSecret = decryptSecret
	}
	return notificationpkg.NewService(deps)
}

func defaultNotificationService() *NotificationService {
	return NewNotificationService(NotificationServiceDeps{})
}

func closeNotificationDelivery(ctx context.Context, service NotificationDeliveryLifecycle) error {
	if service == nil {
		return nil
	}
	return service.Close(ctx)
}

func handleNotificationSettingsStatus(c *gin.Context, service NotificationDeliveryLifecycle) {
	if service == nil {
		service = defaultNotificationService()
	}
	settings, err := service.Settings()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load notification settings"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, settings)
}

func handleNotificationSettingsUpdate(c *gin.Context, service NotificationDeliveryLifecycle) {
	if service == nil {
		service = defaultNotificationService()
	}
	var req NotificationSettingsUpdate
	if err := c.ShouldBindJSON(&req); err != nil {
		audit(c, "notifications.settings", "settings", "notifications", "failure", "Invalid notification settings payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request payload"})
		return
	}
	settings, err := service.SaveSettings(req)
	if err != nil {
		var validationErr *NotificationSettingsValidationError
		if errors.As(err, &validationErr) {
			audit(c, "notifications.settings", "settings", "notifications", "failure", "Invalid notification settings", map[string]any{
				"webhook_url_intent": req.WebhookURLIntent,
			})
			c.JSON(http.StatusBadRequest, gin.H{"error": validationErr.Error()})
			return
		}
		audit(c, "notifications.settings", "settings", "notifications", "failure", "Failed to save notification settings", nil)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save notification settings"})
		return
	}
	audit(c, "notifications.settings", "settings", "notifications", "success", "Notification settings saved", map[string]any{
		"enabled":             settings.Enabled,
		"event_count":         len(settings.EventTypes),
		"webhook_configured":  settings.WebhookConfigured,
		"webhook_url_intent":  settings.WebhookURLIntent,
		"discord_enabled":     settings.Discord.Enabled,
		"discord_configured":  settings.Discord.Configured,
		"telegram_enabled":    settings.Telegram.Enabled,
		"telegram_configured": settings.Telegram.Configured,
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, settings)
}

func handleNotificationDeliveryDiagnostics(c *gin.Context, service NotificationDeliveryLifecycle) {
	if service == nil {
		service = defaultNotificationService()
	}
	diagnostics, err := service.DeliveryDiagnostics()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load notification delivery diagnostics"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, diagnostics)
}

func handleNotificationTest(c *gin.Context, service NotificationDeliveryLifecycle) {
	if service == nil {
		service = defaultNotificationService()
	}
	testCtx := c.Request.Context()
	destination := c.DefaultQuery("destination", notificationpkg.DestinationWebhook)
	switch destination {
	case notificationpkg.DestinationWebhook, notificationpkg.DestinationDiscord, notificationpkg.DestinationTelegram:
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "notification destination is not supported"})
		return
	}
	var status NotificationDeliveryStatus
	var err error
	if destination == notificationpkg.DestinationWebhook {
		status, err = service.TestDelivery(testCtx)
	} else if tester, ok := service.(notificationpkg.DestinationTester); ok {
		status, err = tester.TestDestination(testCtx, destination)
	} else {
		c.JSON(http.StatusBadRequest, gin.H{"error": "notification destination is not supported"})
		return
	}
	if err != nil {
		audit(c, "notifications.test", "settings", "notifications", "failure", "Notification test failed", map[string]any{
			"outcome":              status.Outcome,
			"attempts":             status.Attempts,
			"status_code":          status.StatusCode,
			"consecutive_failures": status.ConsecutiveFailures,
			"destination":          destination,
		})
		c.JSON(http.StatusBadRequest, gin.H{
			"error":         "notification test failed",
			"last_attempt":  status,
			"last_delivery": status,
		})
		return
	}
	audit(c, "notifications.test", "settings", "notifications", "success", "Notification test delivered", map[string]any{
		"outcome":              status.Outcome,
		"attempts":             status.Attempts,
		"status_code":          status.StatusCode,
		"duration_ms":          status.DurationMS,
		"consecutive_failures": status.ConsecutiveFailures,
		"destination":          destination,
	})
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"last_attempt": status, "last_delivery": status})
}
