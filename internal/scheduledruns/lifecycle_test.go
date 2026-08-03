package scheduledruns_test

import (
	"context"
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

type flakyReconciliationRepository struct {
	delegate         *policies.SQLiteRepository
	mu               sync.Mutex
	getFailures      int
	updateFailures   int
	terminalFailures int
}

type terminalBeforeActiveTransitionRepository struct {
	delegate *policies.SQLiteRepository
	once     sync.Once
	err      error
}

func (r *flakyReconciliationRepository) CreateRun(run policies.Run) (policies.Run, bool, error) {
	return r.delegate.CreateRun(run)
}

func (r *flakyReconciliationRepository) GetRun(id int64) (policies.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.getFailures > 0 {
		r.getFailures--
		return policies.Run{}, errors.New("database is locked")
	}
	return r.delegate.GetRun(id)
}

func (r *flakyReconciliationRepository) UpdateRun(id int64, update policies.RunUpdate) error {
	r.mu.Lock()
	if r.updateFailures > 0 {
		r.updateFailures--
		r.mu.Unlock()
		return errors.New("database is busy")
	}
	r.mu.Unlock()
	return r.delegate.UpdateRun(id, update)
}

func (r *flakyReconciliationRepository) TransitionRunTerminal(id int64, update policies.RunUpdate) (bool, error) {
	r.mu.Lock()
	if r.terminalFailures > 0 {
		r.terminalFailures--
		r.mu.Unlock()
		return false, errors.New("database is locked")
	}
	r.mu.Unlock()
	return r.delegate.TransitionRunTerminal(id, update)
}

func (r *flakyReconciliationRepository) TransitionRunActive(id int64, update policies.RunUpdate) (bool, error) {
	return r.delegate.TransitionRunActive(id, update)
}

func (r *terminalBeforeActiveTransitionRepository) CreateRun(run policies.Run) (policies.Run, bool, error) {
	return r.delegate.CreateRun(run)
}

func (r *terminalBeforeActiveTransitionRepository) GetRun(id int64) (policies.Run, error) {
	return r.delegate.GetRun(id)
}

func (r *terminalBeforeActiveTransitionRepository) UpdateRun(id int64, update policies.RunUpdate) error {
	return r.delegate.UpdateRun(id, update)
}

func (r *terminalBeforeActiveTransitionRepository) TransitionRunTerminal(id int64, update policies.RunUpdate) (bool, error) {
	return r.delegate.TransitionRunTerminal(id, update)
}

func (r *terminalBeforeActiveTransitionRepository) TransitionRunActive(id int64, update policies.RunUpdate) (bool, error) {
	r.once.Do(func() {
		status := policies.RunSucceeded
		finishedAt := "2026-08-02T12:01:00Z"
		r.err = r.delegate.UpdateRun(id, policies.RunUpdate{Status: &status, FinishedAt: &finishedAt})
	})
	if r.err != nil {
		return false, r.err
	}
	return r.delegate.TransitionRunActive(id, update)
}

type getOnlyJobRepository struct {
	jobs.Repository
	get func(string) (jobs.Record, error)
}

func (r getOnlyJobRepository) Get(id string) (jobs.Record, error) {
	return r.get(id)
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

func TestWatchJobRetriesUnavailableManagerAndTransientRunWrites(t *testing.T) {
	db, repository, run, audits := newReconciliationHarness(t)
	jobRepository := jobs.NewSQLiteRepository(db)
	manager := jobs.NewManager(jobRepository, jobs.ManagerOptions{
		Now:   func() time.Time { return time.Date(2026, 8, 2, 12, 1, 0, 0, time.UTC) },
		NewID: func() string { return "retry-job" },
	})
	job, err := manager.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindScheduledScan,
		ServerName: run.ServerName,
		Actor:      "system",
		Status:     jobs.StatusSucceeded,
		Summary:    "Scheduled scan completed",
		MetaJSON:   jobs.MarshalJSON(updates.ScheduledJobMeta{Trigger: "scheduled", PolicyID: run.PolicyID, PolicyName: run.PolicyName}),
	})
	if err != nil {
		t.Fatal(err)
	}
	flakyRuns := &flakyReconciliationRepository{delegate: repository, terminalFailures: 2}
	managerReads := 0
	waits := 0
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService: audits,
		CurrentJobManager: func() *jobs.Manager {
			managerReads++
			if managerReads <= 2 {
				return nil
			}
			return manager
		},
		JobTimestampNow:        func() string { return time.Date(2026, 8, 2, 12, 2, 0, 0, time.UTC).Format(time.RFC3339Nano) },
		PolicyRepository:       flakyRuns,
		ReconciliationAttempts: 5,
		ReconciliationWait: func(context.Context, time.Duration) error {
			waits++
			return nil
		},
		ReconciliationBackoff: func(int) time.Duration { return 0 },
	})

	lifecycle.WatchJob(run.ID, job.ID)
	persisted, err := repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Status != policies.RunSucceeded || persisted.JobID != job.ID {
		t.Fatalf("persisted run = %+v, want reconciled success", persisted)
	}
	if managerReads != 3 || waits < 4 {
		t.Fatalf("managerReads=%d waits=%d, want retries for manager and writes", managerReads, waits)
	}
}

func TestWatchJobConfirmsMissingJobBeforeInterruptingRun(t *testing.T) {
	_, repository, run, audits := newReconciliationHarness(t)
	reads := 0
	manager := jobs.NewManager(getOnlyJobRepository{get: func(string) (jobs.Record, error) {
		reads++
		return jobs.Record{}, sql.ErrNoRows
	}}, jobs.ManagerOptions{})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:            audits,
		CurrentJobManager:       func() *jobs.Manager { return manager },
		JobTimestampNow:         func() string { return time.Date(2026, 8, 2, 12, 2, 0, 0, time.UTC).Format(time.RFC3339Nano) },
		PolicyRepository:        repository,
		MissingJobConfirmations: 3,
		ReconciliationWait:      func(context.Context, time.Duration) error { return nil },
		ReconciliationBackoff:   func(int) time.Duration { return 0 },
	})

	lifecycle.WatchJob(run.ID, "missing-job")
	persisted, err := repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 3 || persisted.Status != policies.RunInterrupted || persisted.Reason != policies.RunReasonMissing {
		t.Fatalf("reads=%d persisted=%+v, want confirmed missing interruption", reads, persisted)
	}
}

func TestWatchJobRetriesTransientJobReads(t *testing.T) {
	_, repository, run, audits := newReconciliationHarness(t)
	reads := 0
	manager := jobs.NewManager(getOnlyJobRepository{get: func(id string) (jobs.Record, error) {
		reads++
		if reads <= 2 {
			return jobs.Record{}, errors.New("database is busy")
		}
		return jobs.Record{ID: id, Kind: jobs.KindScheduledScan, ServerName: run.ServerName, Status: jobs.StatusSucceeded, FinishedAt: "2026-08-02T12:01:00Z"}, nil
	}}, jobs.ManagerOptions{})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:           audits,
		CurrentJobManager:      func() *jobs.Manager { return manager },
		PolicyRepository:       repository,
		ReconciliationWait:     func(context.Context, time.Duration) error { return nil },
		ReconciliationBackoff:  func(int) time.Duration { return 0 },
		ReconciliationAttempts: 4,
	})

	lifecycle.WatchJob(run.ID, "transient-job")
	persisted, err := repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 3 || persisted.Status != policies.RunSucceeded {
		t.Fatalf("reads=%d persisted=%+v, want recovery after transient reads", reads, persisted)
	}
}

func TestWatchJobPersistsPermanentReadFailureWithoutRetrying(t *testing.T) {
	_, repository, run, audits := newReconciliationHarness(t)
	reads := 0
	manager := jobs.NewManager(getOnlyJobRepository{get: func(string) (jobs.Record, error) {
		reads++
		return jobs.Record{}, errors.New("job record is corrupt")
	}}, jobs.ManagerOptions{})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:      audits,
		CurrentJobManager: func() *jobs.Manager { return manager },
		PolicyRepository:  repository,
		ReconciliationWait: func(context.Context, time.Duration) error {
			t.Fatal("permanent read error should not be retried")
			return nil
		},
	})

	lifecycle.WatchJob(run.ID, "corrupt-job")
	persisted, err := repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reads != 1 || persisted.Status != policies.RunInterrupted || persisted.Reason != policies.RunReasonPersistence {
		t.Fatalf("reads=%d persisted=%+v, want permanent failure interruption", reads, persisted)
	}
}

func TestWatchJobCancellationStopsRetryWait(t *testing.T) {
	_, repository, run, audits := newReconciliationHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	done := make(chan struct{})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:      audits,
		CurrentJobManager: func() *jobs.Manager { return nil },
		PolicyRepository:  repository,
		ReconciliationWait: func(ctx context.Context, _ time.Duration) error {
			select {
			case <-waiting:
			default:
				close(waiting)
			}
			<-ctx.Done()
			return ctx.Err()
		},
	})
	go func() {
		lifecycle.WatchJobContext(ctx, run.ID, "pending-job")
		close(done)
	}()
	<-waiting
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watcher leaked after cancellation")
	}
}

func TestConcurrentTerminalReconciliationRecordsAuditOnce(t *testing.T) {
	db, repository, run, audits := newReconciliationHarness(t)
	manager := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		Now:   func() time.Time { return time.Date(2026, 8, 2, 12, 1, 0, 0, time.UTC) },
		NewID: func() string { return "terminal-job" },
	})
	job, err := manager.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindScheduledScan,
		ServerName: run.ServerName,
		Actor:      "system",
		Status:     jobs.StatusSucceeded,
		Summary:    "Scheduled scan completed",
		MetaJSON:   jobs.MarshalJSON(updates.ScheduledJobMeta{Trigger: "scheduled", PolicyID: run.PolicyID, PolicyName: run.PolicyName}),
	})
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:      audits,
		CurrentJobManager: func() *jobs.Manager { return manager },
		PolicyRepository:  repository,
	})
	var watchers sync.WaitGroup
	watchers.Add(2)
	for range 2 {
		go func() {
			defer watchers.Done()
			lifecycle.WatchJob(run.ID, job.ID)
		}()
	}
	watchers.Wait()
	listed, err := audits.List(audit.ListFilter{Action: "schedule.run.completed", TargetName: run.ServerName})
	if err != nil {
		t.Fatal(err)
	}
	if listed.Total != 1 {
		t.Fatalf("terminal audit count = %d, want 1", listed.Total)
	}
}

func TestReconcileRunDoesNotReopenTerminalRunFromStaleActiveJob(t *testing.T) {
	_, repository, run, audits := newReconciliationHarness(t)
	jobID := "stale-active-job"
	if err := repository.UpdateRun(run.ID, policies.RunUpdate{JobID: &jobID}); err != nil {
		t.Fatal(err)
	}
	run, err := repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	manager := jobs.NewManager(getOnlyJobRepository{get: func(id string) (jobs.Record, error) {
		return jobs.Record{ID: id, Kind: jobs.KindScheduledScan, ServerName: run.ServerName, Status: jobs.StatusRunning}, nil
	}}, jobs.ManagerOptions{})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:      audits,
		CurrentJobManager: func() *jobs.Manager { return manager },
		PolicyRepository:  &terminalBeforeActiveTransitionRepository{delegate: repository},
	})

	reconciled, err := lifecycle.ReconcileRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != policies.RunSucceeded || reconciled.FinishedAt == "" {
		t.Fatalf("reconciled run = %+v, want terminal success preserved", reconciled)
	}
}

func TestReconcileRunCorrectsRestartInterruptedProjectionFromTerminalJob(t *testing.T) {
	db, repository, run, audits := newReconciliationHarness(t)
	manager := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{
		Now:   func() time.Time { return time.Date(2026, 8, 2, 12, 1, 0, 0, time.UTC) },
		NewID: func() string { return "restart-terminal-job" },
	})
	job, err := manager.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindScheduledScan,
		ServerName: run.ServerName,
		Actor:      "system",
		Status:     jobs.StatusSucceeded,
		Summary:    "Scheduled scan completed before restart",
	})
	if err != nil {
		t.Fatal(err)
	}
	status := policies.RunInterrupted
	reason := policies.RunReasonRestart
	if err := repository.UpdateRun(run.ID, policies.RunUpdate{Status: &status, Reason: &reason, JobID: &job.ID}); err != nil {
		t.Fatal(err)
	}
	run, err = repository.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:      audits,
		CurrentJobManager: func() *jobs.Manager { return manager },
		PolicyRepository:  repository,
	})

	reconciled, err := lifecycle.ReconcileRun(context.Background(), run)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != policies.RunSucceeded || reconciled.JobID != job.ID {
		t.Fatalf("reconciled run = %+v, want authoritative job success", reconciled)
	}
}

func newReconciliationHarness(t *testing.T) (*sql.DB, *policies.SQLiteRepository, policies.Run, *audit.Service) {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	for name, ensure := range map[string]func(*sql.DB) error{"audit": audit.EnsureSchema, "jobs": jobs.EnsureSchema, "policies": policies.EnsureSchema} {
		if err := ensure(db); err != nil {
			t.Fatalf("%s schema: %v", name, err)
		}
	}
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	repository := policies.NewSQLiteRepository(policies.SQLiteRepositoryDeps{DB: func() *sql.DB { return db }, NowString: func() string { return now.Format(time.RFC3339Nano) }})
	run, _, err := repository.CreateRun(policies.Run{
		PolicyID: 91, PolicyName: "wave policy", ServerName: "srv-a", ScheduledForUTC: now.Format(time.RFC3339Nano),
		ExecutionMode: policies.ExecutionScanOnly, PackageScope: policies.PackageScopeSecurity, UpgradeMode: policies.UpgradeModeStandard,
		Status: policies.RunRunning, Summary: "Scheduled run running", ResultJSON: "{}",
	})
	if err != nil {
		t.Fatal(err)
	}
	audits := audit.NewService(audit.ServiceOptions{DB: func() *sql.DB { return db }, Now: func() time.Time { return now }})
	return db, repository, run, audits
}
