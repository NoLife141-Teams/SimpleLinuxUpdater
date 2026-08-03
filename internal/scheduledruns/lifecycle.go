package scheduledruns

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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
	MaintenanceReadiness            func(servers.Server) servers.MaintenanceReadiness
	PolicyRepository                RunRepository
	ServerState                     *servers.State
	StartJobRunner                  func(string, func(), ...func())
	StartScheduledRunReconciliation func(int64, string)
	UpdateService                   *updates.Service
	ReconciliationContext           context.Context
	ReconciliationWait              func(context.Context, time.Duration) error
	ReconciliationBackoff           func(int) time.Duration
	ReconciliationAttempts          int
	MissingJobConfirmations         int
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
	if deps.MaintenanceReadiness == nil {
		deps.MaintenanceReadiness = func(servers.Server) servers.MaintenanceReadiness {
			return servers.MaintenanceReadiness{Ready: true, Code: servers.MaintenanceReadinessReady}
		}
	}
	if deps.ReconciliationContext == nil {
		deps.ReconciliationContext = context.Background()
	}
	if deps.JobTimestampNow == nil {
		deps.JobTimestampNow = func() string { return time.Now().UTC().Format(jobs.TimestampLayout) }
	}
	if deps.ReconciliationWait == nil {
		deps.ReconciliationWait = waitForReconciliation
	}
	if deps.ReconciliationBackoff == nil {
		deps.ReconciliationBackoff = reconciliationBackoff
	}
	if deps.ReconciliationAttempts <= 0 {
		deps.ReconciliationAttempts = 5
	}
	if deps.MissingJobConfirmations <= 0 {
		deps.MissingJobConfirmations = 3
	}
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
	var status, jobID string
	var executionErr error
	if req.Admitted {
		status, jobID, executionErr = l.executeAdmitted(run, req.Policy, req.Server)
	} else {
		status, jobID, executionErr = l.executeRun(run, req.Policy, req.Server)
	}
	if strings.TrimSpace(status) != "" {
		result.Status = status
	}
	result.JobID = jobID
	result.Err = executionErr
	return result
}

func (l *Lifecycle) ExecuteRun(run policies.Run, policy policies.Policy, server servers.Server) {
	_, _, _ = l.executeRun(run, policy, server)
}

func (l *Lifecycle) executeRun(run policies.Run, policy policies.Policy, server servers.Server) (string, string, error) {
	if l == nil {
		return "", "", errors.New("scheduled run lifecycle is unavailable")
	}
	if l.deps.MaintenanceCoordinator != nil {
		lease, decision := l.deps.MaintenanceCoordinator.TryShared(maintenance.WorkScheduled)
		if !decision.Allowed {
			l.markMaintenanceSkipped(run, policy, server, "Maintenance mode active; scheduled run skipped")
			return policies.RunSkipped, "", nil
		}
		defer lease.Close()
	}
	return l.executeAdmitted(run, policy, server)
}

func (l *Lifecycle) executeAdmitted(run policies.Run, policy policies.Policy, server servers.Server) (string, string, error) {
	if current := l.deps.ServerState.CurrentStatusSnapshot(server.Name); current != nil {
		active, _ := l.deps.ServerState.ActionStatusInProgress(server.Name)
		if active {
			return l.executeByMode(run, policy, server)
		}
		readiness := l.deps.MaintenanceReadiness(server)
		if !readiness.Ready {
			return l.markReadinessSkipped(run, policy, server, readiness)
		}
	}
	return l.executeByMode(run, policy, server)
}

func (l *Lifecycle) executeByMode(run policies.Run, policy policies.Policy, server servers.Server) (string, string, error) {
	switch policy.ExecutionMode {
	case policies.ExecutionScanOnly:
		return l.runScan(run, policy, server)
	default:
		return l.runUpdate(run, policy, server)
	}
}

func (l *Lifecycle) markReadinessSkipped(run policies.Run, policy policies.Policy, server servers.Server, readiness servers.MaintenanceReadiness) (string, string, error) {
	status := policies.RunSkipped
	reason := policies.RunReasonReadiness
	summary := strings.TrimSpace(readiness.Message)
	if summary == "" {
		summary = "Server connection is not ready; scheduled run skipped"
	}
	finishedAt := l.deps.JobTimestampNow()
	err := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
		Status:     &status,
		Reason:     &reason,
		Summary:    &summary,
		FinishedAt: &finishedAt,
	})
	_ = l.deps.AuditService.Record("system", "", "schedule.run.skipped", "server", server.Name, "ignored", summary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"scheduled_for_utc": run.ScheduledForUTC,
		"reason_code":       readiness.Code,
	})
	return status, "", err
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
	case policies.RunReasonRolloutGate:
		return "Scheduled run blocked because an earlier rollout batch did not succeed"
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

func (l *Lifecycle) runUpdate(run policies.Run, policy policies.Policy, server servers.Server) (string, string, error) {
	preStartStatus := l.deps.ServerState.CurrentStatusSnapshot(server.Name)
	serverForRun, err := l.deps.ServerState.BeginPackageMutation(server.Name, "updating")
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
		updateErr := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		return status, "", updateErr
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
		updateErr := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		return status, "", errors.Join(err, updateErr)
	}

	runningStatus := policies.RunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled update started"
	if err := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
		Status:    &runningStatus,
		Summary:   &summary,
		JobID:     &job.ID,
		StartedAt: &startedAt,
	}); err != nil {
		l.handleRunStartPersistenceFailure(run, policy, server, job, preStartStatus, "update", err)
		return policies.RunFailed, job.ID, fmt.Errorf("persist scheduled update running state: %w", err)
	}
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
	}, func() {
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
	})
	l.deps.StartScheduledRunReconciliation(run.ID, job.ID)
	return policies.RunRunning, job.ID, nil
}

func (l *Lifecycle) runScan(run policies.Run, policy policies.Policy, server servers.Server) (string, string, error) {
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
		updateErr := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		return status, "", updateErr
	}

	retryPolicy := l.deps.LoadRetryPolicy()
	meta := l.buildScheduledJobMeta(policy, run.ScheduledForUTC)
	jm := l.deps.CurrentJobManager()
	if jm == nil {
		status := policies.RunFailed
		reason := policies.RunReasonPersistence
		summary := "Job manager unavailable"
		finishedAt := l.deps.JobTimestampNow()
		updateErr := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		return status, "", errors.Join(errors.New("job manager unavailable"), updateErr)
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
		updateErr := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
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
		return status, "", errors.Join(err, updateErr)
	}

	runningStatus := policies.RunRunning
	startedAt := l.deps.JobTimestampNow()
	summary := "Scheduled scan started"
	if err := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
		Status:    &runningStatus,
		Summary:   &summary,
		JobID:     &job.ID,
		StartedAt: &startedAt,
	}); err != nil {
		l.handleRunStartPersistenceFailure(run, policy, server, job, preStartStatus, "scan", err)
		return policies.RunFailed, job.ID, fmt.Errorf("persist scheduled scan running state: %w", err)
	}
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
	}, func() {
		l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
	})
	l.deps.StartScheduledRunReconciliation(run.ID, job.ID)
	return policies.RunRunning, job.ID, nil
}

func (l *Lifecycle) ReconcileJob(runID int64, job jobs.Record) {
	if err := l.reconcileJob(l.deps.ReconciliationContext, runID, job); err != nil {
		log.Printf("failed to reconcile scheduled run %d from job %q: %v", runID, job.ID, err)
	}
}

func (l *Lifecycle) reconcileJob(ctx context.Context, runID int64, job jobs.Record) error {
	previous, err := l.getRunWithRetry(ctx, runID)
	if err != nil {
		return err
	}
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
	terminal := isTerminalRunStatus(status)
	claimedTerminal := false
	conditionalTransition := false
	if terminal && !isTerminalRunStatus(previous.Status) {
		if repository, ok := l.deps.PolicyRepository.(interface {
			TransitionRunTerminal(int64, policies.RunUpdate) (bool, error)
		}); ok {
			conditionalTransition = true
			claimedTerminal, err = l.transitionRunTerminalWithRetry(ctx, repository, runID, update)
			if err != nil {
				return err
			}
		}
	}
	if !claimedTerminal {
		current := previous
		if conditionalTransition {
			current, err = l.getRunWithRetry(ctx, runID)
			if err != nil {
				return err
			}
		}
		if current.Status != status || strings.TrimSpace(current.JobID) != strings.TrimSpace(job.ID) || (terminal && strings.TrimSpace(job.FinishedAt) != "" && current.FinishedAt != job.FinishedAt) {
			if err := l.updateRunWithRetry(ctx, runID, update); err != nil {
				return err
			}
		}
		claimedTerminal = !conditionalTransition && terminal && !isTerminalRunStatus(previous.Status)
	}
	accepted, err := l.getRunWithRetry(ctx, runID)
	if err != nil {
		return err
	}
	if accepted.Status != status || strings.TrimSpace(accepted.JobID) != strings.TrimSpace(job.ID) {
		return fmt.Errorf("scheduled run %d did not accept job %q reconciliation", runID, job.ID)
	}
	if terminal && strings.TrimSpace(job.FinishedAt) != "" && accepted.FinishedAt != job.FinishedAt {
		return fmt.Errorf("scheduled run %d terminal timestamp does not match job %q", runID, job.ID)
	}
	if claimedTerminal && hasMeta {
		l.recordScheduledScanTerminalAudit(job, meta)
	}
	return nil
}

func (l *Lifecycle) handleRunStartPersistenceFailure(run policies.Run, policy policies.Policy, server servers.Server, job jobs.Record, preStartStatus *servers.ServerStatus, operation string, persistenceErr error) {
	jobStatus := jobs.StatusFailed
	jobSummary := "Scheduled " + operation + " was not started because its run state could not be persisted"
	errorClass := policies.RunReasonPersistence
	if jm := l.deps.CurrentJobManager(); jm != nil {
		if err := jm.Transition(job.ID, jobs.Intent{Status: &jobStatus, Summary: &jobSummary, ErrorClass: &errorClass}); err != nil {
			log.Printf("failed to mark scheduled job %q failed after run persistence error: %v", job.ID, err)
		}
	}
	l.deps.ServerState.RestoreStatusSnapshot(server.Name, preStartStatus)
	runStatus := policies.RunFailed
	reason := policies.RunReasonPersistence
	finishedAt := l.deps.JobTimestampNow()
	if err := l.deps.PolicyRepository.UpdateRun(run.ID, policies.RunUpdate{
		Status:     &runStatus,
		Reason:     &reason,
		Summary:    &jobSummary,
		JobID:      &job.ID,
		FinishedAt: &finishedAt,
	}); err != nil {
		log.Printf("failed to mark scheduled run %d failed after start persistence error: %v", run.ID, err)
	}
	_ = l.deps.AuditService.Record("system", "", "schedule.run.failed", "server", server.Name, "failure", jobSummary, map[string]any{
		"policy_id":         policy.ID,
		"policy_name":       policy.Name,
		"scheduled_for_utc": run.ScheduledForUTC,
		"job_id":            job.ID,
		"error":             persistenceErr.Error(),
	})
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
	l.WatchJobContext(l.deps.ReconciliationContext, runID, jobID)
}

// WatchJobContext keeps Scheduled Run state converged with its authoritative
// job while allowing application shutdown and tests to cancel the watcher.
func (l *Lifecycle) WatchJobContext(ctx context.Context, runID int64, jobID string) {
	if ctx == nil {
		ctx = context.Background()
	}
	readFailures := 0
	missingConfirmations := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		var jm *jobs.Manager
		if l.deps.CurrentJobManager != nil {
			jm = l.deps.CurrentJobManager()
		}
		if jm == nil {
			readFailures++
			if readFailures >= l.deps.ReconciliationAttempts {
				l.persistReconciliationFailure(ctx, runID, jobID, policies.RunReasonPersistence, "Scheduled run interrupted because the job manager remained unavailable")
				return
			}
			if !l.wait(ctx, readFailures) {
				return
			}
			continue
		}
		job, err := jm.GetJob(jobID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				missingConfirmations++
				if missingConfirmations >= l.deps.MissingJobConfirmations {
					l.persistReconciliationFailure(ctx, runID, jobID, policies.RunReasonMissing, "Scheduled run interrupted because its job could not be found")
					return
				}
			} else {
				missingConfirmations = 0
				readFailures++
				if !isTransientReconciliationError(err) || readFailures >= l.deps.ReconciliationAttempts {
					l.persistReconciliationFailure(ctx, runID, jobID, policies.RunReasonPersistence, "Scheduled run interrupted because its job state could not be read")
					return
				}
			}
			if !l.wait(ctx, max(readFailures, missingConfirmations)) {
				return
			}
			continue
		}
		readFailures = 0
		missingConfirmations = 0
		if err := l.reconcileJob(ctx, runID, job); err != nil {
			log.Printf("failed to reconcile scheduled run %d from job %q: %v", runID, job.ID, err)
			if ctx.Err() == nil {
				l.persistReconciliationFailure(ctx, runID, jobID, policies.RunReasonPersistence, "Scheduled run interrupted because its reconciled state could not be persisted")
			}
			return
		}
		switch job.Status {
		case jobs.StatusSucceeded, jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusInterrupted:
			return
		}
		if !l.wait(ctx, 1) {
			return
		}
	}
}

// ReconcileRun reloads the authoritative job for a persisted run. Rollout
// gates use it before treating a stale non-terminal projection as decisive.
func (l *Lifecycle) ReconcileRun(ctx context.Context, run policies.Run) (policies.Run, error) {
	if strings.TrimSpace(run.JobID) == "" {
		return run, nil
	}
	if ctx == nil {
		ctx = l.deps.ReconciliationContext
	}
	if l.deps.CurrentJobManager == nil {
		return run, errors.New("job manager is unavailable")
	}
	manager := l.deps.CurrentJobManager()
	if manager == nil {
		return run, errors.New("job manager is unavailable")
	}
	job, err := manager.GetJob(run.JobID)
	if err != nil {
		return run, err
	}
	if err := l.reconcileJob(ctx, run.ID, job); err != nil {
		return run, err
	}
	return l.getRunWithRetry(ctx, run.ID)
}

func (l *Lifecycle) getRunWithRetry(ctx context.Context, runID int64) (policies.Run, error) {
	var run policies.Run
	err := l.retry(ctx, func() error {
		var err error
		run, err = l.deps.PolicyRepository.GetRun(runID)
		return err
	})
	return run, err
}

func (l *Lifecycle) updateRunWithRetry(ctx context.Context, runID int64, update policies.RunUpdate) error {
	return l.retry(ctx, func() error { return l.deps.PolicyRepository.UpdateRun(runID, update) })
}

func (l *Lifecycle) transitionRunTerminalWithRetry(ctx context.Context, repository interface {
	TransitionRunTerminal(int64, policies.RunUpdate) (bool, error)
}, runID int64, update policies.RunUpdate) (bool, error) {
	changed := false
	err := l.retry(ctx, func() error {
		var err error
		changed, err = repository.TransitionRunTerminal(runID, update)
		return err
	})
	return changed, err
}

func (l *Lifecycle) retry(ctx context.Context, operation func() error) error {
	var err error
	for attempt := 1; attempt <= l.deps.ReconciliationAttempts; attempt++ {
		if err = operation(); err == nil {
			return nil
		}
		if !isTransientReconciliationError(err) || attempt == l.deps.ReconciliationAttempts || !l.wait(ctx, attempt) {
			return err
		}
	}
	return err
}

func (l *Lifecycle) wait(ctx context.Context, attempt int) bool {
	return l.deps.ReconciliationWait(ctx, l.deps.ReconciliationBackoff(attempt)) == nil
}

func (l *Lifecycle) persistReconciliationFailure(ctx context.Context, runID int64, jobID, reason, summary string) {
	status := policies.RunInterrupted
	finishedAt := l.deps.JobTimestampNow()
	update := policies.RunUpdate{Status: &status, Reason: &reason, Summary: &summary, JobID: &jobID, FinishedAt: &finishedAt}
	if repository, ok := l.deps.PolicyRepository.(interface {
		TransitionRunTerminal(int64, policies.RunUpdate) (bool, error)
	}); ok {
		if _, err := l.transitionRunTerminalWithRetry(ctx, repository, runID, update); err != nil {
			log.Printf("failed to persist reconciliation failure for scheduled run %d: %v", runID, err)
		}
		return
	}
	if err := l.updateRunWithRetry(ctx, runID, update); err != nil {
		log.Printf("failed to persist reconciliation failure for scheduled run %d: %v", runID, err)
	}
}

func isTerminalRunStatus(status string) bool {
	switch status {
	case policies.RunSucceeded, policies.RunFailed, policies.RunSkipped, policies.RunCancelled, policies.RunInterrupted:
		return true
	default:
		return false
	}
}

func isTransientReconciliationError(err error) bool {
	if err == nil || errors.Is(err, sql.ErrNoRows) {
		return false
	}
	type temporary interface{ Temporary() bool }
	var temporaryErr temporary
	if errors.As(err, &temporaryErr) && temporaryErr.Temporary() {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, fragment := range []string{"database is locked", "database is busy", "sqlite_busy", "sqlite_locked", "temporarily unavailable", "temporary failure", "timeout"} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}

func waitForReconciliation(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func reconciliationBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := 250 * time.Millisecond * time.Duration(1<<min(attempt-1, 3))
	jitterRange := delay / 5
	if jitterRange == 0 {
		return delay
	}
	jitter := time.Duration(time.Now().UnixNano()%int64(2*jitterRange+1)) - jitterRange
	return delay + jitter
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
