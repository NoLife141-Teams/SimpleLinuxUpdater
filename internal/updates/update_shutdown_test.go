package updates

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

type lifecycleUpdateTestSession struct {
	*HostMaintenanceSessionFuncs
	ctx context.Context
}

func (s *lifecycleUpdateTestSession) MaintenanceContext() context.Context {
	if s == nil || s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

type updateShutdownAudit struct {
	status  string
	message string
	meta    map[string]any
}

func newUpdateShutdownHarness(t *testing.T, lifecycle context.Context, session *HostMaintenanceSessionFuncs, configure func(*ServiceDeps)) (*Service, *jobs.Manager, *servers.State, <-chan updateShutdownAudit) {
	t.Helper()
	server := servers.Server{Name: "srv-shutdown", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)

	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "update-shutdown.db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	jobID := "update-shutdown-job"
	jm := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{NewID: func() string { return jobID }})
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindUpdate,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create update job: %v", err)
	}

	audits := make(chan updateShutdownAudit, 1)
	wrapped := &lifecycleUpdateTestSession{HostMaintenanceSessionFuncs: session, ctx: lifecycle}
	deps := ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return wrapped, nil
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		StartJobRunner:    func(string, func()) {},
		AuditWithActor: func(_, _, action, _, _, status, message string, meta map[string]any) {
			if action == UpdateCompleteAction {
				audits <- updateShutdownAudit{status: status, message: message, meta: meta}
			}
		},
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
		LoadScheduledJobBehavior:  func(string) ScheduledJobBehavior { return ScheduledJobBehavior{ApprovalTimeout: time.Hour} },
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
	}
	if configure != nil {
		configure(&deps)
	}
	return NewService(deps), jm, state, audits
}

func assertUpdateInterrupted(t *testing.T, jm *jobs.Manager, state *servers.State, audits <-chan updateShutdownAudit) {
	t.Helper()
	job, err := jm.GetJob("update-shutdown-job")
	if err != nil {
		t.Fatalf("get update job: %v", err)
	}
	if job.Status != jobs.StatusInterrupted {
		t.Fatalf("job status = %q, want %q", job.Status, jobs.StatusInterrupted)
	}
	if job.ErrorClass != "interrupted" {
		t.Fatalf("job error class = %q, want interrupted", job.ErrorClass)
	}
	status := state.CurrentStatusSnapshot("srv-shutdown")
	if status == nil || status.Status != "idle" {
		t.Fatalf("runtime status = %+v, want idle", status)
	}
	select {
	case audit := <-audits:
		if audit.status != "interrupted" {
			t.Fatalf("audit status = %q, want interrupted", audit.status)
		}
		if got, _ := audit.meta["last_error_class"].(string); got != "interrupted" {
			t.Fatalf("audit last_error_class = %#v, want interrupted", audit.meta["last_error_class"])
		}
		if interrupted, _ := audit.meta["interrupted"].(bool); !interrupted {
			t.Fatalf("audit interrupted = %#v, want true", audit.meta["interrupted"])
		}
	case <-time.After(time.Second):
		t.Fatal("update completion audit was not emitted")
	}
}

func TestRunUpdateJobMarksCancelledPrechecksInterrupted(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	session := &HostMaintenanceSessionFuncs{
		RunUpdatePrechecksFunc: func(ctx context.Context) PrecheckSummary {
			close(started)
			<-ctx.Done()
			return PrecheckSummary{
				AllPassed:   false,
				FailedCheck: "application_shutdown",
				Results: []PrecheckResult{{
					Name:    "application_shutdown",
					Passed:  false,
					Details: ctx.Err().Error(),
				}},
			}
		},
	}
	service, jm, state, audits := newUpdateShutdownHarness(t, lifecycle, session, nil)
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunUpdateJob(UpdateRunRequest{
			Server: servers.Server{Name: "srv-shutdown", Host: "127.0.0.1", Port: 22, User: "root"},
			Actor:  "tester",
			JobID:  "update-shutdown-job",
			Policy: RetryPolicy{MaxAttempts: 1},
		})
	}()
	<-started
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("update precheck runner did not stop after lifecycle cancellation")
	}
	assertUpdateInterrupted(t, jm, state, audits)
}

func TestRunUpdateJobCancelsApprovalWait(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	waiting := make(chan struct{})
	var waitingOnce sync.Once
	session := &HostMaintenanceSessionFuncs{
		RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} },
		ListFailedSystemdUnitsFunc: func(context.Context) ([]string, string, error) { return nil, "", nil },
		RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
			return HostCommandResult{Attempts: 1}, nil
		},
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{
				PendingPackageCount: 1,
				Upgradable:          []string{"openssl"},
				PendingUpdates: []servers.PendingUpdate{{
					Package: "openssl",
				}},
			}}, nil
		},
		RunPlanDiskPrecheckFunc: func(context.Context, servers.UpgradePlan) PrecheckResult {
			return PrecheckResult{Name: "disk_space_plan", Passed: true}
		},
	}
	service, jm, state, audits := newUpdateShutdownHarness(t, lifecycle, session, func(deps *ServiceDeps) {
		deps.WaitForApprovalPollContext = func(ctx context.Context) error {
			waitingOnce.Do(func() { close(waiting) })
			<-ctx.Done()
			return ctx.Err()
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunUpdateJob(UpdateRunRequest{
			Server: servers.Server{Name: "srv-shutdown", Host: "127.0.0.1", Port: 22, User: "root"},
			Actor:  "tester",
			JobID:  "update-shutdown-job",
			Policy: RetryPolicy{MaxAttempts: 1},
		})
	}()
	select {
	case <-waiting:
	case <-time.After(time.Second):
		t.Fatal("update runner did not enter approval wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("approval wait did not stop after lifecycle cancellation")
	}
	assertUpdateInterrupted(t, jm, state, audits)
}

func TestRunUpdateJobDoesNotPersistCancelledFinalFacts(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	factsStarted := make(chan struct{})
	var factsOnce sync.Once
	savedFacts := false
	session := &HostMaintenanceSessionFuncs{
		RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} },
		ListFailedSystemdUnitsFunc: func(context.Context) ([]string, string, error) { return nil, "", nil },
		RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
			return HostCommandResult{Attempts: 1}, nil
		},
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{}, Attempts: 1}, nil
		},
		CollectServerFactsFunc: func(ctx context.Context) ServerFactsRecord {
			factsOnce.Do(func() { close(factsStarted) })
			<-ctx.Done()
			return ServerFactsRecord{ServerName: "srv-shutdown", CollectedAt: "2026-09-05T11:00:00Z", DiskStatus: "unknown", AptStatus: "unknown"}
		},
	}
	service, jm, state, audits := newUpdateShutdownHarness(t, lifecycle, session, func(deps *ServiceDeps) {
		deps.SaveServerFacts = func(ServerFactsRecord) error {
			savedFacts = true
			return nil
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunUpdateJob(UpdateRunRequest{
			Server: servers.Server{Name: "srv-shutdown", Host: "127.0.0.1", Port: 22, User: "root"},
			Actor:  "tester",
			JobID:  "update-shutdown-job",
			Policy: RetryPolicy{MaxAttempts: 1},
		})
	}()
	select {
	case <-factsStarted:
	case <-time.After(time.Second):
		t.Fatal("update runner did not reach final facts refresh")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("final facts refresh did not stop after lifecycle cancellation")
	}
	if savedFacts {
		t.Fatal("cancelled final facts were persisted")
	}
	assertUpdateInterrupted(t, jm, state, audits)
}