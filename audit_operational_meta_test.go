package main

import (
	observabilitypkg "debian-updater/internal/observability"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestLargeAuditPreservesOperationalFacts(t *testing.T) {
	app := newIsolatedTestApp(t)
	app.authenticate(t)
	pkgs := make([]string, 200)
	for i := range pkgs {
		pkgs[i] = fmt.Sprintf("libreview-component-%03d", i)
	}
	meta := map[string]any{
		"approved_packages": pkgs, "approved_package_count": 200, "approval_scope": "security",
		"execution_duration_ms": int64(120000), "postcheck_failed": "post_apt_health",
		"postcheck_results": []map[string]any{{"name": "post_apt_health", "passed": false, "details": "APT check failed"}},
	}
	if err := app.Deps.AuditService.Record("admin", "", "update.complete", "server", "review-host", "failure", "Final status: error", meta); err != nil {
		t.Fatal(err)
	}
	var raw string
	if err := app.Deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action='update.complete'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var count int
	var apt string
	if err := app.Deps.DB().QueryRow("SELECT package_count,apt_status FROM server_health_snapshots WHERE server_name='review-host'").Scan(&count, &apt); err != nil {
		t.Fatal(err)
	}
	if count != 200 || apt != "critical" {
		t.Fatalf("unexpected preserved facts %d %s", count, apt)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatal(err)
	}
	if got, ok := observabilitypkg.MetaDurationMS(decoded); !ok || got != 120000 {
		t.Fatalf("duration=%v, valid=%v", got, ok)
	}
	if got := observabilitypkg.FailureCauseFromMeta(decoded, true); got != "postcheck:post_apt_health" {
		t.Fatalf("failure cause=%s", got)
	}
	if !strings.Contains(raw, `"_truncated":true`) {
		t.Fatal("expected truncation")
	}
	t.Logf("200 approved packages with failed APT postcheck recorded as package_count=%d, apt_status=%s; audit JSON wrapped as preview (%d bytes)", count, apt, len(raw))
}
