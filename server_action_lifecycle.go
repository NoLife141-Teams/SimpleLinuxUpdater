package main

import (
	"database/sql"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	serverpkg "debian-updater/internal/servers"
)

type serverActionLifecycle struct {
	serverState       *serverpkg.State
	updateService     *UpdateService
	currentJobManager func() *JobManager
	startJobRunner    func(func() *JobManager, string, func())
	loadRetryPolicy   func() RetryPolicy
	jobTimestampNow   func() string
	audit             func(action, targetType, targetName, status, message string, meta map[string]any)
}

type serverActionLifecycleResult struct {
	statusCode         int
	body               map[string]any
	maintenanceBlocked bool
}

type serverActionStartSpec struct {
	status            string
	jobKind           string
	auditAction       string
	startFailure      string
	createFailure     string
	successMessage    string
	missingPasswordOK bool
	runWithJob        func(*UpdateService, Server, string, string, RetryPolicy, string, string)
}

type serverActionApprovalSpec struct {
	scope             string
	jobSummary        string
	successMessage    string
	rollbackLogPrefix string
	notFoundMeta      map[string]any
	precheck          func(*ServerStatus, serverActionApprovalOptions) *serverActionLifecycleResult
	approvalOptions   func(serverActionApprovalOptions) serverpkg.ApprovalOptions
	successAuditMeta  func(*ServerStatus) map[string]any
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
		jobTimestampNow:   jobTimestampNow,
		audit:             audit,
	}
}

func (l *serverActionLifecycle) StartUpdate(name, actor, clientIP string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, "", serverActionStartSpec{
		status:         "updating",
		jobKind:        jobKindUpdate,
		auditAction:    "update.start",
		startFailure:   "Failed to start update",
		createFailure:  "Failed to create update job",
		successMessage: "Update started",
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
		status:         "autoremove",
		jobKind:        jobKindAutoremove,
		auditAction:    "autoremove.start",
		startFailure:   "Failed to start autoremove",
		createFailure:  "Failed to create autoremove job",
		successMessage: "Autoremove started",
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

func (l *serverActionLifecycle) StartSudoersEnable(name, actor, clientIP, sudoPassword string) serverActionLifecycleResult {
	return l.startAction(name, actor, clientIP, sudoPassword, serverActionStartSpec{
		status:         "sudoers",
		jobKind:        jobKindSudoersEnable,
		auditAction:    "sudoers.enable.start",
		startFailure:   "Failed to start sudoers setup",
		createFailure:  "Failed to create sudoers job",
		successMessage: "Sudoers setup started",
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
		status:         "sudoers",
		jobKind:        jobKindSudoersDisable,
		auditAction:    "sudoers.disable.start",
		startFailure:   "Failed to start sudoers disable",
		createFailure:  "Failed to create sudoers disable job",
		successMessage: "Sudoers disable started",
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
	server, err := l.serverState.BeginAction(name, spec.status)
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
		if errors.Is(err, errMaintenanceModeActive) {
			return serverActionLifecycleResult{maintenanceBlocked: true}
		}
		retryMeta["error"] = err.Error()
		l.recordAudit(spec.auditAction, name, "failure", "Failed to create job", retryMeta)
		return jsonResult(http.StatusInternalServerError, spec.createFailure)
	}
	l.startJobRunner(l.currentJobManager, job.ID, func() {
		spec.runWithJob(l.updateService, server, actor, clientIP, policy, job.ID, sudoPassword)
	})
	l.recordAudit(spec.auditAction, name, "started", spec.successMessage, retryMeta)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": spec.successMessage, "job_id": job.ID},
	}
}

func (l *serverActionLifecycle) ApproveAll(name string) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{}, serverActionApprovalSpec{
		scope:             "all",
		jobSummary:        "All pending updates approved",
		successMessage:    "All pending updates approved",
		rollbackLogPrefix: "update approve",
		successAuditMeta: func(*ServerStatus) map[string]any {
			return map[string]any{"scope": "all"}
		},
	})
}

func (l *serverActionLifecycle) ApproveSecurity(name string) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{}, serverActionApprovalSpec{
		scope:             "security",
		jobSummary:        "Security updates approved",
		successMessage:    "Security updates approved",
		rollbackLogPrefix: "security approve",
		notFoundMeta:      map[string]any{"scope": "security"},
		successAuditMeta: func(*ServerStatus) map[string]any {
			return map[string]any{"scope": "security"}
		},
	})
}

func (l *serverActionLifecycle) ApproveKeptBackSecurity(name string, confirmRemovals bool) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{confirmRemovals: confirmRemovals}, serverActionApprovalSpec{
		scope:             "security_kept_back",
		jobSummary:        "Kept-back security updates approved",
		successMessage:    "Kept-back security updates approved",
		rollbackLogPrefix: "kept-back security approve",
		notFoundMeta:      map[string]any{"scope": "security_kept_back"},
		precheck: func(status *ServerStatus, opts serverActionApprovalOptions) *serverActionLifecycleResult {
			packages := keptBackSecurityPackagesFromPendingUpdates(status.PendingUpdates)
			if len(packages) == 0 {
				l.recordAuditWithMeta("update.approve", status.Name, "ignored", "No kept-back security updates pending", map[string]any{"scope": "security_kept_back"})
				result := jsonResult(http.StatusConflict, "No kept-back security updates pending")
				return &result
			}
			if !status.UpgradePlan.KeptBackSecurityPlanAvailable {
				l.recordAuditWithMeta("update.approve", status.Name, "blocked", "Kept-back security upgrade requires a fresh targeted simulation", map[string]any{"scope": "security_kept_back"})
				result := jsonResult(http.StatusConflict, "Kept-back security upgrade requires a fresh package scan")
				return &result
			}
			if len(status.UpgradePlan.KeptBackSecurityRemovedPackages) > 0 && !opts.confirmRemovals {
				l.recordAuditWithMeta("update.approve", status.Name, "blocked", "Kept-back security upgrade requires package removal confirmation", map[string]any{
					"scope":            "security_kept_back",
					"removed_packages": status.UpgradePlan.KeptBackSecurityRemovedPackages,
				})
				result := serverActionLifecycleResult{
					statusCode: http.StatusConflict,
					body: map[string]any{
						"error":            "Kept-back security upgrade may remove packages; confirmation required",
						"removed_packages": status.UpgradePlan.KeptBackSecurityRemovedPackages,
					},
				}
				return &result
			}
			return nil
		},
		approvalOptions: func(opts serverActionApprovalOptions) serverpkg.ApprovalOptions {
			return serverpkg.ApprovalOptions{ConfirmRemovals: opts.confirmRemovals}
		},
		successAuditMeta: func(status *ServerStatus) map[string]any {
			return map[string]any{
				"scope":             "security_kept_back",
				"approved_packages": keptBackSecurityPackagesFromPendingUpdates(status.PendingUpdates),
				"new_packages":      status.UpgradePlan.KeptBackSecurityNewPackages,
				"removed_packages":  status.UpgradePlan.KeptBackSecurityRemovedPackages,
			}
		},
	})
}

func (l *serverActionLifecycle) ApproveFullUpgrade(name string, confirmRemovals bool) serverActionLifecycleResult {
	return l.approve(name, serverActionApprovalOptions{confirmRemovals: confirmRemovals}, serverActionApprovalSpec{
		scope:             "full_upgrade",
		jobSummary:        "Full upgrade approved",
		successMessage:    "Full upgrade approved",
		rollbackLogPrefix: "full approve",
		notFoundMeta:      map[string]any{"scope": "full_upgrade"},
		precheck: func(status *ServerStatus, opts serverActionApprovalOptions) *serverActionLifecycleResult {
			if !status.UpgradePlan.FullUpgradePlanAvailable {
				l.recordAuditWithMeta("update.approve", status.Name, "blocked", "Full upgrade requires a fresh full-upgrade simulation", map[string]any{"scope": "full_upgrade"})
				result := jsonResult(http.StatusConflict, "Full upgrade requires a fresh package scan")
				return &result
			}
			if len(status.UpgradePlan.FullUpgradeRemovedPackages) > 0 && !opts.confirmRemovals {
				l.recordAuditWithMeta("update.approve", status.Name, "blocked", "Full upgrade requires package removal confirmation", map[string]any{
					"scope":            "full_upgrade",
					"removed_packages": status.UpgradePlan.FullUpgradeRemovedPackages,
				})
				result := serverActionLifecycleResult{
					statusCode: http.StatusConflict,
					body: map[string]any{
						"error":            "Full upgrade would remove packages; confirmation required",
						"removed_packages": status.UpgradePlan.FullUpgradeRemovedPackages,
					},
				}
				return &result
			}
			return nil
		},
		approvalOptions: func(opts serverActionApprovalOptions) serverpkg.ApprovalOptions {
			return serverpkg.ApprovalOptions{ConfirmRemovals: opts.confirmRemovals}
		},
		successAuditMeta: func(status *ServerStatus) map[string]any {
			return map[string]any{
				"scope":            "full_upgrade",
				"removed_packages": status.UpgradePlan.FullUpgradeRemovedPackages,
			}
		},
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
	if spec.precheck != nil {
		if result := spec.precheck(preApproveStatus, opts); result != nil {
			return *result
		}
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
	if err := jm.UpdateJobWithoutRuntimeSync(job.ID, JobUpdate{
		Status:   &status,
		Phase:    &phase,
		Summary:  &spec.jobSummary,
		LogsText: &logs,
	}); err != nil {
		l.recordAuditWithMeta("update.approve", name, "failure", "Failed to persist approval", map[string]any{"scope": spec.scope, "error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist approval")
	}
	approvalOptions := serverpkg.ApprovalOptions{}
	if spec.approvalOptions != nil {
		approvalOptions = spec.approvalOptions(opts)
	}
	exists, approved := l.updateService.ApprovePendingUpdateWithOptions(name, spec.scope, approvalOptions)
	if !exists || !approved {
		rollbackStatus := jobStatusWaitingApproval
		rollbackPhase := jobPhaseApprovalWait
		rollbackSummary := "Waiting for approval"
		if rollbackErr := jm.UpdateJobWithoutRuntimeSync(job.ID, JobUpdate{
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
	meta := map[string]any{"scope": spec.scope}
	if spec.successAuditMeta != nil {
		meta = spec.successAuditMeta(preApproveStatus)
	}
	l.recordAuditWithMeta("update.approve", name, "success", spec.successMessage, meta)
	return serverActionLifecycleResult{
		statusCode: http.StatusOK,
		body:       map[string]any{"message": spec.successMessage},
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
	finishedAt := l.jobTimestamp()
	if err := jm.UpdateJobWithoutRuntimeSync(job.ID, JobUpdate{
		Status:     &status,
		Phase:      &phase,
		Summary:    &summary,
		LogsText:   &logsBeforeCancel,
		FinishedAt: &finishedAt,
	}); err != nil {
		l.recordAuditWithMeta("update.cancel", name, "failure", "Failed to persist cancelled update", map[string]any{"error": err.Error()})
		return jsonResult(http.StatusInternalServerError, "Failed to persist cancelled update")
	}
	exists, cancelled := l.updateService.CancelPendingUpdate(name)
	if !exists || !cancelled {
		rollbackStatus := jobStatusWaitingApproval
		rollbackPhase := jobPhaseApprovalWait
		rollbackSummary := "Waiting for approval"
		if rollbackErr := jm.UpdateJobWithoutRuntimeSync(job.ID, JobUpdate{
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

func (l *serverActionLifecycle) jobTimestamp() string {
	if l.jobTimestampNow == nil {
		return jobTimestampNow()
	}
	return l.jobTimestampNow()
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
