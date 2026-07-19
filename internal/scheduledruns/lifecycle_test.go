package scheduledruns_test

import (
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/audit"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/scheduledruns"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"

	_ "modernc.org/sqlite"
)

type failingRunUpdateRepository struct {
	delegate *policies.SQLiteRepository
	err      error
}

func (r failingRunUpdateRepository) CreateRun(run policies.Run) (policies.Run, bool, error) {
	return r.delegate.CreateRun(run)
}

func (r failingRunUpdateRepository) GetRun(id int64) (policies.Run, error) {
	return r.delegate.GetRun(id)
}

func (r failingRunUpdateRepository) UpdateRun(int64, policies.RunUpdate) error {
	return r.err
}

func TestLifecycleHandlesSkippedCandidateIdempotently(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := policies.EnsureSchema(db); err != nil {
		t.Fatalf("policies.EnsureSchema() error = %v", err)
	}
	if err := audit.EnsureSchema(db); err != nil {
		t.Fatalf("audit.EnsureSchema() error = %v", err)
	}
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	repository := policies.NewSQLiteRepository(policies.SQLiteRepositoryDeps{
		DB:        func() *sql.DB { return db },
		NowString: func() string { return now.Format(time.RFC3339Nano) },
	})
	audits := audit.NewService(audit.ServiceOptions{
		DB:  func() *sql.DB { return db },
		Now: func() time.Time { return now },
	})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:     audits,
		JobTimestampNow:  func() string { return now.Add(time.Second).Format(time.RFC3339Nano) },
		PolicyRepository: repository,
	})
	request := policies.ScheduledRunRequest{
		Policy: policies.Policy{
			ID:            41,
			Name:          "blackout policy",
			ExecutionMode: policies.ExecutionScanOnly,
			PackageScope:  policies.PackageScopeSecurity,
			UpgradeMode:   policies.UpgradeModeStandard,
		},
		Server:          servers.Server{Name: "srv-blackout"},
		ScheduledForUTC: now.Format(time.RFC3339Nano),
		Outcome:         policies.RunReasonBlackout,
	}

	first := lifecycle.HandleScheduledRun(request)
	second := lifecycle.HandleScheduledRun(request)
	if !first.Handled || !first.Inserted || first.Status != policies.RunSkipped {
		t.Fatalf("first HandleScheduledRun() = %+v", first)
	}
	if !second.Handled || second.Inserted || second.RunID != first.RunID {
		t.Fatalf("second HandleScheduledRun() = %+v, want existing run %d", second, first.RunID)
	}
	run, err := repository.GetRun(first.RunID)
	if err != nil {
		t.Fatalf("GetRun(%d) error = %v", first.RunID, err)
	}
	if run.Reason != policies.RunReasonBlackout || run.Summary != "Scheduled run skipped due to blackout window" || run.FinishedAt == "" {
		t.Fatalf("persisted run = %+v", run)
	}
	listed, err := audits.List(audit.ListFilter{Action: "schedule.run.skipped", TargetName: request.Server.Name})
	if err != nil {
		t.Fatalf("AuditService.List() error = %v", err)
	}
	if listed.Total != 1 || listed.Items[0].Status != "ignored" {
		t.Fatalf("audit events = %+v, want one ignored skip", listed.Items)
	}
}

func TestLifecycleDoesNotStartUpdateWhenRunningStateCannotBePersisted(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for name, ensure := range map[string]func(*sql.DB) error{
		"audit":    audit.EnsureSchema,
		"jobs":     jobs.EnsureSchema,
		"policies": policies.EnsureSchema,
	} {
		if err := ensure(db); err != nil {
			t.Fatalf("%s schema error = %v", name, err)
		}
	}
	now := time.Date(2026, 7, 17, 12, 0, 0, 0, time.UTC)
	policyRepository := policies.NewSQLiteRepository(policies.SQLiteRepositoryDeps{
		DB:        func() *sql.DB { return db },
		NowString: func() string { return now.Format(time.RFC3339Nano) },
	})
	server := servers.Server{Name: "srv-update", Host: "example.org", Port: 22, User: "root"}
	serverList := []servers.Server{server}
	statusMap := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &serverList, &statusMap, nil)
	jobManager := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		Now:   func() time.Time { return now },
		NewID: func() string { return "scheduled-job" },
		SyncRuntime: func(record jobs.Record) {
			if record.Status != jobs.StatusFailed {
				return
			}
			status := state.CurrentStatusSnapshot(record.ServerName)
			if status == nil {
				return
			}
			status.Status = "error"
			state.RestoreStatusSnapshot(record.ServerName, status)
		},
	})
	runnerStarted := false
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:                    audit.NewService(audit.ServiceOptions{DB: func() *sql.DB { return db }, Now: func() time.Time { return now }}),
		CurrentJobManager:               func() *jobs.Manager { return jobManager },
		JobTimestampNow:                 func() string { return now.Format(time.RFC3339Nano) },
		LoadRetryPolicy:                 func() updates.RetryPolicy { return updates.RetryPolicy{} },
		PolicyRepository:                failingRunUpdateRepository{delegate: policyRepository, err: errors.New("database unavailable")},
		ServerState:                     state,
		StartJobRunner:                  func(string, func(), ...func()) { runnerStarted = true },
		UpdateService:                   &updates.Service{},
		StartScheduledRunReconciliation: func(int64, string) {},
	})

	result := lifecycle.HandleScheduledRun(policies.ScheduledRunRequest{
		Policy: policies.Policy{
			ID:            52,
			Name:          "nightly updates",
			ExecutionMode: policies.ExecutionAutoApply,
			PackageScope:  policies.PackageScopeFull,
			UpgradeMode:   policies.UpgradeModeStandard,
		},
		Server:          server,
		ScheduledForUTC: now.Format(time.RFC3339Nano),
		Admitted:        true,
	})
	if !result.Handled || !result.Inserted {
		t.Fatalf("HandleScheduledRun() = %+v, want inserted run", result)
	}
	if result.Err == nil || !strings.Contains(result.Err.Error(), "database unavailable") {
		t.Fatalf("HandleScheduledRun() error = %v, want running-state persistence error", result.Err)
	}
	if result.Status != policies.RunFailed {
		t.Fatalf("HandleScheduledRun() status = %q, want %q", result.Status, policies.RunFailed)
	}
	if runnerStarted {
		t.Fatal("scheduled update runner started without a persisted running state")
	}
	if got := state.CurrentStatusSnapshot(server.Name); got == nil || got.Status != "idle" {
		t.Fatalf("server status = %+v, want restored idle status", got)
	}
	job, err := jobManager.GetJob("scheduled-job")
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if job.Status != jobs.StatusFailed {
		t.Fatalf("job status = %q, want %q", job.Status, jobs.StatusFailed)
	}
}
