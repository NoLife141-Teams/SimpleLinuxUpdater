package runtime

import (
	"strings"

	"debian-updater/internal/jobs"
)

const (
	StatusIdle            = "idle"
	StatusUpdating        = "updating"
	StatusPendingApproval = "pending_approval"
	StatusApproved        = "approved"
	StatusUpgrading       = "upgrading"
	StatusAutoremove      = "autoremove"
	StatusSudoers         = "sudoers"
	StatusFactsRefresh    = "facts_refresh"
	StatusDone            = "done"
	StatusError           = "error"
	StatusCancelled       = "cancelled"

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
		StatusSudoers,
		StatusFactsRefresh:
		return true
	default:
		return false
	}
}

func RuntimeStatusFromJob(record jobs.Record) string {
	switch record.Kind {
	case jobs.KindUpdate, jobs.KindAutoremove, jobs.KindSudoersEnable, jobs.KindSudoersDisable:
	default:
		return ""
	}

	switch record.Status {
	case jobs.StatusWaitingApproval:
		return StatusPendingApproval
	case jobs.StatusSucceeded:
		return StatusDone
	case jobs.StatusFailed:
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
	case jobs.KindSudoersEnable, jobs.KindSudoersDisable:
		return StatusSudoers
	default:
		return ""
	}
}

func ServerStatusFinishesJob(status string) bool {
	switch status {
	case StatusDone, StatusError, StatusCancelled:
		return true
	default:
		return false
	}
}

func JobUpdateFromServerStatus(status string, options ServerStatusJobUpdateOptions) jobs.Update {
	logs := options.Logs
	update := jobs.Update{LogsText: &logs}
	switch status {
	case StatusPendingApproval:
		status := jobs.StatusWaitingApproval
		phase := jobs.PhaseApprovalWait
		summary := "Waiting for approval"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	case StatusDone:
		status := jobs.StatusSucceeded
		phase := jobs.PhaseComplete
		summary := "Completed successfully"
		finishedAt := options.Timestamp
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
		update.FinishedAt = &finishedAt
	case StatusError:
		status := jobs.StatusFailed
		phase := jobs.PhaseComplete
		summary := "Completed with errors"
		finishedAt := options.Timestamp
		errorClass := strings.TrimSpace(options.LastErrorClass)
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
		update.FinishedAt = &finishedAt
		if errorClass != "" {
			update.ErrorClass = &errorClass
		}
	case StatusCancelled:
		status := jobs.StatusCancelled
		phase := jobs.PhaseComplete
		summary := "Cancelled"
		finishedAt := options.Timestamp
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
		update.FinishedAt = &finishedAt
	case StatusApproved:
		status := jobs.StatusRunning
		phase := jobs.PhaseAptUpgrade
		summary := "Approval received"
		update.Status = &status
		update.Phase = &phase
		update.Summary = &summary
	default:
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
	case jobs.PhaseAptUpgrade, jobs.PhaseAutoremove, jobs.PhaseApply:
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
	case StatusUpgrading, StatusAutoremove:
		return TimelineProjection{CurrentPhase: TimelinePhaseUpgrade, State: TimelineStateActive}
	case StatusDone, "success", StatusApproved:
		return TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}
	case StatusError, "failure", "failed", StatusCancelled:
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
