package observability

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"

	"golang.org/x/crypto/bcrypt"
	_ "modernc.org/sqlite"
)

func newTestDB(t *testing.T, name string) (*sql.DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`
		CREATE TABLE audit_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TEXT NOT NULL,
			actor TEXT NOT NULL DEFAULT '',
			action TEXT NOT NULL,
			target_type TEXT NOT NULL DEFAULT '',
			target_name TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT '',
			message TEXT NOT NULL DEFAULT '',
			meta_json TEXT NOT NULL DEFAULT '',
			request_id TEXT NOT NULL DEFAULT '',
			client_ip TEXT NOT NULL DEFAULT ''
		);
		CREATE TABLE settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		);
	`)
	if err != nil {
		t.Fatalf("create test schema: %v", err)
	}
	return db, path
}

func insertAudit(t *testing.T, db *sql.DB, createdAt, action, status, targetType, targetName, message string, meta map[string]any) {
	t.Helper()
	metaJSON := ""
	if meta != nil {
		raw, err := json.Marshal(meta)
		if err != nil {
			t.Fatalf("json.Marshal() error = %v", err)
		}
		metaJSON = string(raw)
	}
	if _, err := db.Exec(
		`INSERT INTO audit_events (created_at, action, status, target_type, target_name, message, meta_json)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		createdAt, action, status, targetType, targetName, message, metaJSON,
	); err != nil {
		t.Fatalf("insert audit event: %v", err)
	}
}

func testService(db *sql.DB, path string) *Service {
	nowLoc := time.UTC
	return NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return nowLoc, "UTC" },
		CurrentLocation: func() *time.Location { return nowLoc },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return "display:" + raw, "UTC"
		},
		UpdateCompleteAction: "update.complete",
		JobTimestampLayout:   policies.DefaultTimestampLayout,
	})
}

func TestServiceBuildSummaryAggregatesAndSorts(t *testing.T) {
	db, path := newTestDB(t, "summary.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "ok", map[string]any{"duration_ms": 1000})
	insertAudit(t, db, now.Add(-2*time.Hour).Format(time.RFC3339), "update.complete", "failure", "server", "srv-b", "pre", map[string]any{"precheck_failed": "apt_health"})
	insertAudit(t, db, now.Add(-3*time.Hour).Format(time.RFC3339), "update.complete", "failure", "server", "srv-c", "retry", map[string]any{"retry_exhausted": true, "execution_duration_ms": "500"})
	insertAudit(t, db, now.Add(-40*24*time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "old", "old", map[string]any{"duration_ms": 999})

	summary, err := testService(db, path).BuildSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildSummary() error = %v", err)
	}
	if summary.Window != "7d" || !strings.HasPrefix(summary.FromDisplay, "display:") || !strings.HasPrefix(summary.ToDisplay, "display:") {
		t.Fatalf("summary window/display = %q/%q/%q", summary.Window, summary.FromDisplay, summary.ToDisplay)
	}
	if summary.Totals.UpdatesTotal != 3 || summary.Totals.UpdatesSuccess != 1 || summary.Totals.UpdatesFailure != 2 {
		t.Fatalf("totals = %+v, want 3 total, 1 success, 2 failure", summary.Totals)
	}
	if summary.Duration.AvgMS != 750 || summary.Duration.SamplesWithDuration != 2 || summary.Duration.SamplesWithoutDuration != 1 {
		t.Fatalf("duration = %+v, want avg 750 with 2 samples and 1 missing", summary.Duration)
	}
	if len(summary.FailureCauses) != 2 || summary.FailureCauses[0].Cause != "precheck:apt_health" || summary.FailureCauses[1].Cause != "retry_exhausted" {
		t.Fatalf("failure causes = %+v, want deterministic causes", summary.FailureCauses)
	}
}

func TestServiceBuildDashboardSummaryUsesInjectedState(t *testing.T) {
	db, path := newTestDB(t, "dashboard.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	rebootRequired := true
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "done", map[string]any{
		"execution_duration_ms": 300,
		"postcheck_results": []updates.PrecheckResult{
			{Name: updates.PostcheckNameRebootNeeded, Details: "reboot required"},
		},
	})
	insertAudit(t, db, now.Add(-30*time.Minute).Format(time.RFC3339), "server.facts.refresh", "success", "server", "srv-a", "facts", nil)

	service := NewService(ServiceDeps{
		DB:              func() *sql.DB { return db },
		DBPath:          func() string { return path },
		CurrentTimezone: func() (*time.Location, string) { return time.UTC, "UTC" },
		CurrentLocation: func() *time.Location { return time.UTC },
		FormatTimestamp: func(raw string, _ *time.Location, _ string) (string, string) {
			return raw, "UTC"
		},
		ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
			return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}}, map[string]*servers.ServerStatus{
				"srv-a": {Name: "srv-a", PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true, CVEs: []string{"CVE-2026-1"}}}},
			}
		},
		LoadServerFacts: func() (map[string]updates.ServerFactsRecord, error) {
			return map[string]updates.ServerFactsRecord{
				"srv-a": {
					ServerName:     "srv-a",
					CollectedAt:    now.Add(-2 * time.Hour).Format(time.RFC3339),
					OSPrettyName:   "Ubuntu",
					DiskStatus:     "ok",
					AptStatus:      "ok",
					RebootRequired: &rebootRequired,
				},
			}, nil
		},
		ListPolicies:        func() ([]policies.Policy, error) { return nil, nil },
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return nil, nil },
		LoadGlobalBlackouts: func() ([]policies.BlackoutWindow, error) { return nil, nil },
		ListPolicyRuns:      func(int) ([]policies.Run, error) { return nil, nil },
		ParseAppTimestamp: func(raw string) (time.Time, error) {
			return time.Parse(time.RFC3339, raw)
		},
		HealthStatusFromResult: func(result updates.PrecheckResult) string {
			if result.Passed {
				return "ok"
			}
			return "critical"
		},
		RebootResultRequiresRestart: func(updates.PrecheckResult) (bool, bool) { return true, true },
		UpdateCompleteAction:        "update.complete",
	})

	summary, err := service.BuildDashboardSummary("7d", now)
	if err != nil {
		t.Fatalf("BuildDashboardSummary() error = %v", err)
	}
	if len(summary.Servers) != 1 {
		t.Fatalf("server count = %d, want 1", len(summary.Servers))
	}
	got := summary.Servers[0]
	if got.Risk.Level != "critical" || len(got.Risk.CVEs) != 1 {
		t.Fatalf("risk = %+v, want critical CVE risk", got.Risk)
	}
	if got.LastUpdate == nil || got.LastUpdate.DurationMS != 300 {
		t.Fatalf("last update = %+v, want duration 300", got.LastUpdate)
	}
	if got.Health.RebootRequired == nil || !*got.Health.RebootRequired {
		t.Fatalf("reboot required = %v, want true", got.Health.RebootRequired)
	}
	if got.NextRun.State != "none" || got.NoRun.Active {
		t.Fatalf("schedule/no-run = %+v/%+v, want no scheduled run and inactive blackout", got.NextRun, got.NoRun)
	}
}

func TestServiceBuildMetricsCachesPerDBPathAndWindow(t *testing.T) {
	db, path := newTestDB(t, "metrics-cache.db")
	now := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	service := testService(db, path)

	first, err := service.BuildMetrics(now)
	if err != nil {
		t.Fatalf("BuildMetrics(first) error = %v", err)
	}
	insertAudit(t, db, now.Add(-time.Hour).Format(time.RFC3339), "update.complete", "success", "server", "srv-a", "ok", nil)
	second, err := service.BuildMetrics(now.Add(time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(second) error = %v", err)
	}
	if first != second {
		t.Fatalf("cached metrics changed within TTL")
	}

	third, err := service.BuildMetrics(now.Add(DefaultMetricsCacheTTL + time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(third) error = %v", err)
	}
	if !strings.Contains(third, `simplelinuxupdater_update_runs{window="24h",status="success"} 1`) {
		t.Fatalf("metrics after TTL missing new success count:\n%s", third)
	}

	otherDB, otherPath := newTestDB(t, "metrics-cache-other.db")
	service.deps.DB = func() *sql.DB { return otherDB }
	service.deps.DBPath = func() string { return otherPath }
	other, err := service.BuildMetrics(now.Add(2 * time.Second))
	if err != nil {
		t.Fatalf("BuildMetrics(other DB) error = %v", err)
	}
	if strings.Contains(other, `status="success"} 1`) {
		t.Fatalf("other DB metrics reused cached success count:\n%s", other)
	}
}

func TestMetricsTokenServiceLifecycleAndFallback(t *testing.T) {
	db, path := newTestDB(t, "token.db")
	random := byte(1)
	service := NewMetricsTokenService(MetricsTokenDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return path },
		RandomRead: func(buf []byte) (int, error) {
			for i := range buf {
				buf[i] = random
			}
			random++
			return len(buf), nil
		},
		HashPassword: func(token string) (string, error) {
			raw, err := bcrypt.GenerateFromPassword([]byte(token), bcrypt.MinCost)
			return string(raw), err
		},
		ComparePasswordAndHash: func(token, hash string) (bool, error) {
			return bcrypt.CompareHashAndPassword([]byte(hash), []byte(token)) == nil, nil
		},
	})

	if service.Status() {
		t.Fatalf("Status() = true before token creation")
	}
	token, err := service.Rotate()
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if token == "" || !service.Status() {
		t.Fatalf("Rotate() token/status = %q/%t, want token and enabled", token, service.Status())
	}
	ok, err := service.VerifyBearerToken(token)
	if err != nil || !ok {
		t.Fatalf("VerifyBearerToken(valid) = %t/%v, want true/nil", ok, err)
	}
	ok, err = service.VerifyBearerToken("wrong")
	if err != nil || ok {
		t.Fatalf("VerifyBearerToken(wrong) = %t/%v, want false/nil", ok, err)
	}
	if err := service.Clear(); err != nil {
		t.Fatalf("Clear() error = %v", err)
	}
	if service.Status() {
		t.Fatalf("Status() = true after clear")
	}

	cachedHash := "$2a$04$jW5I0PMb7s8eyxZswzruCOx2Unio5jXScWp55MSfS.KMtMucHEVKq"
	closedDB, closedPath := newTestDB(t, "token-closed.db")
	service.deps.DB = func() *sql.DB { return closedDB }
	service.deps.DBPath = func() string { return closedPath }
	service.RestoreCache(cachedHash, false, closedPath)
	_ = closedDB.Close()
	if got := service.Hash(); got != cachedHash {
		t.Fatalf("Hash() locked fallback = %q, want cached hash", got)
	}
}

func TestMetricsTokenServiceUnavailableRandom(t *testing.T) {
	db, path := newTestDB(t, "token-random.db")
	service := NewMetricsTokenService(MetricsTokenDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return path },
		RandomRead: func([]byte) (int, error) {
			return 0, errors.New("entropy unavailable")
		},
		HashPassword: func(string) (string, error) { return "", nil },
	})
	if _, err := service.Rotate(); err == nil {
		t.Fatalf("Rotate() error = nil, want entropy error")
	}
}
