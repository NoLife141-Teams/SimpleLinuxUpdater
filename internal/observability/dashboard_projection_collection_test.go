package observability

import (
	"fmt"
	"testing"
	"time"

	"debian-updater/internal/updates"
)

func TestDashboardProjectionCollectionCollectsTypedUpdateHistory(t *testing.T) {
	db, path := newTestDB(t, "dashboard-projection-collection-history.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	insertAudit(t, db, now.Add(-10*time.Minute).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "newest", map[string]any{
		"execution_duration_ms": 300,
	})
	insertAudit(t, db, now.Add(-20*time.Minute).Format(time.RFC3339), "update.complete", "failure", "server", "srv-a", "failed", map[string]any{
		"duration_ms":     100,
		"precheck_failed": "apt_health",
	})
	olderOverlayAt := now.Add(-30 * time.Minute).Format(time.RFC3339)
	insertAudit(t, db, olderOverlayAt, "update.complete", "success", "server", "srv-overlay", "overlay", map[string]any{
		"postcheck_results": []updates.PrecheckResult{{Name: updates.PostcheckNameAptHealth, Passed: false, Details: "apt unhealthy"}},
	})
	if _, err := db.Exec(
		`INSERT INTO audit_events (created_at, action, status, target_type, target_name, message, meta_json)
		 VALUES (?, 'update.complete', 'success', 'server', 'srv-overlay', 'malformed', '{')`,
		now.Add(-15*time.Minute).Format(time.RFC3339),
	); err != nil {
		t.Fatalf("insert malformed audit event: %v", err)
	}

	collector := newDashboardProjectionCollector(testService(db, path).EnsureDeps())
	got, err := collector.collectUpdateHistory(
		now.Add(-24*time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339),
		time.UTC,
		"UTC",
	)
	if err != nil {
		t.Fatalf("collectUpdateHistory() error = %v", err)
	}

	srvA := got["srv-a"]
	if srvA.lastSuccess == nil || srvA.lastSuccess.Message != "newest" || srvA.lastSuccess.DurationMS != 300 {
		t.Fatalf("srv-a last success = %+v, want newest success", srvA.lastSuccess)
	}
	if srvA.lastFailure == nil || srvA.lastFailure.FailureCause != "precheck:apt_health" {
		t.Fatalf("srv-a last failure = %+v, want typed failure cause", srvA.lastFailure)
	}
	if srvA.samples != 2 || srvA.durationSum != 400 {
		t.Fatalf("srv-a duration = %v across %d samples, want 400 across 2", srvA.durationSum, srvA.samples)
	}
	if srvA.lastSuccess.FinishedAtDisplay != "display:"+srvA.lastSuccess.FinishedAt {
		t.Fatalf("srv-a display time = %q, want formatted completion time", srvA.lastSuccess.FinishedAtDisplay)
	}

	overlay := got["srv-overlay"].healthOverlay
	if overlay.collectedAt != olderOverlayAt || len(overlay.results) != 1 || overlay.results[0].Name != updates.PostcheckNameAptHealth {
		t.Fatalf("srv-overlay health overlay = %+v, want newest valid typed metadata", overlay)
	}
}

func TestDashboardProjectionCollectionCollectsTypedCommandHistory(t *testing.T) {
	db, path := newTestDB(t, "dashboard-projection-collection-commands.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 10; i++ {
		createdAt := now.Add(-time.Duration(i) * time.Minute).Format(time.RFC3339)
		if _, err := db.Exec(
			`INSERT INTO audit_events (created_at, actor, action, target_type, target_name, status, message)
			 VALUES (?, ?, ?, 'server', 'srv-a', 'success', ?)`,
			createdAt,
			fmt.Sprintf("actor-%d", i),
			fmt.Sprintf("server.command.%d", i),
			fmt.Sprintf("message-%d", i),
		); err != nil {
			t.Fatalf("insert command history %d: %v", i, err)
		}
	}
	insertAudit(t, db, now.Format(time.RFC3339), "settings.changed", "success", "app", "", "ignored", nil)

	collector := newDashboardProjectionCollector(testService(db, path).EnsureDeps())
	got, err := collector.collectCommandHistory(
		now.Add(-24*time.Hour).Format(time.RFC3339),
		now.Format(time.RFC3339),
		time.UTC,
		"UTC",
	)
	if err != nil {
		t.Fatalf("collectCommandHistory() error = %v", err)
	}

	items := got["srv-a"]
	if len(items) != 8 {
		t.Fatalf("srv-a command history length = %d, want 8", len(items))
	}
	if items[0].Action != "server.command.0" || items[0].Actor != "actor-0" || items[0].Message != "message-0" {
		t.Fatalf("first command = %+v, want newest complete command facts", items[0])
	}
	if items[7].Action != "server.command.7" {
		t.Fatalf("last retained command = %+v, want eighth-newest command", items[7])
	}
	if items[0].CreatedAtDisplay != "display:"+items[0].CreatedAt {
		t.Fatalf("display time = %q, want formatted command time", items[0].CreatedAtDisplay)
	}
	if _, ok := got[""]; ok {
		t.Fatalf("non-server audit event leaked into command history: %+v", got[""])
	}
}
