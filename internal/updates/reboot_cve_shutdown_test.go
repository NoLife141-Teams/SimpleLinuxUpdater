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

	_ "modernc.org/sqlite"
)

func newShutdownJobManager(t *testing.T, jobID string) *jobs.Manager {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), jobID+".db"))
	if err != nil {
		t.Fatalf("open jobs db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := jobs.EnsureSchema(db); err != nil {
		t.Fatalf("ensure jobs schema: %v", err)
	}
	return jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{NewID: func() string { return jobID }})
}

func TestRunRebootJobCancelsVerificationWait(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	server := servers.Server{Name: "srv-reboot-shutdown", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	jobID := "reboot-shutdown-job"
	jm := newShutdownJobManager(t, jobID)
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindReboot,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create reboot job: %v", err)
	}

	sleepStarted := make(chan struct{})
	var sleepOnce sync.Once
	audits := make(chan updateShutdownAudit, 1)
	session := &lifecycleUpdateTestSession{
		ctx: lifecycle,
		HostMaintenanceSessionFuncs: &HostMaintenanceSessionFuncs{
			CollectServerFactsFunc: func(context.Context) ServerFactsRecord {
				return ServerFactsRecord{ServerName: server.Name, UptimeSeconds: 3600, RunningKernelVersion: "6.8.0"}
			},
			RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Operation != "reboot.command" {
					t.Fatalf("unexpected command operation %q", req.Operation)
				}
				return HostCommandResult{Attempts: 1}, nil
			},
		},
	}
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return session, nil
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		AuditWithActor: func(_, _, action, _, _, status, message string, meta map[string]any) {
			if action == "reboot.complete" {
				audits <- updateShutdownAudit{status: status, message: message, meta: meta}
			}
		},
		SleepContext: func(ctx context.Context, _ time.Duration) error {
			sleepOnce.Do(func() { close(sleepStarted) })
			<-ctx.Done()
			return ctx.Err()
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunRebootJob(RebootRunRequest{
			Server: server,
			Actor:  "tester",
			JobID:  jobID,
			Policy: RetryPolicy{MaxAttempts: 1},
		})
	}()

	select {
	case <-sleepStarted:
	case <-time.After(time.Second):
		t.Fatal("reboot runner did not enter verification wait")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reboot verification did not stop after lifecycle cancellation")
	}

	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get reboot job: %v", err)
	}
	if job.Status != jobs.StatusInterrupted || job.ErrorClass != "interrupted" {
		t.Fatalf("reboot job = status %q error_class %q, want interrupted/interrupted", job.Status, job.ErrorClass)
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "idle" {
		t.Fatalf("reboot runtime status = %+v, want idle", status)
	}
	select {
	case audit := <-audits:
		if audit.status != "interrupted" {
			t.Fatalf("reboot audit status = %q, want interrupted", audit.status)
		}
		if got, _ := audit.meta["last_error_class"].(string); got != "interrupted" {
			t.Fatalf("reboot audit last_error_class = %#v, want interrupted", audit.meta["last_error_class"])
		}
	case <-time.After(time.Second):
		t.Fatal("reboot completion audit was not emitted")
	}
}

func TestPendingCVEEnrichmentCancellationIsInterruptedWithoutAvailabilityMutation(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	server := servers.Server{Name: "srv-cve-shutdown", Host: "127.0.0.1", Port: 22, User: "root"}
	pending := []servers.PendingUpdate{{Package: "openssl", CVEState: "pending"}}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {
			Name:           server.Name,
			Status:         "pending_approval",
			PendingUpdates: servers.ClonePendingUpdates(pending),
			Upgradable:     []string{"openssl"},
		},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	jobID := "cve-shutdown-job"
	jm := newShutdownJobManager(t, jobID)
	scanStarted := make(chan struct{})
	var scanOnce sync.Once
	runnerDone := make(chan struct{})

	session := &lifecycleUpdateTestSession{ctx: lifecycle, HostMaintenanceSessionFuncs: &HostMaintenanceSessionFuncs{}}
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return session, nil
		}),
		VulnerabilityScanner: VulnerabilityScannerFunc(func(ctx context.Context, _ HostMaintenanceSession, _ []servers.PendingUpdate) ([]servers.PendingUpdate, error) {
			scanOnce.Do(func() { close(scanStarted) })
			<-ctx.Done()
			return nil, ctx.Err()
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		StartJobRunner: func(_ string, run func()) {
			go func() {
				defer close(runnerDone)
				run()
			}()
		},
	})

	service.StartPendingCVEEnrichment(server, pending, "", "tester", "")
	select {
	case <-scanStarted:
	case <-time.After(time.Second):
		t.Fatal("CVE enrichment did not start vulnerability lookup")
	}
	cancel()
	select {
	case <-runnerDone:
	case <-time.After(time.Second):
		t.Fatal("CVE enrichment did not stop after lifecycle cancellation")
	}

	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get CVE enrichment job: %v", err)
	}
	if job.Status != jobs.StatusInterrupted || job.ErrorClass != "interrupted" {
		t.Fatalf("CVE enrichment job = status %q error_class %q, want interrupted/interrupted", job.Status, job.ErrorClass)
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || len(status.PendingUpdates) != 1 {
		t.Fatalf("pending update state = %+v, want one pending update", status)
	}
	if got := status.PendingUpdates[0].CVEState; got != "pending" {
		t.Fatalf("CVE state after shutdown = %q, want pending", got)
	}
	if status.Status != "pending_approval" {
		t.Fatalf("parent runtime status after CVE shutdown = %q, want pending_approval", status.Status)
	}
}
