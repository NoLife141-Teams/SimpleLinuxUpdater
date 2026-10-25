package updates

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

type pendingApprovalRepository struct {
	jobs.Repository
	beforeWaiting func() error
}

func (r pendingApprovalRepository) ApplyTransition(record jobs.Record, revision int64, activeOnly bool) (bool, error) {
	if record.Status == jobs.StatusWaitingApproval && r.beforeWaiting != nil {
		if err := r.beforeWaiting(); err != nil {
			return false, err
		}
	}
	return r.Repository.ApplyTransition(record, revision, activeOnly)
}

func newPendingApprovalTestRepository(t *testing.T) *jobs.SQLiteRepository {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "approval.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	return jobs.NewSQLiteRepository(db)
}

func TestPendingApprovalIsNotPublishedBeforeJobCommit(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	baseRepository := newPendingApprovalTestRepository(t)
	repository := pendingApprovalRepository{Repository: baseRepository, beforeWaiting: func() error {
		close(entered)
		<-release
		return nil
	}}
	var syncCalls atomic.Int32
	jm := jobs.NewManager(repository, jobs.ManagerOptions{SyncRuntime: func(jobs.Record) { syncCalls.Add(1) }})
	fullLog := strings.Repeat("earlier command output\n", 4000)
	job, err := jm.CreateJob(jobs.CreateParams{Kind: jobs.KindUpdate, ServerName: "approval", Status: jobs.StatusRunning, LogsText: fullLog})
	if err != nil {
		t.Fatal(err)
	}
	syncCalls.Store(0)
	var mu sync.Mutex
	inventory := []servers.Server{{Name: "approval", User: "root"}}
	statuses := map[string]*servers.ServerStatus{"approval": {Name: "approval", Status: "updating", JobID: job.ID, Logs: BoundStatusLogs(fullLog)}}
	state := servers.NewState(&mu, &inventory, &statuses, nil)
	runner := &withActorRunner{server: inventory[0], jobID: job.ID, service: NewService(ServiceDeps{
		ServerState: state, CurrentJobManager: func() *jobs.Manager { return jm },
	})}
	done := make(chan bool, 1)
	go func() {
		done <- runner.withStatus(func(status *servers.ServerStatus) {
			status.Status = "pending_approval"
			status.ApprovalGeneration++
			status.PendingUpdates = []servers.PendingUpdate{{Package: "openssl"}}
			status.Logs += "\nUpgradable packages:\nopenssl"
		})
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("runner did not reach waiting-state persistence")
	}
	if mu.TryLock() {
		published := servers.CloneServerStatus(statuses["approval"])
		mu.Unlock()
		if published.Status == "pending_approval" || published.ApprovalGeneration != 0 {
			t.Error("approval identity became visible before the waiting job was committed")
		}
	}
	before, err := jm.GetJob(job.ID)
	if err != nil || before.Status != jobs.StatusRunning {
		t.Fatalf("test boundary is not before the waiting commit: %+v %v", before, err)
	}
	releaseOnce.Do(func() { close(release) })
	if !<-done {
		t.Fatal("pending publication failed")
	}
	published := state.CurrentStatusSnapshot("approval")
	saved, err := jm.GetJob(job.ID)
	if err != nil || saved.Status != jobs.StatusWaitingApproval || published.Status != "pending_approval" || published.ApprovalGeneration != 1 {
		t.Fatalf("committed pending plan mismatch: %+v %+v %v", published, saved, err)
	}
	logs, expired, truncated, err := baseRepository.ReadFullLog(job.ID)
	if err != nil || expired || truncated || logs != fullLog+"\nUpgradable packages:\nopenssl" {
		t.Fatalf("waiting publication replaced full output with the runtime preview: bytes=%d expired=%t truncated=%t err=%v", len(logs), expired, truncated, err)
	}
	if syncCalls.Load() != 0 {
		t.Error("waiting persistence re-entered runtime publication instead of leaving it to the owner")
	}
}

func TestUpdateRunnerStopsWhenPendingApprovalCannotBePersisted(t *testing.T) {
	var waitingWrites atomic.Int32
	repository := pendingApprovalRepository{Repository: newPendingApprovalTestRepository(t), beforeWaiting: func() error {
		if waitingWrites.Add(1) == 1 {
			return errors.New("injected waiting-state write failure")
		}
		return nil
	}}
	jm := jobs.NewManager(repository, jobs.ManagerOptions{NewID: func() string { return "update-shutdown-job" }})
	job, err := jm.CreateJob(jobs.CreateParams{Kind: jobs.KindUpdate, ServerName: "srv-shutdown", Status: jobs.StatusRunning})
	if err != nil {
		t.Fatal(err)
	}
	var polls, mutations int
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []servers.PendingUpdate{{Package: "openssl", CVEState: "done"}}}}, nil
		},
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			if strings.Contains(req.Command, "upgrade") {
				mutations++
			}
			return HostCommandResult{}, nil
		},
	}
	service, _, state, audits := newUpdateShutdownHarness(t, context.Background(), session, func(deps *ServiceDeps) {
		deps.CurrentJobManager = func() *jobs.Manager { return jm }
		deps.WaitForApprovalPollContext = func(context.Context) error {
			polls++
			deps.ServerState.CancelPendingUpdate("srv-shutdown")
			return nil
		}
	})
	service.RunUpdateJob(UpdateRunRequest{Server: state.CloneServers()[0], JobID: job.ID, Actor: "tester", Policy: RetryPolicy{MaxAttempts: 1}})
	if polls != 0 || mutations != 0 {
		t.Errorf("failed waiting commit still exposed approval or dispatched work: polls=%d mutations=%d", polls, mutations)
	}
	if current := state.CurrentStatusSnapshot("srv-shutdown"); current.Status != "error" || current.ApprovalGeneration != 0 {
		t.Errorf("failed waiting commit published a plan: %+v", current)
	}
	saved, err := jm.GetJob(job.ID)
	if err != nil || saved.Status != jobs.StatusFailed || saved.ErrorClass != "persistence" {
		t.Errorf("waiting-state persistence failure was not recorded: %+v %v", saved, err)
	}
	audit := <-audits
	if audit.status != "failure" || audit.meta["last_error_class"] != "persistence" || audit.meta["upgrade_completed"] != false {
		t.Errorf("missing persistence failure audit metadata: %+v", audit)
	}
}

func TestUpdateRunnerPublishesAndExpiresApprovalWithDurableGenerationAudit(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	actions := map[string]map[string]any{}
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []servers.PendingUpdate{{Package: "openssl", CVEState: "done"}}}}, nil
		},
	}
	service, jm, state, _ := newUpdateShutdownHarness(t, context.Background(), session, func(deps *ServiceDeps) {
		deps.Now = func() time.Time { return now }
		deps.WaitForApprovalPollContext = func(context.Context) error { now = now.Add(2 * time.Hour); return nil }
		deps.AuditWithActor = func(_, _, action, _, _, status, _ string, meta map[string]any) {
			if action == "approval.published" || action == "approval.expired" {
				if status != "success" {
					t.Errorf("accepted transition audit status %s", status)
				}
				job, err := deps.CurrentJobManager().GetJob("update-shutdown-job")
				if err != nil {
					t.Fatal(err)
				}
				if action == "approval.published" && (job.Status != jobs.StatusWaitingApproval || !strings.Contains(job.MetaJSON, `"approval_generation":1`)) {
					t.Fatalf("audit preceded durable generation: %+v", job)
				}
				if action == "approval.expired" && job.Status != jobs.StatusCancelled {
					t.Fatalf("expiry audit preceded transition: %+v", job)
				}
			}
			actions[action] = meta
		}
	})
	service.RunUpdateJob(UpdateRunRequest{Server: state.CloneServers()[0], JobID: "update-shutdown-job", Actor: "tester", Policy: RetryPolicy{MaxAttempts: 1}})
	job, err := jm.GetJob("update-shutdown-job")
	if err != nil || job.Status != jobs.StatusCancelled {
		t.Fatalf("%+v %v", job, err)
	}
	for _, action := range []string{"approval.published", "approval.expired"} {
		if actions[action]["job_id"] != "update-shutdown-job" || actions[action]["approval_generation"] != uint64(1) {
			t.Errorf("%s identity: %+v", action, actions[action])
		}
	}
	if actions[UpdateCompleteAction]["upgrade_completed"] != false {
		t.Fatal("expiry audit incorrectly claims package mutation")
	}
}

func TestAutomaticApprovalDoesNotPublishOperatorConditionDuringRevalidation(t *testing.T) {
	var jm *jobs.Manager
	discoveries := 0
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			discoveries++
			if discoveries == 2 {
				job, err := jm.GetJob("update-shutdown-job")
				if err != nil {
					t.Fatal(err)
				}
				// This deliberately observes the internal waiting job while the second
				// remote discovery is in progress, before the next persisted runner phase.
				if !strings.Contains(job.MetaJSON, `"approval_generation":0`) {
					t.Fatalf("auto approval exposed an actionable generation: %+v", job)
				}
			}
			return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []servers.PendingUpdate{{Package: "openssl", CVEState: "done"}}}}, nil
		},
	}
	service, manager, state, audits := newUpdateShutdownHarness(t, context.Background(), session, func(deps *ServiceDeps) {
		deps.SaveServerFacts = func(ServerFactsRecord) error { return nil }
		deps.LoadScheduledJobBehavior = func(string) ScheduledJobBehavior {
			return ScheduledJobBehavior{ApprovalTimeout: time.Hour, AutoApproveScope: ApprovalScopeAll}
		}
		original := deps.AuditWithActor
		deps.AuditWithActor = func(actor, ip, action, targetType, targetName, status, message string, meta map[string]any) {
			if action == "approval.published" {
				t.Error("automatic approval emitted operator publication")
			}
			original(actor, ip, action, targetType, targetName, status, message, meta)
		}
	})
	jm = manager
	service.RunUpdateJob(UpdateRunRequest{Server: state.CloneServers()[0], JobID: "update-shutdown-job", Actor: "tester", Policy: RetryPolicy{MaxAttempts: 1}})
	if discoveries < 2 {
		t.Fatal("test never reached approval revalidation")
	}
	audit := <-audits
	if audit.status != "success" || audit.meta["upgrade_completed"] != true {
		t.Fatalf("automatic update outcome changed: %+v", audit)
	}
}
