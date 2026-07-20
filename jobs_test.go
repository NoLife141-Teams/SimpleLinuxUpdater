package main

import (
	"database/sql"
	"path/filepath"
	"sync"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

func TestJobRuntimeStatusSyncFromRecord(t *testing.T) {
	newIsolatedTestApp(t)

	server := Server{Name: "srv-runtime-sync", Host: "example.org", Port: 22, User: "root"}
	mu.Lock()
	servers = []Server{server}
	statusMap = map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	mu.Unlock()
	if err := initializeJobManager(); err != nil {
		t.Fatalf("initializeJobManager() error = %v", err)
	}

	job, err := currentJobManager().CreateJob(JobCreateParams{
		Kind:       jobKindUpdate,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobStatusQueued,
	})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	status := jobStatusRunning
	phase := jobPhaseAptUpgrade
	if err := currentJobManager().Transition(job.ID, JobTransitionIntent{
		Status: &status,
		Phase:  &phase,
	}); err != nil {
		t.Fatalf("Transition() error = %v", err)
	}

	snapshot := currentStatusSnapshot(server.Name)
	if snapshot == nil || snapshot.Status != "upgrading" {
		t.Fatalf("runtime status = %+v, want upgrading", snapshot)
	}
}

func TestStartJobRunnerDoesNotExecuteWhenRunningStateCannotBePersisted(t *testing.T) {
	app := newIsolatedTestApp(t)
	manager := newJobManager(app.Deps.DB())
	job, err := manager.CreateJob(JobCreateParams{
		Kind:       jobKindUpdate,
		ServerName: "srv-runner-admission",
		Actor:      "tester",
		Status:     jobStatusQueued,
	})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	if err := app.Deps.DB().Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}

	executed := make(chan struct{}, 1)
	restored := make(chan struct{}, 1)
	startJobRunnerWithManager(func() *JobManager { return manager }, job.ID, func() {
		executed <- struct{}{}
	}, func() {
		restored <- struct{}{}
	})
	waitForUpdateRunners()

	select {
	case <-executed:
		t.Fatal("runner executed without a durable running state")
	default:
	}
	select {
	case <-restored:
	default:
		t.Fatal("runner admission failure did not invoke runtime rollback")
	}
}

func TestStartJobRunnerTerminalizesJobAndRestoresRuntimeOnAdmissionFailure(t *testing.T) {
	var stateMu sync.Mutex
	server := Server{Name: "srv-runner-rollback", Host: "example.org", Port: 22, User: "root"}
	serverList := []Server{server}
	original := &ServerStatus{Name: server.Name, Status: "idle", Logs: "ready"}
	statuses := map[string]*ServerStatus{server.Name: cloneServerStatus(original)}
	state := serverpkg.NewState(&stateMu, &serverList, &statuses, statusInProgress)

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "jobs.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureJobSchema(db); err != nil {
		t.Fatalf("ensure job schema: %v", err)
	}
	manager := newJobManagerWithRuntime(db, nil, state, nil)
	job, err := manager.CreateJob(JobCreateParams{
		Kind:       jobKindUpdate,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobStatusQueued,
	})
	if err != nil {
		t.Fatalf("CreateJob() error = %v", err)
	}
	preStartStatus := state.CurrentStatusSnapshot(server.Name)
	if _, err := state.BeginAction(server.Name, "updating"); err != nil {
		t.Fatalf("BeginAction() error = %v", err)
	}
	if _, err := db.Exec(`
		CREATE TRIGGER reject_running_job
		BEFORE UPDATE OF status ON jobs
		WHEN NEW.status = 'running'
		BEGIN
			SELECT RAISE(FAIL, 'runner admission denied');
		END;
	`); err != nil {
		t.Fatalf("create admission failure trigger: %v", err)
	}

	executed := make(chan struct{}, 1)
	startJobRunnerWithManager(func() *JobManager { return manager }, job.ID, func() {
		executed <- struct{}{}
	}, func() {
		state.RestoreStatusSnapshot(server.Name, preStartStatus)
	})
	waitForUpdateRunners()

	select {
	case <-executed:
		t.Fatal("runner executed without a durable running state")
	default:
	}
	persisted, err := manager.GetJob(job.ID)
	if err != nil {
		t.Fatalf("GetJob() error = %v", err)
	}
	if persisted.Status != jobStatusFailed || persisted.Phase != jobPhaseComplete || persisted.Summary != "Runner admission failed" || persisted.ErrorClass != "persistence" || persisted.FinishedAt == "" {
		t.Fatalf("terminal job = %+v, want failed persistence admission job", persisted)
	}
	restoredStatus := state.CurrentStatusSnapshot(server.Name)
	if restoredStatus == nil || restoredStatus.Status != "idle" || restoredStatus.Logs != "ready" {
		t.Fatalf("restored runtime = %+v, want original idle snapshot", restoredStatus)
	}
	if _, err := state.BeginAction(server.Name, "updating"); err != nil {
		t.Fatalf("server remains blocked after admission failure: %v", err)
	}
}
