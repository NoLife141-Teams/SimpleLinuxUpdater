package runtime

import (
	"strings"

	"debian-updater/internal/jobs"
)

const (
	StatusIdle                = "idle"
	StatusUpdating            = "updating"
	StatusPendingApproval     = "pending_approval"
	StatusApproved            = "approved"
	StatusUpgrading           = "upgrading"
	StatusAutoremove          = "autoremove"
	StatusRepairing           = "repairing"
	StatusNeedsReconciliation = "needs_reconciliation"
	StatusSudoers             = "sudoers"
	StatusFactsRefresh        = "facts_refresh"
	StatusDone                = "done"
	StatusError               = "error"
	StatusCancelled           = "cancelled"

	TimelinePhasePendingApproval = "pending_approval"
	TimelinePhasePrechecks       = "prechecks"
	TimelinePhaseAptUpdate       = "apt_update"
	TimelinePhaseUpgrade         = "upgrade"
	TimelinePhasePostchecks      = "postchecks"
	TimelinePhaseDoneError       = "done_error"

	TimelineStateIdle    = "idle"
	TimelineStateQueued  = "queued"
	TimelineStateActive  = "active"
	TimelineStateWaiting = "waiting"
	TimelineStateDone    = "done"
	TimelineStateError   = "error"
)

type ServerStatusJobUpdateOptions struct {
	Logs           string
	LastErrorClass string
	CurrentPhase   string
	Timestamp      string
}

type TimelineProjection struct {
	CurrentPhase string
	State        string
}

func StatusInProgress(status string) bool {
	switch status {
	case StatusUpdating,
		StatusPendingApproval,
		StatusApproved,
		StatusUpgrading,
		StatusAutoremove,
		StatusRepairing,
		StatusSudoers,
		StatusFactsRefresh:
		return true
	default:
		return false
	}
}

func BlocksTransientAction(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusUpdating,
		StatusPendingApproval,
		StatusApproved,
		StatusUpgrading,
		StatusAutoremove,
		StatusRepairing,
		StatusSudoers,
		StatusFactsRefresh:
		return true
	default:
		return false
	}
}

func BlocksPackageMutation(status string) bool {
	return BlocksTransientAction(status) || strings.EqualFold(strings.TrimSpace(status), StatusNeedsReconciliation)
}

func RuntimeStatusFromJob(record jobs.Record) string {
	switch record.Kind {
	case jobs.KindUpdate, jobs.KindAutoremove, jobs.KindAptRepair, jobs.KindSudoersEnable, jobs.KindSudoersDisable:
	default:
		return ""
	}

	switch record.Status {
	case jobs.StatusWaitingApproval:
		return StatusPendingApproval
	case jobs.StatusSucceeded:
		return StatusDone
	case jobs.StatusFailed:
		if strings.EqualFold(strings.TrimSpace(record.ErrorClass), "reconciliation_required") {
			return StatusNeedsReconciliation
		}
		return StatusError
	case jobs.StatusCancelled:
		return StatusCancelled
	case jobs.StatusInterrupted:
		return StatusIdle
	}
	switch record.Kind {
	case jobs.KindUpdate:
		switch record.Phase {
		case jobs.PhaseApprovalWait:
			return StatusPendingApproval
		case jobs.PhaseAptUpgrade, jobs.PhasePostchecks, jobs.PhaseComplete:
			return StatusUpgrading
		default:
			return StatusUpdating
		}
	case jobs.KindAutoremove:
		return StatusAutoremove
	case jobs.KindAptRepair:
		return StatusRepairing
	case jobs.KindSudoersEnable, jobs.KindSudoersDisable:
		return StatusSudoers
	default:
		return ""
	}
}

func ServerStatusFinishesJob(status string) bool {
	switch status {
	case StatusDone, StatusError, StatusNeedsReconciliation, StatusCancelled:
		return true
	default:
		return false
	}
}

func JobTransitionIntentFromServerStatus(status string, options ServerStatusJobUpdateOptions) jobs.Intent {
	logs := options.Logs
	update := jobs.Intent{LogsText: &logs}
	switch status {
	case StatusPendingApproval:
		update.Kind = jobs.IntentWaitForApproval
		status := jobs.StatusWaitingApproval
		phase := jobs.PhaseApprovalWait
		summary := "Waiting for approval"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	case StatusDone:
		update.Kind = jobs.IntentSucceed
		status := jobs.StatusSucceeded
		phase := jobs.PhaseComplete
		summary := "Completed successfully"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	case StatusError:
		update.Kind = jobs.IntentFail
		status := jobs.StatusFailed
		phase := jobs.PhaseComplete
		summary := "Completed with errors"
		errorClass := strings.TrimSpace(options.LastErrorClass)
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
		if errorClass != "" {
			update.ErrorClass = &errorClass
		}
	case StatusNeedsReconciliation:
		update.Kind = jobs.IntentFail
		status := jobs.StatusFailed
		phase := jobs.PhaseComplete
		summary := "APT reconciliation required"
		errorClass := "reconciliation_required"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
		update.ErrorClass = &errorClass
	case StatusCancelled:
		update.Kind = jobs.IntentCancel
		status := jobs.StatusCancelled
		phase := jobs.PhaseComplete
		summary := "Cancelled"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	case StatusApproved:
		update.Kind = jobs.IntentResumeAfterApproval
		status := jobs.StatusRunning
		phase := jobs.PhaseAptUpgrade
		summary := "Approval received"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	default:
		update.Kind = jobs.IntentAdvance
		status := jobs.StatusRunning
		update.Status = &status
		if currentPhase := strings.TrimSpace(options.CurrentPhase); currentPhase != "" {
			phase := currentPhase
			update.Phase = &phase
		}
	}
	return update
}

func TimelinePhaseFromJobPhase(phase string) string {
	switch strings.ToLower(strings.TrimSpace(phase)) {
	case jobs.PhaseDial, jobs.PhasePrechecks:
		return TimelinePhasePrechecks
	case jobs.PhaseAptUpdate:
		return TimelinePhaseAptUpdate
	case jobs.PhaseApprovalWait:
		return TimelinePhasePendingApproval
	case jobs.PhaseAptUpgrade, jobs.PhaseAutoremove, jobs.PhaseReconcile, jobs.PhaseApply:
		return TimelinePhaseUpgrade
	case jobs.PhasePostchecks:
		return TimelinePhasePostchecks
	case jobs.PhaseComplete:
		return TimelinePhaseDoneError
	default:
		return ""
	}
}

func TimelineProjectionFromServerStatus(status string) TimelineProjection {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case StatusPendingApproval:
		return TimelineProjection{CurrentPhase: TimelinePhasePendingApproval, State: TimelineStateWaiting}
	case StatusUpdating:
		return TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateActive}
	case StatusSudoers, StatusFactsRefresh:
		return TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateActive}
	case StatusUpgrading, StatusAutoremove, StatusRepairing:
		return TimelineProjection{CurrentPhase: TimelinePhaseUpgrade, State: TimelineStateActive}
	case StatusDone, "success", StatusApproved:
		return TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}
	case StatusError, StatusNeedsReconciliation, "failure", "failed", StatusCancelled:
		return TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}
	default:
		return TimelineProjection{State: TimelineStateIdle}
	}
}

func TimelineProjectionFromJob(job jobs.Record) TimelineProjection {
	status := strings.ToLower(strings.TrimSpace(job.Status))
	phase := TimelinePhaseFromJobPhase(job.Phase)
	switch status {
	case jobs.StatusSucceeded:
		return TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}
	case jobs.StatusFailed, jobs.StatusCancelled, jobs.StatusInterrupted:
		return TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}
	case jobs.StatusWaitingApproval:
		return TimelineProjection{CurrentPhase: TimelinePhasePendingApproval, State: TimelineStateWaiting}
	case jobs.StatusQueued:
		if phase == "" {
			phase = TimelinePhasePrechecks
		}
		return TimelineProjection{CurrentPhase: phase, State: TimelineStateQueued}
	case jobs.StatusRunning:
		if phase == "" {
			phase = TimelinePhasePrechecks
		}
		return TimelineProjection{CurrentPhase: phase, State: TimelineStateActive}
	default:
		if phase != "" {
			return TimelineProjection{CurrentPhase: phase, State: TimelineStateActive}
		}
		return TimelineProjection{State: TimelineStateIdle}
	}
}

func DashboardTimelineJobForStatus(status string, job *jobs.Record) *jobs.Record {
	if job == nil || strings.TrimSpace(job.ID) == "" {
		return nil
	}
	serverProjection := TimelineProjectionFromServerStatus(status)
	jobProjection := TimelineProjectionFromJob(*job)
	if jobProjection.State == TimelineStateIdle {
		return nil
	}
	// A host awaiting operator approval must not briefly display progress from a
	// stale running job. The runtime waiting state is authoritative until the
	// matching job has also persisted its waiting-for-approval transition.
	if serverProjection.State == TimelineStateWaiting && jobProjection.State != TimelineStateWaiting {
		return nil
	}
	if ActiveTimelineState(serverProjection.State) && !ActiveTimelineState(jobProjection.State) {
		return nil
	}
	if TerminalTimelineState(serverProjection.State) &&
		TerminalTimelineState(jobProjection.State) &&
		serverProjection.State != jobProjection.State {
		return nil
	}
	return job
}

func ActiveTimelineState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case TimelineStateActive, TimelineStateQueued, TimelineStateWaiting:
		return true
	default:
		return false
	}
}

func RunningTimelineState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case TimelineStateActive, TimelineStateQueued:
		return true
	default:
		return false
	}
}

func TerminalTimelineState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case TimelineStateDone, TimelineStateError:
		return true
	default:
		return false
	}
}
