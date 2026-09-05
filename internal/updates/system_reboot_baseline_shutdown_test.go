package updates

import (
	"context"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

func TestSudoersDisableCancellationIsInterrupted(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	server := servers.Server{Name: "srv-sudoers-shutdown", Host: "127.0.0.1", Port: 22, User: "operator"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	jobID := "sudoers-shutdown-job"
	jm := newShutdownJobManager(t, jobID)
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindSudoersDisable,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create sudoers job: %v", err)
	}

	commandStarted := make(chan struct{})
	var commandOnce sync.Once
	audits := make(chan updateShutdownAudit, 1)
	session := &lifecycleUpdateTestSession{
		ctx: lifecycle,
		HostMaintenanceSessionFuncs: &HostMaintenanceSessionFuncs{
			RunCommandFunc: func(ctx context.Context, _ HostCommandRequest) (HostCommandResult, error) {
				commandOnce.Do(func() { close(commandStarted) })
				<-ctx.Done()
				return HostCommandResult{Attempts: 1}, ctx.Err()
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
			if action == "sudoers.disable.complete" {
				audits <- updateShutdownAudit{status: status, message: message, meta: meta}
			}
		},
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		service.RunSudoersDisableJob(SudoersRunRequest{
			Server:       server,
			SudoPassword: "ignored-in-test",
			Actor:        "tester",
			JobID:        jobID,
			Policy:       RetryPolicy{MaxAttempts: 1},
		})
	}()
	select {
	case <-commandStarted:
	case <-time.After(time.Second):
		t.Fatal("sudoers disable command did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sudoers disable runner did not stop after lifecycle cancellation")
	}

	job, err := jm.GetJob(jobID)
	if err != nil {
		t.Fatalf("get sudoers job: %v", err)
	}
	if job.Status != jobs.StatusInterrupted || job.ErrorClass != "interrupted" {
		t.Fatalf("sudoers job = status %q error_class %q, want interrupted/interrupted", job.Status, job.ErrorClass)
	}
	status := state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "idle" {
		t.Fatalf("sudoers runtime status = %+v, want idle", status)
	}
	select {
	case audit := <-audits:
		if audit.status != "interrupted" {
			t.Fatalf("sudoers audit status = %q, want interrupted", audit.status)
		}
		if got, _ := audit.meta["last_error_class"].(string); got != "interrupted" {
			t.Fatalf("sudoers audit last_error_class = %#v, want interrupted", audit.meta["last_error_class"])
		}
	case <-time.After(time.Second):
		t.Fatal("sudoers completion audit was not emitted")
	}
}

func TestRunRebootJobCancelsBaselineCollectionBeforeSendingReboot(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	server := servers.Server{Name: "srv-reboot-baseline-shutdown", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	jobID := "reboot-baseline-shutdown-job"
	jm := newShutdownJobManager(t, jobID)
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindReboot,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create reboot job: %v", err)
	}

	baselineStarted := make(chan struct{})
	var baselineOnce sync.Once
	commandCalled := false
	audits := make(chan updateShutdownAudit, 1)
	session := &lifecycleUpdateTestSession{
		ctx: lifecycle,
		HostMaintenanceSessionFuncs: &HostMaintenanceSessionFuncs{
			CollectServerFactsFunc: func(ctx context.Context) ServerFactsRecord {
				baselineOnce.Do(func() { close(baselineStarted) })
				<-ctx.Done()
				return ServerFactsRecord{}
			},
			RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				commandCalled = true
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
	case <-baselineStarted:
	case <-time.After(time.Second):
		t.Fatal("reboot baseline collection did not start")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reboot baseline collection did not stop after lifecycle cancellation")
	}
	if commandCalled {
		t.Fatal("reboot command was sent after baseline collection was cancelled")
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
