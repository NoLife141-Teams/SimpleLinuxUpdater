package observability

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"debian-updater/internal/audit"
	"debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/testutil/dashboardfixture"
)

func scalingCollector(t testing.TB, n int) (dashboardProjectionCollector, *dashboardfixture.SQLMeter) {
	t.Helper()
	db, meter, err := dashboardfixture.Open(filepath.Join(t.TempDir(), "collector.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	for _, install := range []func(*sql.DB) error{servers.EnsureSchema, audit.EnsureSchema, health.EnsureServerFactsSchema, jobs.EnsureSchema, policies.EnsureSchema} {
		if err := install(db); err != nil {
			t.Fatal(err)
		}
	}
	d := dashboardfixture.Generate(n)
	if err := d.SeedDB(db); err != nil {
		t.Fatal(err)
	}
	repo := policies.NewSQLiteRepository(policies.SQLiteRepositoryDeps{DB: func() *sql.DB { return db }})
	policy := policies.NewService(policies.ServiceDeps{ListPolicies: repo.ListPolicies, LoadOverrides: repo.LoadAllOverrides, LoadGlobalBlackouts: repo.LoadGlobalBlackouts, ListRuns: repo.ListRuns})
	return newDashboardProjectionCollector(ServiceDeps{DB: func() *sql.DB { return db }, ServerSnapshot: func() ([]servers.Server, map[string]*servers.ServerStatus) {
		return servers.CloneServers(d.Servers), servers.CloneStatusMap(d.Statuses)
	}, HostHealthObservation: health.SQLiteObservation{DB: func() *sql.DB { return db }}, ProjectPolicySchedule: policy.ProjectSchedule}), meter
}

func BenchmarkDashboardScalingPhases(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			collector, _ := scalingCollector(b, n)
			options := DashboardSummaryOptions{OmitCommandHistory: true}
			input, err := collector.CollectContextWithOptions(context.Background(), "24h", dashboardfixture.Now, options)
			if err != nil {
				b.Fatal(err)
			}
			b.Run("Collect", func(b *testing.B) {
				b.ReportAllocs()
				for range b.N {
					if _, err := collector.CollectContextWithOptions(context.Background(), "24h", dashboardfixture.Now, options); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("Project", func(b *testing.B) {
				b.ReportAllocs()
				p := newDashboardProjection()
				for range b.N {
					p.Project(input)
				}
			})
		})
	}
}

func TestDashboardScalingPhaseMeasurements(t *testing.T) {
	out := os.Getenv("SLU_SCALING_OUTPUT")
	if out == "" {
		t.Skip("optional phase measurements")
	}
	for _, n := range []int{10, 100, 1000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			c, meter := scalingCollector(t, n)
			options := DashboardSummaryOptions{OmitCommandHistory: true}
			input, err := c.CollectContextWithOptions(context.Background(), "24h", dashboardfixture.Now, options)
			if err != nil {
				t.Fatal(err)
			}
			p := newDashboardProjection()
			results := []map[string]any{}
			for _, phase := range []struct {
				name string
				run  func()
			}{{"collect", func() {
				var err error
				input, err = c.CollectContextWithOptions(context.Background(), "24h", dashboardfixture.Now, options)
				if err != nil {
					t.Fatal(err)
				}
			}}, {"project", func() { p.Project(input) }}} {
				phase.run()
				meter.Reset()
				times := []float64{}
				for range 15 {
					at := time.Now()
					phase.run()
					times = append(times, float64(time.Since(at).Nanoseconds())/1e6)
				}
				sort.Float64s(times)
				results = append(results, map[string]any{"name": phase.name, "n": 15, "p50_ms": times[7], "p95_ms": times[14], "sql_statements": meter.Count() / 15})
			}
			raw, err := json.MarshalIndent(map[string]any{"manifest": dashboardfixture.Generate(n).Manifest, "measurements": results, "scope": "collector uses real SQLite health/policy/jobs/audit; default UTC formatting and ready action projection; HTTP composition measured separately"}, "", "  ")
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(out, fmt.Sprintf("phases-%d.json", n)), append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
		})
	}
}
