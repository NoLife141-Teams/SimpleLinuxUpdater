package main

import (
	"errors"
	"net/http"

	jobspkg "debian-updater/internal/jobs"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

// Every browser decision identifies the exact pending plan the operator saw.
type serverActionApprovalIdentity struct {
	JobID      string `json:"job_id"`
	Generation uint64 `json:"approval_generation"`
}

type serverActionApprovalRequest struct {
	serverActionApprovalIdentity
	ConfirmRemovals bool `json:"confirm_removals"`
}

const staleApprovalMessage = "The pending update changed. Refresh the package plan and confirm again."

func (l *serverActionLifecycle) ApproveAll(name string, identity serverActionApprovalIdentity) serverActionLifecycleResult {
	return l.approve(name, identity, updatespkg.ApprovalScopeAll, false)
}
func (l *serverActionLifecycle) ApproveSecurity(name string, identity serverActionApprovalIdentity) serverActionLifecycleResult {
	return l.approve(name, identity, updatespkg.ApprovalScopeSecurity, false)
}
func (l *serverActionLifecycle) ApproveKeptBackSecurity(name string, confirmRemovals bool, identity serverActionApprovalIdentity) serverActionLifecycleResult {
	return l.approve(name, identity, updatespkg.ApprovalScopeSecurityKeptBack, confirmRemovals)
}
func (l *serverActionLifecycle) ApproveFullUpgrade(name string, confirmRemovals bool, identity serverActionApprovalIdentity) serverActionLifecycleResult {
	return l.approve(name, identity, updatespkg.ApprovalScopeFullUpgrade, confirmRemovals)
}

// The state lock excludes plan publication, timeout, cancellation and a second
// browser decision until persistence and runtime state agree. The job manager's
// approval-specific operation deliberately leaves runtime publication to us.
func (l *serverActionLifecycle) withPendingApproval(name string, identity serverActionApprovalIdentity, apply func(*ServerStatus) serverActionLifecycleResult) serverActionLifecycleResult {
	l.serverState.Lock()
	defer l.serverState.Unlock()
	status := l.serverState.StatusMap()[name]
	if status == nil {
		return jsonResult(http.StatusNotFound, "Server not found")
	}
	if status.Status != "pending_approval" {
		return jsonResult(http.StatusConflict, "Server not pending approval")
	}
	if identity.JobID == "" || identity.Generation == 0 || status.JobID != identity.JobID || status.ApprovalGeneration != identity.Generation {
		return jsonResult(http.StatusConflict, staleApprovalMessage)
	}
	return apply(status)
}

func (l *serverActionLifecycle) persistPendingDecision(status *ServerStatus, cancelled bool, summary string) error {
	jm := l.currentJobManager()
	if jm == nil {
		return errors.New("job manager unavailable")
	}
	job, err := jm.GetJob(status.JobID)
	if err != nil {
		return err
	}
	if job.ServerName != status.Name || job.Kind != jobKindUpdate || job.Status != jobStatusWaitingApproval {
		return jobspkg.ErrTransitionConflict
	}
	committed, err := jm.ResolvePendingApproval(job.ID, job.Revision, cancelled, summary)
	if err != nil {
		return err
	}
	status.JobRevision = committed.Revision
	return nil
}

func (l *serverActionLifecycle) approve(name string, identity serverActionApprovalIdentity, scope string, confirmRemovals bool) serverActionLifecycleResult {
	var approval updatespkg.ApprovalScopeInterpretation
	var persistErr error
	result := l.withPendingApproval(name, identity, func(status *ServerStatus) serverActionLifecycleResult {
		approval = updatespkg.EvaluateManualApproval(status, scope, updatespkg.ApprovalScopeOptions{ConfirmRemovals: confirmRemovals})
		if !approval.Allowed {
			body := map[string]any{"error": approval.BodyMessage}
			if len(approval.RemovedPackages) > 0 {
				body["removed_packages"] = approval.RemovedPackages
			}
			return serverActionLifecycleResult{statusCode: http.StatusConflict, body: body}
		}
		if persistErr = l.persistPendingDecision(status, false, approval.JobSummary); persistErr != nil {
			if errors.Is(persistErr, jobspkg.ErrTransitionConflict) {
				return jsonResult(http.StatusConflict, staleApprovalMessage)
			}
			return jsonResult(http.StatusInternalServerError, "Failed to persist approval")
		}
		status.ApprovalScope = scope
		status.ApprovalConfirmRemovals = approval.StateOptions.ConfirmRemovals
		status.Status = "approved"
		return serverActionLifecycleResult{statusCode: http.StatusOK, body: map[string]any{"message": approval.SuccessMessage}}
	})
	meta := approval.AuditMeta
	if meta == nil {
		meta = map[string]any{"scope": scope}
	}
	meta["job_id"] = identity.JobID
	meta["approval_generation"] = identity.Generation
	auditStatus, message := approval.AuditStatus, approval.AuditMessage
	if persistErr != nil {
		meta["error"] = persistErr.Error()
	}
	if result.statusCode != http.StatusOK && (approval.Allowed || message == "") {
		auditStatus = "ignored"
		if result.statusCode >= 500 || result.statusCode == http.StatusNotFound {
			auditStatus = "failure"
		}
		message, _ = result.body["error"].(string)
	}
	l.recordAuditWithMeta("update.approve", name, auditStatus, message, meta)
	return result
}

func (l *serverActionLifecycle) Cancel(name string, identity serverActionApprovalIdentity) serverActionLifecycleResult {
	var persistErr error
	result := l.withPendingApproval(name, identity, func(status *ServerStatus) serverActionLifecycleResult {
		if persistErr = l.persistPendingDecision(status, true, "Update cancelled"); persistErr != nil {
			if errors.Is(persistErr, jobspkg.ErrTransitionConflict) {
				return jsonResult(http.StatusConflict, staleApprovalMessage)
			}
			return jsonResult(http.StatusInternalServerError, "Failed to persist cancelled update")
		}
		status.Status = "cancelled"
		status.ApprovalScope = ""
		status.ApprovalConfirmRemovals = false
		status.Logs = ""
		status.Upgradable = nil
		status.PendingUpdates = nil
		status.UpgradePlan = serverpkg.UpgradePlan{}
		return serverActionLifecycleResult{statusCode: http.StatusOK, body: map[string]any{"message": "Upgrade cancelled"}}
	})
	auditStatus, message := "success", "Upgrade cancelled"
	meta := map[string]any{"job_id": identity.JobID, "approval_generation": identity.Generation}
	if result.statusCode != http.StatusOK {
		auditStatus = "ignored"
		if result.statusCode >= 500 || result.statusCode == http.StatusNotFound {
			auditStatus = "failure"
		}
		message, _ = result.body["error"].(string)
	}
	if persistErr != nil {
		meta["error"] = persistErr.Error()
	}
	l.recordAuditWithMeta("update.cancel", name, auditStatus, message, meta)
	return result
}
