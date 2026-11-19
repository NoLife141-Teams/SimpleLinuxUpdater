package policies

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

func TestSchedulerDenialSurvivesAProcessRestartAndPartialHistory(t *testing.T) {
	for _, test := range []struct {
		name        string
		replaceHost bool
		partial     bool
	}{
		{name: "retains exact winner after partially recording history", partial: true},
		{name: "inventory replacement invalidates attribution", replaceHost: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			const blockingJob = "encrypted-export"
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "policies.db")
			now := time.Date(2026, 11, 19, 4, 29, 0, 0, time.UTC)
			slot := now.Add(time.Minute)
			inventory := []servers.Server{{Name: "host", Identity: "original-id", Host: "ssh.test"}}
			var db *sql.DB
			var repo *SQLiteRepository
			var service *Service
			var store SchedulerWatermarkStore
			var coordinator *maintenance.Coordinator
			var failLower bool
			var launched int
			restart := func() {
				t.Helper()
				reopening := db != nil
				if reopening {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				db, err = sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if !reopening {
					if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
						t.Fatal(err)
					}
					if err := EnsureSchema(db); err != nil {
						t.Fatal(err)
					}
				}
				provider := func() *sql.DB { return db }
				repo = NewSQLiteRepository(SQLiteRepositoryDeps{DB: provider, NowString: func() string { return now.Format(DefaultTimestampLayout) }})
				coordinator = maintenance.NewCoordinator(maintenance.Deps{Store: maintenance.SQLiteStore{DB: provider}})
				if err := coordinator.Initialize(ctx); err != nil {
					t.Fatal(err)
				}
				deps := testServiceDeps()
				deps.Maintenance = coordinator
				deps.Now = func() time.Time { return now }
				deps.ListPolicies = repo.ListPolicies
				deps.ListRuns = repo.ListRuns
				deps.SnapshotServers = func() []servers.Server { return inventory }
				deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
					if req.Outcome == "" {
						launched++
					}
					if failLower && req.Policy.ExecutionMode == ExecutionScanOnly {
						return ScheduledRunResult{Err: errors.New("interrupted history write")}
					}
					_, inserted, err := repo.CreateRun(Run{
						PolicyID: req.Policy.ID, ServerName: req.Server.Name, ServerIdentity: req.Server.Identity,
						ScheduledForUTC: req.ScheduledForUTC, Status: RunSkipped, Reason: req.Outcome,
						MaintenanceCause: req.MaintenanceBlocker.Cause, BlockingJobID: req.MaintenanceBlocker.JobID,
					})
					return ScheduledRunResult{Inserted: inserted, Err: err}
				}
				service = NewService(deps)
				store = SchedulerWatermarkStore{
					Load: repo.LoadSchedulerWatermark, Save: repo.SaveSchedulerWatermark,
					LoadStateFingerprint: repo.LoadSchedulerStateFingerprint, SaveStateFingerprint: repo.SaveSchedulerStateFingerprint,
					LoadStateRevision: repo.LoadSchedulerStateRevision, SaveDenial: repo.SaveSchedulerDenial,
					LoadDenials: repo.LoadSchedulerDenials, DeleteDenialsBefore: repo.DeleteSchedulerDenialsBefore,
				}
			}
			restart()
			defer func() { _ = db.Close() }()
			for _, mode := range []string{ExecutionApprovalRequired, ExecutionScanOnly} {
				if _, err := repo.CreatePolicy(Policy{
					Name: mode, Enabled: true, TargetServers: []string{"host"},
					ExecutionMode: mode, PackageScope: PackageScopeSecurity, CadenceKind: CadenceDaily, TimeLocal: "04:30",
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := service.ProcessDueWithRecovery(now, store); err != nil {
				t.Fatal(err)
			}
			lease, decision := coordinator.TryExclusive(maintenance.OperationBackupExport)
			if !decision.Allowed {
				t.Fatal("exclusive export refused")
			}
			if err := lease.Activate(ctx, maintenance.OperationFacts{JobID: blockingJob}); err != nil {
				t.Fatal(err)
			}
			now = slot.Add(20 * time.Second)
			if err := service.ProcessDueWithRecovery(slot, store); err != nil {
				t.Fatal(err)
			}
			// Simulate death with the export still active and no Scheduled Run.
			if runs, err := repo.ListRuns(10); err != nil || len(runs) != 0 {
				t.Fatalf("runs before replacement = %+v, %v", runs, err)
			}
			restart()
			if test.replaceHost {
				inventory[0].Identity = "replacement-id"
				now = slot.Add(time.Minute)
			}
			if test.partial {
				failLower = true
				if err := service.ProcessDueWithRecovery(now, store); err == nil {
					t.Fatal("expected interrupted history write")
				}
				failLower = false
				restart()
			}
			for range 2 {
				if err := service.ProcessDueWithRecovery(now, store); err != nil {
					t.Fatal(err)
				}
				restart()
			}
			now = slot.Add(2 * time.Minute)
			if err := service.ProcessDueWithRecovery(now, store); err != nil {
				t.Fatal(err)
			}
			runs, err := repo.ListRuns(10)
			if err != nil || len(runs) != 2 || launched != 0 {
				t.Fatalf("runs = %+v, launched = %d, error = %v", runs, launched, err)
			}
			for _, run := range runs {
				if !test.replaceHost && run.PolicyID == 1 {
					if run.Reason != RunReasonMaintenance || run.BlockingJobID != blockingJob || run.MaintenanceCause != maintenance.BlockBackupExport {
						t.Fatalf("lost exact denial: %+v", run)
					}
				} else if run.BlockingJobID != "" || run.MaintenanceCause != "" {
					t.Fatalf("unexpected attribution: %+v", run)
				}
			}
			if denials, err := repo.LoadSchedulerDenials(slot, now); err != nil || len(denials) != 0 {
				t.Fatalf("consumed denials = %+v, %v", denials, err)
			}
		})
	}
}

func TestSchedulerDenialRetainsFirstObservationAndPrunesExpiredRows(t *testing.T) {
	repo, _ := newTestRepository(t)
	now := time.Date(2026, 11, 19, 4, 29, 0, 0, time.UTC)
	old := now.Add(-DefaultSchedulerRecoveryHorizon - time.Minute)
	for _, denial := range []SchedulerDenial{
		{Tick: old, ObservedAt: old, Fingerprint: "expired"},
		{Tick: now, ObservedAt: now.Add(10 * time.Second), Fingerprint: "first",
			Blocker: maintenance.Blocker{Cause: maintenance.BlockBackupExport, JobID: "first-job"}},
		{Tick: now, ObservedAt: now.Add(20 * time.Second), Fingerprint: "later",
			Blocker: maintenance.Blocker{Cause: maintenance.BlockBackupRestore, JobID: "later-job"}},
	} {
		if err := repo.SaveSchedulerDenial(denial); err != nil {
			t.Fatal(err)
		}
	}
	denials, err := repo.LoadSchedulerDenials(old, now)
	if err != nil || len(denials) != 1 || denials[0].Blocker.JobID != "first-job" ||
		denials[0].Fingerprint != "first" || !denials[0].ObservedAt.Equal(now.Add(10*time.Second)) {
		t.Fatalf("retained denials = %+v, %v", denials, err)
	}
}
