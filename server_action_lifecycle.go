package main

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
)

type serverActionLifecycle struct {
	serverState          *serverpkg.State
	updateService        *UpdateService
	currentJobManager    func() *JobManager
	startJobRunner       func(func() *JobManager, string, func(), ...func())
	loadRetryPolicy      func() RetryPolicy
	audit                func(action, targetType, targetName, status, message string, meta map[string]any)
	maintenanceReadiness func(Server) serverpkg.MaintenanceReadiness
}

type serverActionLifecycleResult struct {
	statusCode int
	body       map[string]any
}

type serverActionStartSpec struct {
	status                 string
	jobKind                string
	auditAction            string
	startFailure           string
	createFailure          string
	successMessage         string
	missingPasswordOK      bool
	allowedStatuses        map[string]bool
	invalidStatus          string
	packageMutation        bool
	preserveReconciliation bool
	runWithJob             func(*UpdateService, Server, string, string, RetryPolicy, string, string)
}

func newServerActionLifecycle(deps AppDeps, audit func(action, targetType, targetName, status, message string, meta map[string]any)) *serverActionLifecycle {
	deps = deps.withDefaults()
	currentJobs := deps.CurrentJobManager
	if deps.UpdateService != nil {
		currentJobs = updateServiceEnsureDeps(deps.UpdateService).CurrentJobManager
	}
	if currentJobs == nil {
		currentJobs = currentJobManager
	}
	if audit == nil {
		audit = func(string, string, string, string, string, map[string]any) {}
	}
	return &serverActionLifecycle{
		serverState:       deps.ServerState,
		updateService:     deps.UpdateService,
		currentJobManager: currentJobs,
		startJobRunner:    startJobRunnerWithManager,
		loadRetryPolicy:   loadRetryPolicyFromEnv,
		audit:             audit,
		maintenanceReadiness: func(server Server) serverpkg.MaintenanceReadiness {
			return deps.MaintenanceReadiness([]Server{server})[server.Name]
		},
	}
}

func (l *serverActionLifecycle) StartUpdate(name, actor, clientIP string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, "", serverActionStartSpec{
		status:          "updating",
		packageMutation: true,
		jobKind:         jobKindUpdate,
		auditAction:     "update.start",
		startFailure:    "Failed to start update",
		createFailure:   "Failed to create update job",
		successMessage:  "Update started",
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, _ string) {
			service.RunUpdateJob(UpdateRunRequest{
				Server:   server,
				Actor:    actor,
				ClientIP: clientIP,
				Policy:   policy,
				JobID:    jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) StartAutoremove(name, actor, clientIP string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, "", serverActionStartSpec{
		status:          "autoremove",
		packageMutation: true,
		jobKind:         jobKindAutoremove,
		auditAction:     "autoremove.start",
		startFailure:    "Failed to start autoremove",
		createFailure:   "Failed to create autoremove job",
		successMessage:  "Autoremove started",
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, _ string) {
			service.RunAutoremoveJob(AutoremoveRunRequest{
				Server:   server,
				Actor:    actor,
				ClientIP: clientIP,
				Policy:   policy,
				JobID:    jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) StartAptRepair(name, actor, clientIP string, confirmed bool) serverActionLifecycleResult {
	if !confirmed {
		l.recordAudit("apt_repair.start", name, "failure", "APT repair confirmation required", retryPolicyMeta(l.retryPolicy()))
		return jsonResult(http.StatusBadRequest, "APT repair confirmation required")
	}
	return l.startAction(name, actor, clientIP, "", serverActionStartSpec{
		status:         runtimepkg.StatusRepairing,
		jobKind:        jobKindAptRepair,
		auditAction:    "apt_repair.start",
		startFailure:   "Failed to start APT repair",
		createFailure:  "Failed to create APT repair job",
		successMessage: "APT repair started",
		allowedStatuses: map[string]bool{
			runtimepkg.StatusNeedsReconciliation: true,
		},
		invalidStatus: "Server does not require APT repair",
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, _ string) {
			service.RunAptRepairJob(AptRepairRunRequest{
				Server:   server,
				Actor:    actor,
				ClientIP: clientIP,
				Policy:   policy,
				JobID:    jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) StartReboot(name, actor, clientIP string, confirmed bool) serverActionLifecycleResult {
	if !confirmed {
		l.recordAudit("reboot.start", name, "failure", "Reboot confirmation required", retryPolicyMeta(l.retryPolicy()))
		return jsonResult(http.StatusBadRequest, "Reboot confirmation required")
	}
	return l.startAction(name, actor, clientIP, "", serverActionStartSpec{
		status:         runtimepkg.StatusRebooting,
		jobKind:        jobKindReboot,
		auditAction:    "reboot.start",
		startFailure:   "Failed to start reboot",
		createFailure:  "Failed to create reboot job",
		successMessage: "Reboot started",
		allowedStatuses: map[string]bool{
			runtimepkg.StatusIdle:      true,
			runtimepkg.StatusDone:      true,
			runtimepkg.StatusError:     true,
			runtimepkg.StatusCancelled: true,
		},
		invalidStatus: "Server is not ready for a controlled reboot",
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, _ string) {
			service.RunRebootJob(RebootRunRequest{
				Server:   server,
				Actor:    actor,
				ClientIP: clientIP,
				Policy:   policy,
				JobID:    jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) StartSudoersEnable(name, actor, clientIP, sudoPassword string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, sudoPassword, serverActionStartSpec{
		status:                 "sudoers",
		jobKind:                jobKindSudoersEnable,
		auditAction:            "sudoers.enable.start",
		startFailure:           "Failed to start sudoers setup",
		createFailure:          "Failed to create sudoers job",
		successMessage:         "Sudoers setup started",
		preserveReconciliation: true,
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, sudoPassword string) {
			service.RunSudoersBootstrapJob(SudoersRunRequest{
				Server:       server,
				SudoPassword: sudoPassword,
				Actor:        actor,
				ClientIP:     clientIP,
				Policy:       policy,
				JobID:        jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) StartSudoersDisable(name, actor, clientIP, sudoPassword string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, sudoPassword, serverActionStartSpec{
		status:                 "sudoers",
		jobKind:                jobKindSudoersDisable,
		auditAction:            "sudoers.disable.start",
		startFailure:           "Failed to start sudoers disable",
		createFailure:          "Failed to create sudoers disable job",
		successMessage:         "Sudoers disable started",
		preserveReconciliation: true,
		runWithJob: func(service *UpdateService, server Server, actor, clientIP string, policy RetryPolicy, jobID, sudoPassword string) {
			service.RunSudoersDisableJob(SudoersRunRequest{
				Server:       server,
				SudoPassword: sudoPassword,
				Actor:        actor,
				ClientIP:     clientIP,
				Policy:       policy,
				JobID:        jobID,
			})
		},
	})
}

func (l *serverActionLifecycle) startAction(name, actor, clientIP, sudoPassword string, spec serverActionStartSpec) serverActionLifecycleResult {
	policy := l.retryPolicy()
	retryMeta := retryPolicyMeta(policy)
	if !spec.missingPasswordOK && strings.Contains(spec.auditAction, "sudoers.") && strings.TrimSpace(sudoPassword) == "" {
		l.recordAudit(spec.auditAction, name, "failure", "Missing sudo password", retryMeta)
		return jsonResult(http.StatusBadRequest, "missing sudo password")
	}
	preStartStatus := l.serverState.CurrentStatusSnapshot(name)
	if preStartStatus == nil {
		l.recordAudit(spec.auditAction, name, "failure", "Server not found", retryMeta)
		return jsonResult(http.StatusNotFound, "Server not found")
	}
	if preStartStatus != nil && spec.packageMutation && strings.EqualFold(strings.TrimSpace(preStartStatus.Status), runtimepkg.StatusNeedsReconciliation) {
		retryMeta["current_status"] = preStartStatus.Status
		l.recordAudit(spec.auditAction, name, "ignored", "APT reconciliation is required before another package mutation", retryMeta)
		return jsonResult(http.StatusConflict, "APT reconciliation is required before another package mutation")
	}
	if preStartStatus != nil && len(spec.allowedStatuses) > 0 && !spec.allowedStatuses[strings.ToLower(strings.TrimSpace(preStartStatus.Status))] {
		message := strings.TrimSpace(spec.invalidStatus)
		if message == "" {
			message = "Action is not available for the current server status"
		}
		retryMeta["current_status"] = preStartStatus.Status
		l.recordAudit(spec.auditAction, name, "ignored", message, retryMeta)
		return jsonResult(http.StatusConflict, message)
	}
	if preStartStatus != nil && statusInProgress(preStartStatus.Status) {
		retryMeta["current_status"] = preStartStatus.Status
		l.recordAudit(spec.auditAction, name, "failure", "Action already in progress", retryMeta)
		return jsonResult(http.StatusConflict, "Update already in progress")
	}
	serverForReadiness, found := serverByName(l.serverState, name)
	if !found {
		l.recordAudit(spec.auditAction, name, "failure", "Server not found", retryMeta)
		return jsonResult(http.StatusNotFound, "Server not found")
	}
	if l.maintenanceReadiness != nil {
		readiness := l.maintenanceReadiness(serverForReadiness)
		if !readiness.Ready {
			retryMeta["reason_code"] = readiness.Code
			l.recordAudit(spec.auditAction, name, "ignored", readiness.Message, retryMeta)
			return jsonResult(http.StatusConflict, readiness.Message)
		}
	}
	server, admittedSnapshot, err := l.serverState.BeginActionWithOptions(name, spec.status, serverpkg.ActionAdmissionOptions{
		PackageMutation: spec.packageMutation, AllowedStatuses: spec.allowedStatuses,
	})
	if err != nil {
		if errors.Is(err, serverpkg.ErrActionNotAllowed) {
			l.recordAudit(spec.auditAction, name, "ignored", spec.invalidStatus, retryMeta)
			return jsonResult(http.StatusConflict, spec.invalidStatus)
		}
		if errors.Is(err, sql.ErrNoRows) {
			l.recordAudit(spec.auditAction, name, "failure", "Server not found", retryMeta)
			return jsonResult(http.StatusNotFound, "Server not found")
		}
		if errors.Is(err, errActionInProgress) {
			l.recordAudit(spec.auditAction, name, "failure", "Action already in progress", retryMeta)
			return jsonResult(http.StatusConflict, "Update already in progress")
		}
		retryMeta["error"] = err.Error()
		l.recordAudit(spec.auditAction, name, "failure", spec.startFailure, retryMeta)
		return jsonResult(http.StatusInternalServerError, spec.startFailure)
	}
	preStartStatus = admittedSnapshot
	admittedGeneration := admittedSnapshot.ActionGeneration + 1
	job, err := createServerActionJobWithStateAndManager(l.currentJobManager(), l.serverState, spec.jobKind, name, actor, clientIP, policy)
	if err != nil {
		l.serverState.RestoreAdmittedAction(name, admittedGeneration, preStartStatus)
		retryMeta["error"] = err.Error()
		l.recordAudit(spec.auditAction, name, "failure", "Failed to create job", retryMeta)
		return jsonResult(http.StatusInternalServerError, spec.createFailure)
	}
	preserveReconciliation := spec.preserveReconciliation && preStartStatus != nil && strings.EqualFold(strings.TrimSpace(preStartStatus.Status), runtimepkg.StatusNeedsReconciliation)
	l.startJobRunner(l.currentJobManager, job.ID, func() {
		if preserveReconciliation {
			defer l.serverState.RestoreAdmittedAction(name, admittedGeneration, preStartStatus)
		}
		spec.runWithJob(l.updateService, server, actor, clientIP, policy, job.ID, sudoPassword)
	}, func() {
		l.serverState.RestoreAdmittedAction(name, admittedGeneration, preStartStatus)
	})
	l.recordAudit(spec.auditAction, name, "started", spec.successMessage, retryMeta)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": spec.successMessage, "job_id": job.ID},
	}
}

func (l *serverActionLifecycle) retryPolicy() RetryPolicy {
	if l.loadRetryPolicy == nil {
		return loadRetryPolicyFromEnv()
	}
	return l.loadRetryPolicy()
}

func (l *serverActionLifecycle) recordAudit(action, targetName, status, message string, meta map[string]any) {
	l.recordAuditWithMeta(action, targetName, status, message, meta)
}

func (l *serverActionLifecycle) recordAuditWithMeta(action, targetName, status, message string, meta map[string]any) {
	if l.audit == nil {
		return
	}
	l.audit(action, "server", targetName, status, message, meta)
}

func retryPolicyMeta(policy RetryPolicy) map[string]any {
	return map[string]any{
		"max_attempts":        policy.MaxAttempts,
		"base_delay_ms":       int(policy.BaseDelay / time.Millisecond),
		"max_delay_ms":        int(policy.MaxDelay / time.Millisecond),
		"jitter_pct":          policy.JitterPct,
		"total_attempts_used": 0,
		"retry_exhausted":     false,
	}
}

func jsonResult(status int, err string) serverActionLifecycleResult {
	return serverActionLifecycleResult{
		statusCode: status,
		body:       map[string]any{"error": err},
	}
}
