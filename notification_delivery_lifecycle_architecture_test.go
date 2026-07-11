package main

import (
	"os"
	"strings"
	"testing"
)

func TestNotificationDeliveryLifecycleArchitecture(t *testing.T) {
	auditSource, err := os.ReadFile("audit_service.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"go func", "http.NewRequest", "WebhookPayload", "time.After"} {
		if strings.Contains(string(auditSource), forbidden) {
			t.Errorf("audit adapter restores notification delivery implementation %q", forbidden)
		}
	}
	handlerSource, err := os.ReadFile("notification_service.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"http.NewRequest", "WebhookPayload", "time.After", "storeLastDelivery"} {
		if strings.Contains(string(handlerSource), forbidden) {
			t.Errorf("notification HTTP adapter restores lifecycle implementation %q", forbidden)
		}
	}
	lifecycleSource, err := os.ReadFile("internal/notifications/service.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, removed := range []string{"NotifyAuditEvent", "NotifyDeliveryIntent", "DeliverAuditEvent", "DeliverDeliveryIntent"} {
		if strings.Contains(string(lifecycleSource), removed) {
			t.Errorf("notification module retains obsolete delivery interface %q", removed)
		}
	}
	webserverSource, err := os.ReadFile("webserver.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(webserverSource), "closeNotificationDelivery(deliveryCtx, deps.NotificationService)") {
		t.Error("process shutdown does not close Notification Delivery Lifecycle")
	}
}
