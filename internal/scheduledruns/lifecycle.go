package scheduledruns

import (
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"debian-updater/internal/audit"
	"debian-updater/internal/jobs"
	"debian-updater/internal/maintenance"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

type Deps struct {
	AuditService                    *audit.Service
	CurrentJobManager               func() *jobs.Manager
	JobTimestampNow                 func() string
	LoadRetryPolicy                 func() updates.RetryPolicy
	MaintenanceCoordinator          *maintenance.Coordinator
	PolicyRepository                RunRepository
	ServerState                     *servers.State
	StartJobRunner                  func(string, func())
	StartScheduledRunReconciliation func(int64, string)
	UpdateService                   *updates.Service
}

// RunRepository is the complete persistence surface owned by the Scheduled Run
// Lifecycle. Keeping this contract local prevents unrelated policy persistence
// changes from widening the lifecycle module.
type RunRepository interface {
	CreateRun(policies.Run) (policies.Run, bool, error)
	GetRun(int64) (policies.Run, error)
	UpdateRun(int64, policies.RunUpdate) error
}

type Lifecycle struct {
	deps Deps
}

func New(deps Deps) *Lifecycle {
	return &Lifecycle{deps: deps}
}

func (l *Lifecycle) HandleScheduledRun(req policies.ScheduledRunRequest) policies.ScheduledRunResult {
	if l == nil {
		return policies.ScheduledRunResult{Err: errors.New("scheduled run lifecycle is unavailable")}
	}
	if strings.TrimSpace(req.Outcome) != "" {
		return l.recordSkippedCandidate(req.Policy, req.Server, req.ScheduledForUTC, req.Outcome)
	}
	run, inserted, err := l.deps.PolicyRepository.CreateRun(policies.Run{
		PolicyID:        req.Policy.ID,
		PolicyName:      req.Policy.Name,
		ServerName:      req.Server.Name,
		ScheduledForUTC: req.ScheduledForUTC,
		ExecutionMode:   req.Policy.ExecutionMode,
		PackageScope:    req.Policy.PackageScope,
		UpgradeMode:     req.Policy.UpgradeMode,
		Status:          policies.RunQueued,
		Summary:         "Scheduled run queued",
		ResultJSON:      "{}",
	})
	if err != nil {
		return policies.ScheduledRunResult{Handled: false, Err: err}
	}
	result := policies.ScheduledRunResult{
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
		l.ExecuteRun(run, req.Policy, req.Server)
	}
	return result
}

func (l *Lifecycle) ExecuteRun(run policies.Run, policy policies.Policy, server servers.Server) {
	if l == nil {
		return
	}
	if l.deps.MaintenanceCoordinator != nil {
		lease, decision := l.deps.MaintenanceCoordinator.TryShared(maintenance.WorkScheduled)
		if !decision.Allowed {
			l.markMaintenanceSkipped(run, policy, server, "Maintenance mode active; scheduled run skipped")
			return
		}
		defer lease.Close()
	}
	l.executeAdmitted(run, policy, server)
}

func (l *Lifecycle) executeAdmitted(run policies.Run, policy policies.Policy, server servers.Server) {
	switch policy.ExecutionMode {
	case policies.ExecutionScanOnly:
		l.runScan(run, policy, server)
	default:
		l.runUpdate(run, policy, server)
	}
}

func (l *Lifecycle) buildScheduledJobMeta(policy policies.Policy, scheduledForUTC string) updates.ScheduledJobMeta {
	return updates.BuildScheduledJobMeta(policy, scheduledForUTC)
}

func (l *Lifecycle) recordSkippedCandidate(policy policies.Policy, server servers.Server, scheduledForUTC, reason string) policies.ScheduledRunResult {
	summary := scheduledRunSkippedSummary(reason)
	finishedAt := l.deps.JobTimestampNow()
	run, inserted, err := l.deps.PolicyRepository.CreateRun(policies.Run{
		PolicyID:        policy.ID,
		PolicyName:      policy.Name,
		ServerName:      server.Name,
		ScheduledForUTC: scheduledForUTC,
		ExecutionMode:   policy.ExecutionMode,
		PackageScope:    policy.PackageScope,
		UpgradeMode:     policy.UpgradeMode,
		Status:          policies.RunSkipped,
		Reason:          reason,
		Summary:         summary,
		ResultJSON:      "{}",
		FinishedAt:      finishedAt,
	})
	if err != nil {
		return policies.ScheduledRunResult{Handled: false, Err: err}
	}
	result := policies.ScheduledRunResult{
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
	case policies.RunReasonMaintenance:
		return "Maintenance mode active; scheduled run skipped"
	case policies.RunReasonBlackout:
		return "Scheduled run skipped due to blackout window"
	case policies.RunReasonSuperseded:
		return "Scheduled run superseded by higher-priority policy"
	default:
		return "Scheduled run skipped"
	}
}

func (l *Lifecycle) markMaintenanceSkipped(run policies.Run, policy policies.Policy, server servers.Server, summary string) {
	status := policies.RunSkipped
	reason := policies.RunReasonMaintenance
	finishedAt := l.deps.JobTimestampNow()
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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

func (l *Lifecycle) runUpdate(run policies.Run, policy policies.Policy, server servers.Server) {
	preStartStatus := l.deps.ServerState.CurrentStatusSnapshot(server.Name)
	serverForRun, err := l.deps.ServerState.BeginAction(server.Name, "updating")
	if err != nil {
		status := policies.RunFailed
		reason := policies.RunReasonMissing
		summary := "Server unavailable for scheduled update"
		if errors.Is(err, servers.ErrActionInProgress) {
			status = policies.RunSkipped
			reason = policies.RunReasonBusy
			summary = "Server busy; scheduled update skipped"
		}
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
	job, err := l.createServerActionJob(l.deps.CurrentJobManager(), l.deps.ServerState, jobs.KindUpdate, server.Name, "system", "", retryPolicy, meta)
	if err != nil {
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
		status := policies.RunFailed
		reason := policies.RunReasonPersistence
		summary := "Failed to create scheduled update job"
		auditAction := "schedule.run.failed"
		auditStatus := "failure"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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

	runningStatus := policies.RunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled update started"
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		l.deps.UpdateService.RunUpdateJob(updates.UpdateRunRequest{
			Server:   serverForRun,
			Actor:    "system",
			ClientIP: "",
			Policy:   retryPolicy,
			JobID:    job.ID,
		})
	})
	l.deps.StartScheduledRunReconciliation(run.ID, job.ID)
}

func (l *Lifecycle) runScan(run policies.Run, policy policies.Policy, server servers.Server) {
	preStartStatus := l.deps.ServerState.CurrentStatusSnapshot(server.Name)
	serverForRun, err := l.deps.ServerState.BeginAction(server.Name, "updating")
	if err != nil {
		status := policies.RunFailed
		reason := policies.RunReasonMissing
		summary := "Server unavailable for scheduled scan"
		if errors.Is(err, servers.ErrActionInProgress) {
			status = policies.RunSkipped
			reason = policies.RunReasonBusy
			summary = "Server busy; scheduled scan skipped"
		}
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		status := policies.RunFailed
		reason := policies.RunReasonPersistence
		summary := "Job manager unavailable"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
	job, err := jm.CreateJob(jobs.CreateParams{
		Kind:            jobs.KindScheduledScan,
		ServerName:      server.Name,
		Actor:           "system",
		Status:          jobs.StatusQueued,
		RetryPolicyJSON: jobs.MarshalJSON(retryPolicy),
		MetaJSON:        jobs.MarshalJSON(meta),
		Summary:         "Scheduled scan queued",
	})
	if err != nil {
		status := policies.RunFailed
		reason := policies.RunReasonPersistence
		summary := "Failed to create scheduled scan job"
		auditAction := "schedule.run.failed"
		auditStatus := "failure"
		finishedAt := l.deps.JobTimestampNow()
		_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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

	runningStatus := policies.RunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled scan started"
	_ = l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		l.deps.UpdateService.RunScheduledScanJob(updates.ScheduledScanRunRequest{
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

func (l *Lifecycle) ReconcileJob(runID int64, job jobs.Record) {
	previous, previousErr := l.deps.PolicyRepository.GetRun(runID)
	status := policies.RunRunning
	switch job.Status {
	case jobs.StatusQueued:
		status = policies.RunQueued
	case jobs.StatusRunning:
		status = policies.RunRunning
	case jobs.StatusWaitingApproval:
		status = policies.RunWaitingApproval
	case jobs.StatusSucceeded:
		status = policies.RunSucceeded
	case jobs.StatusFailed:
		status = policies.RunFailed
	case jobs.StatusCancelled:
		status = policies.RunCancelled
	case jobs.StatusInterrupted:
		status = policies.RunInterrupted
	}
	update := policies.RunUpdate{
		Status:    &status,
		Summary:   stringPointer(strings.TrimSpace(job.Summary)),
		JobID:     &job.ID,
		StartedAt: &job.StartedAt,
	}
	var meta updates.ScheduledJobMeta
	hasMeta := false
	if job.FinishedAt != "" {
		update.FinishedAt = &job.FinishedAt
	}
	if strings.TrimSpace(job.MetaJSON) != "" {
		if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err == nil {
			hasMeta = true
			if meta.Discovery != nil {
				resultJSON := jobs.MarshalJSON(meta.Discovery)
				update.ResultJSON = &resultJSON
			}
		}
	}
	if status == policies.RunFailed || status == policies.RunCancelled || status == policies.RunInterrupted {
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

func (l *Lifecycle) recordScheduledScanTerminalAudit(job jobs.Record, meta updates.ScheduledJobMeta) {
	if job.Kind != jobs.KindScheduledScan || meta.Trigger != "scheduled" {
		return
	}
	summary := strings.TrimSpace(job.Summary)
	switch job.Status {
	case jobs.StatusSucceeded:
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
	case jobs.StatusFailed:
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

func (l *Lifecycle) WatchJob(runID int64, jobID string) {
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
		l.ReconcileJob(runID, job)
		switch job.Status {
		case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusInterrupted:
			return
		}
		<-ticker.C
	}
}

func (l *Lifecycle) LoadJobBehavior(jobID string) updates.ScheduledJobBehavior {
	behavior := updates.ScheduledJobBehavior{ApprovalTimeout: 30 * time.Minute}
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
	var meta updates.ScheduledJobMeta
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
		switch updates.NormalizeApprovalScope(meta.AutoApproveScope) {
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

func (l *Lifecycle) UpdateJobDiscovery(jobID string, discovery updates.PackageDiscoveryOutcome) {
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
	var meta updates.ScheduledJobMeta
	if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
		return
	}
	if meta.Trigger != "scheduled" {
		return
	}
	cloned := discovery.Clone()
	meta.Discovery = &cloned
	metaJSON := jobs.MarshalJSON(meta)
	if err := jm.Transition(jobID, jobs.Intent{MetaJSON: &metaJSON}); err != nil {
		log.Printf("failed to persist scheduled discovery meta for job %q: %v", jobID, err)
	}
}

func (l *Lifecycle) createServerActionJob(jm *jobs.Manager, state *servers.State, kind, serverName, actor, clientIP string, policy updates.RetryPolicy, meta any) (jobs.Record, error) {
	if jm == nil {
		return jobs.Record{}, errors.New("job manager is not initialized")
	}
	initialLogs := ""
	if state != nil {
		if snapshot := state.CurrentStatusSnapshot(serverName); snapshot != nil {
			initialLogs = snapshot.Logs
		}
	}
	return jm.CreateJob(jobs.CreateParams{
		Kind:            kind,
		ServerName:      serverName,
		Actor:           actor,
		ClientIP:        clientIP,
		Status:          jobs.StatusQueued,
		LogsText:        initialLogs,
		RetryPolicyJSON: jobs.MarshalJSON(policy),
		MetaJSON:        jobs.MarshalJSON(meta),
	})
}

func stringPointer(value string) *string { return &value }
