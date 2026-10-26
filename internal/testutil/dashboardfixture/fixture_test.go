package dashboardfixture

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"debian-updater/internal/audit"
	"debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
)

func TestFixtureIntegrityAndSQLMeter(t *testing.T) {
	for _, n := range []int{10, 100, 1000} {
		t.Run(strconv.Itoa(n), func(t *testing.T) {
			d := Generate(n)
			again := Generate(n)
			if !reflect.DeepEqual(d, again) || len(d.Manifest.SHA256) != 64 {
				t.Fatal("fixture is not deterministic")
			}
			if Generate(n+1).Manifest.SHA256 == d.Manifest.SHA256 {
				t.Fatal("digest ignores dimensions")
			}
			if d.Manifest.Active != n/10 || d.Manifest.Approvals != n/5 || d.Manifest.PackageHosts != n*6/10 || d.Manifest.TrustedHosts != n*8/10 {
				t.Fatal("fixture proportions drifted")
			}
			db, meter, err := Open(filepath.Join(t.TempDir(), "fixture.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, install := range []func(*sql.DB) error{servers.EnsureSchema, audit.EnsureSchema, health.EnsureServerFactsSchema, jobs.EnsureSchema, policies.EnsureSchema} {
				if err := install(db); err != nil {
					t.Fatal(err)
				}
			}
			if err := d.SeedDB(db); err != nil {
				t.Fatal(err)
			}
			for status, want := range map[string]int{jobs.StatusRunning: n / 10, jobs.StatusWaitingApproval: n / 5, jobs.StatusSucceeded: n * 7 / 10} {
				var got int
				if err := db.QueryRow("SELECT count(*) FROM jobs WHERE status=?", status).Scan(&got); err != nil || got != want {
					t.Fatalf("canonical job status %s count %d want %d: %v", status, got, want, err)
				}
			}
			for table, want := range map[string]int{"servers": n, "jobs": n, "job_log_chunks": n, "audit_events": n * 14, "update_policies": 3} {
				var got int
				if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&got); err != nil || got != want {
					t.Fatalf("%s count %d want %d: %v", table, got, want, err)
				}
			}
			meter.Reset()
			var count int
			if err := db.QueryRowContext(context.Background(), "SELECT count(*) FROM servers").Scan(&count); err != nil {
				t.Fatal(err)
			}
			stmt, err := db.Prepare("SELECT count(*) FROM jobs WHERE kind=?")
			if err != nil {
				t.Fatal(err)
			}
			if err := stmt.QueryRow("update").Scan(&count); err != nil {
				t.Fatal(err)
			}
			_ = stmt.Close()
			exec, err := db.Prepare("UPDATE jobs SET summary=? WHERE id=?")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := exec.Exec("synthetic", "missing"); err != nil {
				t.Fatal(err)
			}
			_ = exec.Close()
			if meter.Count() != 3 {
				t.Fatalf("meter did not count prepared and direct executions exactly once: %v", meter.Snapshot())
			}
			meter.Reset()
			if meter.Count() != 0 {
				t.Fatal("meter reset failed")
			}
			if err := d.SeedDB(db); err == nil {
				t.Fatal("fixture unexpectedly overwrote existing dataset")
			}
		})
	}
}
