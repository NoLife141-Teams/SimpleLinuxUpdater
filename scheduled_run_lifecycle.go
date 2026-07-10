package main

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	updatespkg "debian-updater/internal/updates"
)

type scheduledRunLifecycle struct {
	deps AppDeps
}

func newScheduledRunLifecycle(deps AppDeps) *scheduledRunLifecycle {
	return &scheduledRunLifecycle{deps: deps.withDefaults()}
}

func (l *scheduledRunLifecycle) HandleScheduledRun(req policypkg.ScheduledRunRequest) policypkg.ScheduledRunResult {
	if l == nil {
		l = newScheduledRunLifecycle(AppDeps{})
	}
	if strings.TrimSpace(req.Outcome) != "" {
		return l.recordSkippedCandidate(req.Policy, req.Server, req.ScheduledForUTC, req.Outcome)
	}
	run, inserted, err := l.deps.PolicyRepository.CreateRun(UpdatePolicyRun{
		PolicyID:        req.Policy.ID,
		PolicyName:      req.Policy.Name,
		ServerName:      req.Server.Name,
		ScheduledForUTC: req.ScheduledForUTC,
		ExecutionMode:   req.Policy.ExecutionMode,
		PackageScope:    req.Policy.PackageScope,
		UpgradeMode:     req.Policy.UpgradeMode,
		Status:          updatePolicyRunQueued,
		Summary:         "Scheduled run queued",
		ResultJSON:      "{}",
	})
	if err != nil {
		return policypkg.ScheduledRunResult{Handled: false, Err: err}
	}
	result := policypkg.ScheduledRunResult{
		Handled:  true,
		Inserted: inserted,
		RunID:    run.ID,
		Status:   run.Status,
	}
	if !inserted {
		return result
	}
	if req.Admitted {
		l.executeAdmitted(run, req.Policy, req.Server)
	} else {
		l.Execute(run, req.Policy, req.Server)
	}
	return result
}

func (l *scheduledRunLifecycle) Execute(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	if l == nil {
		l = newScheduledRunLifecycle(AppDeps{})
	}
	if l.deps.MaintenanceCoordinator != nil {
		lease, decision := l.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
		if !decision.Allowed {
			l.markMaintenanceSkipped(run, policy, server, "Maintenance mode active; scheduled run skipped")
			return
		}
		defer lease.Close()
	}
	l.executeAdmitted(run, policy, server)
}

func (l *scheduledRunLifecycle) executeAdmitted(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	switch policy.ExecutionMode {
	case updatePolicyExecutionScanOnly:
		l.runScan(run, policy, server)
	default:
		l.runUpdate(run, policy, server)
	}
}

func (l *scheduledRunLifecycle) buildScheduledJobMeta(policy UpdatePolicy, scheduledForUTC string) scheduledJobMeta {
	return updatespkg.BuildScheduledJobMeta(policy, scheduledForUTC)
}

func (l *scheduledRunLifecycle) recordSkippedCandidate(policy UpdatePolicy, server Server, scheduledForUTC, reason string) policypkg.ScheduledRunResult {
	summary := scheduledRunSkippedSummary(reason)
	finishedAt := l.deps.JobTimestampNow()
	run, inserted, err := l.deps.PolicyRepository.CreateRun(UpdatePolicyRun{
		PolicyID:        policy.ID,
		PolicyName:      policy.Name,
		ServerName:      server.Name,
		ScheduledForUTC: scheduledForUTC,
		ExecutionMode:   policy.ExecutionMode,
		PackageScope:    policy.PackageScope,
		UpgradeMode:     policy.UpgradeMode,
		Status:          updatePolicyRunSkipped,
		Reason:          reason,
		Summary:         summary,
		ResultJSON:      "{}",
		FinishedAt:      finishedAt,
	})
	if err != nil {
		return policypkg.ScheduledRunResult{Handled: false, Err: err}
	}
	result := policypkg.ScheduledRunResult{
		Handled:  true,
		Inserted: inserted,
		RunID:    run.ID,
		Status:   run.Status,
	}
	if !inserted {
		return result
	}
	_ = l.deps.AuditService.Record("system", "", "schedule.run.skipped", "server", server.Name, "ignored", summary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"reason":            reason,
		"scheduled_for_utc": scheduledForUTC,
		"run_id":            run.ID,
	})
	return result
}

func scheduledRunSkippedSummary(reason string) string {
	switch reason {
	case updatePolicyRunReasonMaintenance:
		return "Maintenance mode active; scheduled run skipped"
	case updatePolicyRunReasonBlackout:
		return "Scheduled run skipped due to blackout window"
	case updatePolicyRunReasonSuperseded:
		return "Scheduled run superseded by higher-priority policy"
	default:
		return "Scheduled run skipped"
	}
}

func (l *scheduledRunLifecycle) markMaintenanceSkipped(run UpdatePolicyRun, policy UpdatePolicy, server Server, summary string) {
	status := updatePolicyRunSkipped
	reason := updatePolicyRunReasonMaintenance
	finishedAt := l.deps.JobTimestampNow()
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
		Status:     &status,
		Reason:     &reason,
		Summary:    &summary,
		FinishedAt: &finishedAt,
	})
	_ = l.deps.AuditService.Record("system", "", "schedule.run.skipped", "server", server.Name, "skipped", summary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"scheduled_for_utc": run.ScheduledForUTC,
	})
}

func (l *scheduledRunLifecycle) runUpdate(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	preStartStatus := l.deps.ServerState.CurrentStatusSnapshot(server.Name)
	serverForRun, err := l.deps.ServerState.BeginAction(server.Name, "updating")
	if err != nil {
		status := updatePolicyRunFailed
		reason := updatePolicyRunReasonMissing
		summary := "Server unavailable for scheduled update"
		if errors.Is(err, errActionInProgress) {
			status = updatePolicyRunSkipped
			reason = updatePolicyRunReasonBusy
			summary = "Server busy; scheduled update skipped"
		}
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
			Status:     &status,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		_ = l.deps.AuditService.Record("system", "", "schedule.run."+status, "server", server.Name, status, summary, map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"scheduled_for_utc": run.ScheduledForUTC,
		})
		return
	}

	retryPolicy := l.deps.LoadRetryPolicy()
	meta := l.buildScheduledJobMeta(policy, run.ScheduledForUTC)
	job, err := createServerActionJobWithMetaAndState(l.deps.CurrentJobManager(), l.deps.ServerState, jobKindUpdate, server.Name, "system", "", retryPolicy, meta)
	if err != nil {
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
		status := updatePolicyRunFailed
		reason := updatePolicyRunReasonPersistence
		summary := "Failed to create scheduled update job"
		auditAction := "schedule.run.failed"
		auditStatus := "failure"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
			Status:     &status,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		_ = l.deps.AuditService.Record("system", "", auditAction, "server", server.Name, auditStatus, summary, map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"scheduled_for_utc": run.ScheduledForUTC,
			"error":             err.Error(),
		})
		return
	}

	runningStatus := updatePolicyRunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled update started"
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
		Status:    &runningStatus,
		Summary:   &summary,
		JobID:     &job.ID,
		StartedAt: &startedAt,
	})
	_ = l.deps.AuditService.Record("system", "", "schedule.run.started", "server", server.Name, "started", summary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"scheduled_for_utc": run.ScheduledForUTC,
		"job_id":            job.ID,
		"execution_mode":    policy.ExecutionMode,
		"package_scope":     policy.PackageScope,
		"upgrade_mode":      policy.UpgradeMode,
	})
	l.deps.StartJobRunner(job.ID, func() {
		l.deps.UpdateService.RunUpdateJob(UpdateRunRequest{
			Server:   serverForRun,
			Actor:    "system",
			ClientIP: "",
			Policy:   retryPolicy,
			JobID:    job.ID,
		})
	})
	l.deps.StartScheduledRunReconciliation(run.ID, job.ID)
}

func (l *scheduledRunLifecycle) runScan(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	preStartStatus := l.deps.ServerState.CurrentStatusSnapshot(server.Name)
	serverForRun, err := l.deps.ServerState.BeginAction(server.Name, "updating")
	if err != nil {
		status := updatePolicyRunFailed
		reason := updatePolicyRunReasonMissing
		summary := "Server unavailable for scheduled scan"
		if errors.Is(err, errActionInProgress) {
			status = updatePolicyRunSkipped
			reason = updatePolicyRunReasonBusy
			summary = "Server busy; scheduled scan skipped"
		}
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
			Status:     &status,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		_ = l.deps.AuditService.Record("system", "", "schedule.run."+status, "server", server.Name, status, summary, map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"scheduled_for_utc": run.ScheduledForUTC,
		})
		return
	}

	retryPolicy := l.deps.LoadRetryPolicy()
	meta := l.buildScheduledJobMeta(policy, run.ScheduledForUTC)
	jm := l.deps.CurrentJobManager()
	if jm == nil {
		status := updatePolicyRunFailed
		reason := updatePolicyRunReasonPersistence
		summary := "Job manager unavailable"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
			Status:     &status,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		_ = l.deps.AuditService.Record("system", "", "schedule.run.failed", "server", server.Name, "failure", summary, map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"scheduled_for_utc": run.ScheduledForUTC,
			"error":             "job manager unavailable",
		})
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
		return
	}
	job, err := jm.CreateJob(JobCreateParams{
		Kind:            jobKindScheduledScan,
		ServerName:      server.Name,
		Actor:           "system",
		Status:          jobStatusQueued,
		RetryPolicyJSON: marshalJobJSON(retryPolicy),
		MetaJSON:        marshalJobJSON(meta),
		Summary:         "Scheduled scan queued",
	})
	if err != nil {
		status := updatePolicyRunFailed
		reason := updatePolicyRunReasonPersistence
		summary := "Failed to create scheduled scan job"
		auditAction := "schedule.run.failed"
		auditStatus := "failure"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
			Status:     &status,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		_ = l.deps.AuditService.Record("system", "", auditAction, "server", server.Name, auditStatus, summary, map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"scheduled_for_utc": run.ScheduledForUTC,
			"error":             err.Error(),
		})
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
		return
	}

	runningStatus := updatePolicyRunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled scan started"
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, updatePolicyRunUpdate{
		Status:    &runningStatus,
		Summary:   &summary,
		JobID:     &job.ID,
		StartedAt: &startedAt,
	})
	_ = l.deps.AuditService.Record("system", "", "schedule.run.started", "server", server.Name, "started", summary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"scheduled_for_utc": run.ScheduledForUTC,
		"job_id":            job.ID,
		"execution_mode":    policy.ExecutionMode,
		"package_scope":     policy.PackageScope,
		"upgrade_mode":      policy.UpgradeMode,
	})

	l.deps.StartJobRunner(job.ID, func() {
		defer l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
		l.deps.UpdateService.RunScheduledScanJob(ScheduledScanRunRequest{
			JobID:           job.ID,
			RunID:           run.ID,
			ScheduledForUTC: run.ScheduledForUTC,
			Server:          serverForRun,
			Policy:          policy,
			RetryPolicy:     retryPolicy,
		})
	})
	l.deps.StartScheduledRunReconciliation(run.ID, job.ID)
}

func (l *scheduledRunLifecycle) updatePolicyRunFromJobRecord(runID int64, job JobRecord) {
	previous, previousErr := l.deps.PolicyRepository.GetRun(runID)
	status := updatePolicyRunRunning
	switch job.Status {
	case jobStatusQueued:
		status = updatePolicyRunQueued
	case jobStatusRunning:
		status = updatePolicyRunRunning
	case jobStatusWaitingApproval:
		status = updatePolicyRunWaitingApproval
	case jobStatusSucceeded:
		status = updatePolicyRunSucceeded
	case jobStatusFailed:
		status = updatePolicyRunFailed
	case jobStatusCancelled:
		status = updatePolicyRunCancelled
	case jobStatusInterrupted:
		status = updatePolicyRunInterrupted
	}
	update := updatePolicyRunUpdate{
		Status:    &status,
		Summary:   stringPtr(strings.TrimSpace(job.Summary)),
		JobID:     &job.ID,
		StartedAt: &job.StartedAt,
	}
	var meta scheduledJobMeta
	hasMeta := false
	if job.FinishedAt != "" {
		update.FinishedAt = &job.FinishedAt
	}
	if strings.TrimSpace(job.MetaJSON) != "" {
		if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err == nil {
			hasMeta = true
			if meta.Discovery != nil {
				resultJSON := marshalJobJSON(meta.Discovery)
				update.ResultJSON = &resultJSON
			}
		}
	}
	if status == updatePolicyRunFailed || status == updatePolicyRunCancelled || status == updatePolicyRunInterrupted {
		reason := status
		update.Reason = &reason
	}
	_ = l.deps.PolicyRepository.UpdateRun(runID, update)
	if previousErr == nil && previous.Status == status && previous.FinishedAt != "" {
		return
	}
	if hasMeta {
		l.recordScheduledScanTerminalAudit(job, meta)
	}
}

func (l *scheduledRunLifecycle) recordScheduledScanTerminalAudit(job JobRecord, meta scheduledJobMeta) {
	if job.Kind != jobKindScheduledScan || meta.Trigger != "scheduled" {
		return
	}
	summary := strings.TrimSpace(job.Summary)
	switch job.Status {
	case jobStatusSucceeded:
		if summary == "" {
			summary = "Scheduled scan completed"
		}
		auditMeta := map[string]any{
			"policy_id":   meta.PolicyID,
			"policy_name": meta.PolicyName,
		}
		if meta.Discovery != nil {
			auditMeta["pending_package_count"] = meta.Discovery.PendingPackageCount
			auditMeta["security_package_count"] = meta.Discovery.SecurityPackageCount
		}
		_ = l.deps.AuditService.Record("system", "", "schedule.run.completed", "server", job.ServerName, "success", summary, auditMeta)
	case jobStatusFailed:
		if summary == "" {
			summary = "Scheduled scan failed"
		}
		auditMeta := map[string]any{
			"policy_id":      meta.PolicyID,
			"policy_name":    meta.PolicyName,
			"execution_mode": meta.ExecutionMode,
			"package_scope":  meta.PackageScope,
		}
		if strings.TrimSpace(meta.Error) != "" {
			auditMeta["error"] = meta.Error
		}
		_ = l.deps.AuditService.Record("system", "", "schedule.run.failed", "server", job.ServerName, "failure", summary, auditMeta)
	}
}

func (l *scheduledRunLifecycle) watchUpdatePolicyRunForJob(runID int64, jobID string) {
	startTrackedActionRunner(func() {
		ticker := time.NewTicker(1 * time.Second)
		defer ticker.Stop()
		for {
			jm := l.deps.CurrentJobManager()
			if jm == nil {
				return
			}
			job, err := jm.GetJob(jobID)
			if err != nil {
				log.Printf("failed to read scheduled job %q for run %d: %v", jobID, runID, err)
				return
			}
			l.updatePolicyRunFromJobRecord(runID, job)
			switch job.Status {
			case jobStatusSucceeded, jobStatusFailed, jobStatusCancelled, jobStatusInterrupted:
				return
			}
			<-ticker.C
		}
	})
}

func (l *scheduledRunLifecycle) loadScheduledJobBehavior(jobID string) scheduledJobBehavior {
	behavior := scheduledJobBehavior{ApprovalTimeout: 30 * time.Minute}
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return behavior
	}
	jm := l.deps.CurrentJobManager()
	if jm == nil {
		return behavior
	}
	job, err := jm.GetJob(jobID)
	if err != nil || strings.TrimSpace(job.MetaJSON) == "" {
		return behavior
	}
	var meta scheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		return behavior
	}
	if meta.Trigger != "scheduled" {
		return behavior
	}
	if meta.ApprovalTimeoutMinutes > 0 {
		behavior.ApprovalTimeout = time.Duration(meta.ApprovalTimeoutMinutes) * time.Minute
	}
	if strings.TrimSpace(meta.AutoApproveScope) != "" {
		switch normalizeApprovalScope(meta.AutoApproveScope) {
		case "security":
			behavior.AutoApproveScope = "security"
		case "full_upgrade":
			behavior.AutoApproveScope = "full_upgrade"
		case "all":
			behavior.AutoApproveScope = "all"
		}
	}
	return behavior
}

func (l *scheduledRunLifecycle) updateScheduledJobDiscoveryMeta(jobID string, discovery PackageDiscoveryOutcome) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return
	}
	jm := l.deps.CurrentJobManager()
	if jm == nil {
		return
	}
	job, err := jm.GetJob(jobID)
	if err != nil || strings.TrimSpace(job.MetaJSON) == "" {
		return
	}
	var meta scheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		return
	}
	if meta.Trigger != "scheduled" {
		return
	}
	cloned := discovery.Clone()
	meta.Discovery = &cloned
	metaJSON := marshalJobJSON(meta)
	if err := jm.UpdateJobWithoutRuntimeSync(jobID, JobUpdate{MetaJSON: &metaJSON}); err != nil {
		log.Printf("failed to persist scheduled discovery meta for job %q: %v", jobID, err)
	}
}
