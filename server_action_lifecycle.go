package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
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

type serverActionApprovalSpec struct {
	scope             string
	rollbackLogPrefix string
	notFoundMeta      map[string]any
}

type serverActionApprovalOptions struct {
	confirmRemovals bool
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
	var server Server
	var err error
	if spec.packageMutation {
		server, err = l.serverState.BeginPackageMutation(name, spec.status)
	} else {
		server, err = l.serverState.BeginAction(name, spec.status)
	}
	if err != nil {
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
	job, err := createServerActionJobWithStateAndManager(l.currentJobManager(), l.serverState, spec.jobKind, name, actor, clientIP, policy)
	if err != nil {
		l.serverState.RestoreStatusSnapshot(name, preStartStatus)
		retryMeta["error"] = err.Error()
		l.recordAudit(spec.auditAction, name, "failure", "Failed to create job", retryMeta)
		return jsonResult(http.StatusInternalServerError, spec.createFailure)
	}
	preserveReconciliation := spec.preserveReconciliation && preStartStatus != nil && strings.EqualFold(strings.TrimSpace(preStartStatus.Status), runtimepkg.StatusNeedsReconciliation)
	l.startJobRunner(l.currentJobManager, job.ID, func() {
		if preserveReconciliation {
			defer l.serverState.RestoreStatusSnapshot(name, preStartStatus)
		}
		spec.runWithJob(l.updateService, server, actor, clientIP, policy, job.ID, sudoPassword)
	}, func() {
		l.serverState.RestoreStatusSnapshot(name, preStartStatus)
	})
	l.recordAudit(spec.auditAction, name, "started", spec.successMessage, retryMeta)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": spec.successMessage, "job_id": job.ID},
	}
}

func (l *serverActionLifecycle) ApproveAll(name string) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{}, serverActionApprovalSpec{
		scope:             updatespkg.ApprovalScopeAll,
		rollbackLogPrefix: "update approve",
	})
}

func (l *serverActionLifecycle) ApproveSecurity(name string) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{}, serverActionApprovalSpec{
		scope:             updatespkg.ApprovalScopeSecurity,
		rollbackLogPrefix: "security approve",
		notFoundMeta:      map[string]any{"scope": updatespkg.ApprovalScopeSecurity},
	})
}

func (l *serverActionLifecycle) ApproveKeptBackSecurity(name string, confirmRemovals bool) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{confirmRemovals: confirmRemovals}, serverActionApprovalSpec{
		scope:             updatespkg.ApprovalScopeSecurityKeptBack,
		rollbackLogPrefix: "kept-back security approve",
		notFoundMeta:      map[string]any{"scope": updatespkg.ApprovalScopeSecurityKeptBack},
	})
}

func (l *serverActionLifecycle) ApproveFullUpgrade(name string, confirmRemovals bool) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{confirmRemovals: confirmRemovals}, serverActionApprovalSpec{
		scope:             updatespkg.ApprovalScopeFullUpgrade,
		rollbackLogPrefix: "full approve",
		notFoundMeta:      map[string]any{"scope": updatespkg.ApprovalScopeFullUpgrade},
	})
}

func (l *serverActionLifecycle) approve(name string, opts serverActionApprovalOptions, spec serverActionApprovalSpec) serverActionLifecycleResult {
	preApproveStatus := l.serverState.CurrentStatusSnapshot(name)
	if preApproveStatus == nil {
		l.recordAuditWithMeta("update.approve", name, "failure", "Server not found", spec.notFoundMeta)
		return jsonResult(http.StatusNotFound, "Server not found")
	}
	if preApproveStatus.Status != "pending_approval" {
		l.recordAuditWithMeta("update.approve", name, "ignored", "Server not pending approval", map[string]any{"scope": spec.scope})
		return jsonResult(http.StatusConflict, "Server not pending approval")
	}
	approval := updatespkg.EvaluateManualApproval(preApproveStatus, spec.scope, updatespkg.ApprovalScopeOptions{ConfirmRemovals: opts.confirmRemovals})
	if !approval.Allowed {
		l.recordAuditWithMeta("update.approve", name, approval.AuditStatus, approval.AuditMessage, approval.AuditMeta)
		if len(approval.RemovedPackages) > 0 {
			return serverActionLifecycleResult{
				statusCode: http.StatusConflict,
				body: map[string]any{
					"error":            approval.BodyMessage,
					"removed_packages": approval.RemovedPackages,
				},
			}
		}
		return jsonResult(http.StatusConflict, approval.BodyMessage)
	}

	jm := l.currentJobManager()
	if jm == nil {
		l.recordAuditWithMeta("update.approve", name, "failure", "Failed to persist approval", map[string]any{"scope": spec.scope, "error": "job manager unavailable"})
		return jsonResult(http.StatusInternalServerError, "Failed to persist approval")
	}
	job, err := jm.FindLatestActiveJobByServerAndKind(name, jobKindUpdate)
	if err != nil {
		l.recordAuditWithMeta("update.approve", name, "failure", "Failed to persist approval", map[string]any{"scope": spec.scope, "error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist approval")
	}
	status := jobStatusRunning
	phase := jobPhaseAptUpgrade
	logs := preApproveStatus.Logs
	if err := jm.Transition(job.ID, JobTransitionIntent{
		Kind:     jobIntentResumeApproval,
		Status:   &status,
		Phase:    &phase,
		Summary:  &approval.JobSummary,
		LogsText: &logs,
	}); err != nil {
		l.recordAuditWithMeta("update.approve", name, "failure", "Failed to persist approval", map[string]any{"scope": spec.scope, "error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist approval")
	}
	approvalOptions := serverpkg.ApprovalOptions{ConfirmRemovals: approval.StateOptions.ConfirmRemovals}
	exists, approved := l.updateService.ApprovePendingUpdateWithOptions(name, spec.scope, approvalOptions)
	if !exists || !approved {
		rollbackStatus := jobStatusWaitingApproval
		rollbackPhase := jobPhaseApprovalWait
		rollbackSummary := "Waiting for approval"
		if rollbackErr := jm.Transition(job.ID, JobTransitionIntent{
			Kind:     jobIntentWaitApproval,
			Status:   &rollbackStatus,
			Phase:    &rollbackPhase,
			Summary:  &rollbackSummary,
			LogsText: &logs,
		}); rollbackErr != nil {
			log.Printf("%s rollback failed for job %q: %v", spec.rollbackLogPrefix, job.ID, rollbackErr)
		}
		l.recordAuditWithMeta("update.approve", name, "ignored", "Server not pending approval", map[string]any{"scope": spec.scope})
		return jsonResult(http.StatusConflict, "Server not pending approval")
	}
	l.recordAuditWithMeta("update.approve", name, approval.AuditStatus, approval.AuditMessage, approval.AuditMeta)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": approval.SuccessMessage},
	}
}

func (l *serverActionLifecycle) Cancel(name string) serverActionLifecycleResult {
	preCancelStatus := l.serverState.CurrentStatusSnapshot(name)
	if preCancelStatus == nil {
		l.recordAuditWithMeta("update.cancel", name, "failure", "Server not found", nil)
		return jsonResult(http.StatusNotFound, "Server not found")
	}
	if preCancelStatus.Status != "pending_approval" {
		l.recordAuditWithMeta("update.cancel", name, "ignored", "Server not pending approval", nil)
		return jsonResult(http.StatusConflict, "Server not pending approval")
	}
	logsBeforeCancel := preCancelStatus.Logs

	jm := l.currentJobManager()
	if jm == nil {
		l.recordAuditWithMeta("update.cancel", name, "failure", "Failed to persist cancelled update", map[string]any{"error": "job manager unavailable"})
		return jsonResult(http.StatusInternalServerError, "Failed to persist cancelled update")
	}
	job, err := jm.FindLatestActiveJobByServerAndKind(name, jobKindUpdate)
	if err != nil {
		l.recordAuditWithMeta("update.cancel", name, "failure", "Failed to persist cancelled update", map[string]any{"error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist cancelled update")
	}
	status := jobStatusCancelled
	phase := jobPhaseComplete
	summary := "Update cancelled"
	if err := jm.Transition(job.ID, JobTransitionIntent{
		Kind:     jobIntentCancel,
		Status:   &status,
		Phase:    &phase,
		Summary:  &summary,
		LogsText: &logsBeforeCancel,
	}); err != nil {
		l.recordAuditWithMeta("update.cancel", name, "failure", "Failed to persist cancelled update", map[string]any{"error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist cancelled update")
	}
	exists, cancelled := l.updateService.CancelPendingUpdate(name)
	if !exists || !cancelled {
		rollbackStatus := jobStatusWaitingApproval
		rollbackPhase := jobPhaseApprovalWait
		rollbackSummary := "Waiting for approval"
		if rollbackErr := jm.Transition(job.ID, JobTransitionIntent{
			Kind:     jobIntentWaitApproval,
			Status:   &rollbackStatus,
			Phase:    &rollbackPhase,
			Summary:  &rollbackSummary,
			LogsText: &logsBeforeCancel,
		}); rollbackErr != nil {
			log.Printf("cancel rollback failed for job %q: %v", job.ID, rollbackErr)
		}
		l.recordAuditWithMeta("update.cancel", name, "ignored", "Server not pending approval", nil)
		return jsonResult(http.StatusConflict, "Server not pending approval")
	}
	l.recordAuditWithMeta("update.cancel", name, "success", "Upgrade cancelled", nil)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": "Upgrade cancelled"},
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
