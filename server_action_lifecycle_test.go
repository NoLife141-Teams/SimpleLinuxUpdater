package main

import (
	"database/sql"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	serverpkg "debian-updater/internal/servers"
)

type lifecycleAuditRecord struct {
	action  string
	status  string
	message string
	meta    map[string]any
}

type lifecycleTestHarness struct {
	state       *serverpkg.State
	updateSvc   *UpdateService
	jobManager  *JobManager
	db          *sql.DB
	auditEvents []lifecycleAuditRecord
	runnerJobID string
	runnerRun   func()
}

func newLifecycleTestHarness(t *testing.T, server Server, status *ServerStatus) *lifecycleTestHarness {
	t.Helper()
	var stateMu sync.Mutex
	servers := []Server{server}
	statuses := map[string]*ServerStatus{}
	if status != nil {
		statuses[server.Name] = cloneServerStatus(status)
	}
	state := serverpkg.NewState(&stateMu, &servers, &statuses, statusInProgress)

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
	jm := newJobManagerWithRuntime(db, nil, state, func() bool { return false })
	h := &lifecycleTestHarness{
		state:      state,
		jobManager: jm,
		db:         db,
	}
	h.updateSvc = NewUpdateService(UpdateServiceDeps{
		ServerState: state,
		CurrentJobManager: func() *JobManager {
			return h.jobManager
		},
		StartJobRunner: func(string, func()) {},
	})
	return h
}

func (h *lifecycleTestHarness) lifecycle() *serverActionLifecycle {
	return &serverActionLifecycle{
		serverState:       h.state,
		updateService:     h.updateSvc,
		currentJobManager: func() *JobManager { return h.jobManager },
		startJobRunner: func(_ func() *JobManager, jobID string, run func()) {
			h.runnerJobID = jobID
			h.runnerRun = run
		},
		loadRetryPolicy: func() RetryPolicy {
			return RetryPolicy{MaxAttempts: 2, BaseDelay: time.Second, MaxDelay: 3 * time.Second, JitterPct: 0}
		},
		jobTimestampNow: func() string { return "2026-05-17T15:00:00.000000000Z" },
		audit: func(action, _, _, status, message string, meta map[string]any) {
			h.auditEvents = append(h.auditEvents, lifecycleAuditRecord{action: action, status: status, message: message, meta: meta})
		},
	}
}

func (h *lifecycleTestHarness) createPendingUpdateJob(t *testing.T, serverName string) JobRecord {
	t.Helper()
	job, err := h.jobManager.CreateJob(JobCreateParams{
		Kind:       jobKindUpdate,
		ServerName: serverName,
		Actor:      "tester",
		Status:     jobStatusWaitingApproval,
		Phase:      jobPhaseApprovalWait,
		Summary:    "Waiting for approval",
		LogsText:   "pending logs",
	})
	if err != nil {
		t.Fatalf("create pending update job: %v", err)
	}
	return job
}

func TestServerActionLifecycleStartUpdateSuccessDispatchesRunner(t *testing.T) {
	server := Server{Name: "srv-start", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "idle"})

	result := h.lifecycle().StartUpdate(server.Name, "alice", "192.0.2.10")

	if result.statusCode != http.StatusOK || result.body["message"] != "Update started" {
		t.Fatalf("StartUpdate result = %+v, want 200 update started", result)
	}
	jobID, _ := result.body["job_id"].(string)
	if strings.TrimSpace(jobID) == "" {
		t.Fatalf("StartUpdate result missing job_id: %+v", result.body)
	}
	if h.runnerJobID != jobID || h.runnerRun == nil {
		t.Fatalf("runner dispatch = job %q run nil? %t, want job %q and run func", h.runnerJobID, h.runnerRun == nil, jobID)
	}
	status := h.state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "updating" {
		t.Fatalf("runtime status = %+v, want updating", status)
	}
	var jobKind, actor, clientIP string
	if err := h.db.QueryRow("SELECT kind, actor, client_ip FROM jobs WHERE id = ?", jobID).Scan(&jobKind, &actor, &clientIP); err != nil {
		t.Fatalf("query job: %v", err)
	}
	if jobKind != jobKindUpdate || actor != "alice" || clientIP != "192.0.2.10" {
		t.Fatalf("job fields = kind=%q actor=%q ip=%q", jobKind, actor, clientIP)
	}
	if len(h.auditEvents) != 1 || h.auditEvents[0].status != "started" || h.auditEvents[0].message != "Update started" {
		t.Fatalf("audit events = %+v, want started update audit", h.auditEvents)
	}
}

func TestServerActionLifecycleStartRejectsMissingServerAndInProgress(t *testing.T) {
	server := Server{Name: "srv-busy", Host: "example.org", Port: 22, User: "root", Pass: "pw"}

	t.Run("missing server", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, nil)
		result := h.lifecycle().StartUpdate(server.Name, "alice", "192.0.2.10")
		if result.statusCode != http.StatusNotFound || result.body["error"] != "Server not found" {
			t.Fatalf("missing result = %+v, want 404 server not found", result)
		}
	})

	t.Run("action in progress", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "updating"})
		result := h.lifecycle().StartAutoremove(server.Name, "alice", "192.0.2.10")
		if result.statusCode != http.StatusConflict || result.body["error"] != "Update already in progress" {
			t.Fatalf("busy result = %+v, want 409 update already in progress", result)
		}
	})
}

func TestServerActionLifecycleStartJobFailureRollsBackRuntimeState(t *testing.T) {
	server := Server{Name: "srv-job-fail", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	original := &ServerStatus{Name: server.Name, Status: "idle", Logs: "ready", Upgradable: []string{"openssl"}}
	h := newLifecycleTestHarness(t, server, original)
	lifecycle := h.lifecycle()
	lifecycle.currentJobManager = func() *JobManager { return nil }

	result := lifecycle.StartUpdate(server.Name, "alice", "192.0.2.10")

	if result.statusCode != http.StatusInternalServerError || result.body["error"] != "Failed to create update job" {
		t.Fatalf("job failure result = %+v, want failed create update job", result)
	}
	restored := h.state.CurrentStatusSnapshot(server.Name)
	if restored == nil || restored.Status != "idle" || restored.Logs != "ready" || len(restored.Upgradable) != 1 {
		t.Fatalf("restored status = %+v, want original idle snapshot", restored)
	}
}

func TestServerActionLifecycleApproveSuccessUpdatesJobFirst(t *testing.T) {
	server := Server{Name: "srv-approve", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{
		Name:           server.Name,
		Status:         "pending_approval",
		Logs:           "pending logs",
		PendingUpdates: []PendingUpdate{{Package: "openssl", Security: true}},
	})
	job := h.createPendingUpdateJob(t, server.Name)

	result := h.lifecycle().ApproveSecurity(server.Name)

	if result.statusCode != http.StatusOK || result.body["message"] != "Security updates approved" {
		t.Fatalf("approve result = %+v, want security approval success", result)
	}
	status := h.state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "approved" || status.ApprovalScope != "security" {
		t.Fatalf("runtime status = %+v, want approved security", status)
	}
	var jobStatus, phase, summary string
	if err := h.db.QueryRow("SELECT status, phase, summary FROM jobs WHERE id = ?", job.ID).Scan(&jobStatus, &phase, &summary); err != nil {
		t.Fatalf("query approved job: %v", err)
	}
	if jobStatus != jobStatusRunning || phase != jobPhaseAptUpgrade || summary != "Security updates approved" {
		t.Fatalf("job after approve = %q/%q/%q, want running/apt_upgrade/security summary", jobStatus, phase, summary)
	}
}

func TestServerActionLifecycleApprovePreconditions(t *testing.T) {
	server := Server{Name: "srv-approval-preconditions", Host: "example.org", Port: 22, User: "root", Pass: "pw"}

	t.Run("not pending", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "idle"})
		result := h.lifecycle().ApproveAll(server.Name)
		if result.statusCode != http.StatusConflict || result.body["error"] != "Server not pending approval" {
			t.Fatalf("not pending result = %+v, want conflict", result)
		}
	})

	t.Run("kept back requires packages", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval"})
		result := h.lifecycle().ApproveKeptBackSecurity(server.Name, false)
		if result.statusCode != http.StatusConflict || result.body["error"] != "No kept-back security updates pending" {
			t.Fatalf("kept-back no packages result = %+v", result)
		}
	})

	t.Run("kept back requires fresh scan", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{
			Name:           server.Name,
			Status:         "pending_approval",
			PendingUpdates: []PendingUpdate{{Package: "linux-image", Security: true, KeptBack: true, RequiresFull: true}},
		})
		result := h.lifecycle().ApproveKeptBackSecurity(server.Name, false)
		if result.statusCode != http.StatusConflict || result.body["error"] != "Kept-back security upgrade requires a fresh package scan" {
			t.Fatalf("kept-back stale scan result = %+v", result)
		}
	})

	t.Run("kept back removals require confirmation", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{
			Name:           server.Name,
			Status:         "pending_approval",
			PendingUpdates: []PendingUpdate{{Package: "linux-image", Security: true, KeptBack: true, RequiresFull: true}},
			UpgradePlan: UpgradePlan{
				KeptBackSecurityPlanAvailable:   true,
				KeptBackSecurityRemovedPackages: []string{"old-kernel"},
			},
		})
		result := h.lifecycle().ApproveKeptBackSecurity(server.Name, false)
		if result.statusCode != http.StatusConflict || result.body["error"] != "Kept-back security upgrade may remove packages; confirmation required" {
			t.Fatalf("kept-back confirmation result = %+v", result)
		}
	})

	t.Run("full requires fresh scan", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval"})
		result := h.lifecycle().ApproveFullUpgrade(server.Name, false)
		if result.statusCode != http.StatusConflict || result.body["error"] != "Full upgrade requires a fresh package scan" {
			t.Fatalf("full stale scan result = %+v", result)
		}
	})

	t.Run("full removals require confirmation", func(t *testing.T) {
		h := newLifecycleTestHarness(t, server, &ServerStatus{
			Name:   server.Name,
			Status: "pending_approval",
			UpgradePlan: UpgradePlan{
				FullUpgradePlanAvailable:   true,
				FullUpgradeRemovedPackages: []string{"obsolete"},
			},
		})
		result := h.lifecycle().ApproveFullUpgrade(server.Name, false)
		if result.statusCode != http.StatusConflict || result.body["error"] != "Full upgrade would remove packages; confirmation required" {
			t.Fatalf("full confirmation result = %+v", result)
		}
	})
}

func TestServerActionLifecycleApproveJobManagerUnavailable(t *testing.T) {
	server := Server{Name: "srv-no-job-manager", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval", Logs: "pending logs"})
	lifecycle := h.lifecycle()
	lifecycle.currentJobManager = func() *JobManager { return nil }

	result := lifecycle.ApproveAll(server.Name)

	if result.statusCode != http.StatusInternalServerError || result.body["error"] != "Failed to persist approval" {
		t.Fatalf("no manager result = %+v, want persist approval failure", result)
	}
	status := h.state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "pending_approval" {
		t.Fatalf("runtime status = %+v, want still pending", status)
	}
}

func TestServerActionLifecycleApproveRaceRollsBackJob(t *testing.T) {
	server := Server{Name: "srv-approve-race", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval", Logs: "pending logs"})
	job := h.createPendingUpdateJob(t, server.Name)

	var otherMu sync.Mutex
	otherServers := []Server{server}
	otherStatuses := map[string]*ServerStatus{}
	otherState := serverpkg.NewState(&otherMu, &otherServers, &otherStatuses, statusInProgress)
	h.updateSvc = NewUpdateService(UpdateServiceDeps{ServerState: otherState})

	result := h.lifecycle().ApproveAll(server.Name)

	if result.statusCode != http.StatusConflict || result.body["error"] != "Server not pending approval" {
		t.Fatalf("race result = %+v, want not pending conflict", result)
	}
	var jobStatus, phase, summary string
	if err := h.db.QueryRow("SELECT status, phase, summary FROM jobs WHERE id = ?", job.ID).Scan(&jobStatus, &phase, &summary); err != nil {
		t.Fatalf("query rolled back job: %v", err)
	}
	if jobStatus != jobStatusWaitingApproval || phase != jobPhaseApprovalWait || summary != "Waiting for approval" {
		t.Fatalf("job after rollback = %q/%q/%q, want waiting approval", jobStatus, phase, summary)
	}
}

func TestServerActionLifecycleCancelSuccess(t *testing.T) {
	server := Server{Name: "srv-cancel", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{
		Name:           server.Name,
		Status:         "pending_approval",
		Logs:           "pending logs",
		Upgradable:     []string{"openssl"},
		PendingUpdates: []PendingUpdate{{Package: "openssl", Security: true}},
	})
	job := h.createPendingUpdateJob(t, server.Name)

	result := h.lifecycle().Cancel(server.Name)

	if result.statusCode != http.StatusOK || result.body["message"] != "Upgrade cancelled" {
		t.Fatalf("cancel result = %+v, want success", result)
	}
	status := h.state.CurrentStatusSnapshot(server.Name)
	if status == nil || status.Status != "cancelled" || status.Logs != "" || len(status.PendingUpdates) != 0 {
		t.Fatalf("runtime status = %+v, want cleared cancelled state", status)
	}
	var jobStatus, phase, summary, logs, finishedAt string
	if err := h.db.QueryRow("SELECT status, phase, summary, logs_text, finished_at FROM jobs WHERE id = ?", job.ID).Scan(&jobStatus, &phase, &summary, &logs, &finishedAt); err != nil {
		t.Fatalf("query cancelled job: %v", err)
	}
	if jobStatus != jobStatusCancelled || phase != jobPhaseComplete || summary != "Update cancelled" || logs != "pending logs" || finishedAt == "" {
		t.Fatalf("job after cancel = status=%q phase=%q summary=%q logs=%q finished=%q", jobStatus, phase, summary, logs, finishedAt)
	}
}
