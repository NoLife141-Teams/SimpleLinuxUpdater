package main

import (
	"os"
	"strings"
	"testing"

	auditpkg "debian-updater/internal/audit"
	updatespkg "debian-updater/internal/updates"
)

func TestMaintenanceCompletionFromAuditEventTranslatesSupportedServerActions(t *testing.T) {
	tests := []struct {
		action string
		kind   updatespkg.MaintenanceKind
		ok     bool
	}{
		{action: updateCompleteAction, kind: updatespkg.MaintenanceKindUpdate, ok: true},
		{action: "schedule.run.completed", kind: updatespkg.MaintenanceKindScheduledRun, ok: true},
		{action: "schedule.run.failed", kind: updatespkg.MaintenanceKindScheduledRun, ok: true},
		{action: "update.started", ok: false},
	}
	for _, test := range tests {
		t.Run(test.action, func(t *testing.T) {
			got, ok := maintenanceCompletionFromAuditEvent(auditpkg.Event{
				Action: test.action, TargetType: "server", TargetName: "srv-a",
				CreatedAt: "2026-07-10T12:00:00Z", Status: "success", MetaJSON: `{"x":1}`,
			})
			if ok != test.ok {
				t.Fatalf("ok = %t, want %t", ok, test.ok)
			}
			if ok && (got.Kind != test.kind || got.ServerName != "srv-a" || got.RawJSON != `{"x":1}`) {
				t.Fatalf("completion = %+v, want translated domain facts", got)
			}
		})
	}
}

func TestHealthSnapshotCaptureArchitectureBoundary(t *testing.T) {
	auditSource, err := os.ReadFile("audit_service.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"HealthSnapshotRecord{", "updateHealthFromResults(", "SaveHealthSnapshot("} {
		if strings.Contains(string(auditSource), forbidden) {
			t.Errorf("audit adapter restores capture implementation %q", forbidden)
		}
	}
	dashboardSource, err := os.ReadFile("internal/observability/dashboard_projection.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"CaptureFacts(", "CaptureCompletion(", "SaveHealthSnapshot("} {
		if strings.Contains(string(dashboardSource), forbidden) {
			t.Errorf("Dashboard Projection restores health-history write %q", forbidden)
		}
	}
}
