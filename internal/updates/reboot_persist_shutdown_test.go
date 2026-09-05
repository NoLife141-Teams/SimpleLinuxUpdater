package updates

import (
	"context"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

func TestRunRebootJobCancellationDuringFactsPersistenceIsInterrupted(t *testing.T) {
	lifecycle, cancel := context.WithCancel(context.Background())
	server := servers.Server{Name: "srv-reboot-persist-shutdown", Host: "127.0.0.1", Port: 22, User: "root"}
	inventory := []servers.Server{server}
	statuses := map[string]*servers.ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	}
	state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
	jobID := "reboot-persist-shutdown-job"
	jm := newShutdownJobManager(t, jobID)
	if _, err := jm.CreateJob(jobs.CreateParams{
		Kind:       jobs.KindReboot,
		ServerName: server.Name,
		Actor:      "tester",
		Status:     jobs.StatusRunning,
	}); err != nil {
		t.Fatalf("create reboot job: %v", err)
	}

	var factsMu sync.Mutex
	factsCalls := 0
	session := &lifecycleUpdateTestSession{
		ctx: lifecycle,
		HostMaintenanceSessionFuncs: &HostMaintenanceSessionFuncs{
			CollectServerFactsFunc: func(context.Context) ServerFactsRecord {
				factsMu.Lock()
				defer factsMu.Unlock()
				factsCalls++
				if factsCalls == 1 {
					return ServerFactsRecord{ServerName: server.Name, UptimeSeconds: 3600, RunningKernelVersion: "6.8.0-old"}
				}
				return ServerFactsRecord{ServerName: server.Name, UptimeSeconds: 12, RunningKernelVersion: "6.8.0-new"}
			},
			RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Operation != "reboot.command" {
					t.Fatalf("unexpected command operation %q", req.Operation)
				}
				return HostCommandResult{Attempts: 1}, nil
			},
		},
	}

	saveStarted := make(chan struct{})
	releaseSave := make(chan struct{})
	var saveOnce sync.Once
	audits := make(chan updateShutdownAudit, 1)
	service := NewService(ServiceDeps{
		ServerState: state,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return session, nil
		}),
		CurrentJobManager: func() *jobs.Manager { return jm },
		SleepContext: func(context.Context, time.Duration) error { return nil },
		SaveServerFacts: func(ServerFactsRecord) error {
			saveOnce.Do(func() { close(saveStarted) })
			<-releaseSave
			return nil
		},
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
	case <-saveStarted:
	case <-time.After(time.Second):
		t.Fatal("reboot runner did not enter post-reboot facts persistence")
	}
	cancel()
	close(releaseSave)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("reboot runner did not stop after cancellation during facts persistence")
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
