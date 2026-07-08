package runtime

import (
	"testing"

	"debian-updater/internal/jobs"
)

func TestStatusInProgress(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"updating", StatusUpdating, true},
		{"pending approval", StatusPendingApproval, true},
		{"approved", StatusApproved, true},
		{"upgrading", StatusUpgrading, true},
		{"autoremove", StatusAutoremove, true},
		{"sudoers", StatusSudoers, true},
		{"facts refresh", StatusFactsRefresh, true},
		{"idle", StatusIdle, false},
		{"done", StatusDone, false},
		{"exact match only", " updating ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StatusInProgress(tt.status); got != tt.want {
				t.Fatalf("StatusInProgress(%q) = %t, want %t", tt.status, got, tt.want)
			}
		})
	}
}

func TestBlocksTransientAction(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   bool
	}{
		{"normalizes active status", " UPDATING ", true},
		{"normalizes pending approval", " pending_approval ", true},
		{"blocks approved", StatusApproved, true},
		{"allows idle", StatusIdle, false},
		{"allows terminal", " DONE ", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := BlocksTransientAction(tt.status); got != tt.want {
				t.Fatalf("BlocksTransientAction(%q) = %t, want %t", tt.status, got, tt.want)
			}
		})
	}
}

func TestRuntimeStatusFromJob(t *testing.T) {
	tests := []struct {
		name   string
		record jobs.Record
		want   string
	}{
		{"ignores unsupported kind", jobs.Record{Kind: jobs.KindBackupExport, Status: jobs.StatusRunning}, ""},
		{"waiting approval wins", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusWaitingApproval}, StatusPendingApproval},
		{"succeeded", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusSucceeded}, StatusDone},
		{"failed", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusFailed}, StatusError},
		{"cancelled", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusCancelled}, StatusCancelled},
		{"interrupted", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusInterrupted}, StatusIdle},
		{"update approval phase", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusRunning, Phase: jobs.PhaseApprovalWait}, StatusPendingApproval},
		{"update upgrade phase", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusRunning, Phase: jobs.PhaseAptUpgrade}, StatusUpgrading},
		{"update postchecks phase", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusRunning, Phase: jobs.PhasePostchecks}, StatusUpgrading},
		{"update complete phase", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusRunning, Phase: jobs.PhaseComplete}, StatusUpgrading},
		{"update default", jobs.Record{Kind: jobs.KindUpdate, Status: jobs.StatusRunning}, StatusUpdating},
		{"autoremove", jobs.Record{Kind: jobs.KindAutoremove, Status: jobs.StatusRunning}, StatusAutoremove},
		{"sudoers enable", jobs.Record{Kind: jobs.KindSudoersEnable, Status: jobs.StatusRunning}, StatusSudoers},
		{"sudoers disable", jobs.Record{Kind: jobs.KindSudoersDisable, Status: jobs.StatusRunning}, StatusSudoers},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := RuntimeStatusFromJob(tt.record); got != tt.want {
				t.Fatalf("RuntimeStatusFromJob(%+v) = %q, want %q", tt.record, got, tt.want)
			}
		})
	}
}

func TestJobUpdateFromServerStatus(t *testing.T) {
	tests := []struct {
		name              string
		status            string
		options           ServerStatusJobUpdateOptions
		wantStatus        string
		wantPhase         string
		wantPhaseSet      bool
		wantSummary       string
		wantSummarySet    bool
		wantLogs          string
		wantFinishedAt    string
		wantFinishedAtSet bool
		wantErrorClass    string
		wantErrorClassSet bool
		wantFinishesJob   bool
	}{
		{
			name:           "pending approval",
			status:         StatusPendingApproval,
			options:        ServerStatusJobUpdateOptions{Logs: "pending logs"},
			wantStatus:     jobs.StatusWaitingApproval,
			wantPhase:      jobs.PhaseApprovalWait,
			wantPhaseSet:   true,
			wantSummary:    "Waiting for approval",
			wantSummarySet: true,
			wantLogs:       "pending logs",
		},
		{
			name:              "done",
			status:            StatusDone,
			options:           ServerStatusJobUpdateOptions{Logs: "done logs", Timestamp: "2026-05-01T12:00:00Z"},
			wantStatus:        jobs.StatusSucceeded,
			wantPhase:         jobs.PhaseComplete,
			wantPhaseSet:      true,
			wantSummary:       "Completed successfully",
			wantSummarySet:    true,
			wantLogs:          "done logs",
			wantFinishedAt:    "2026-05-01T12:00:00Z",
			wantFinishedAtSet: true,
			wantFinishesJob:   true,
		},
		{
			name:              "error with class",
			status:            StatusError,
			options:           ServerStatusJobUpdateOptions{Logs: "error logs", LastErrorClass: " transient ", Timestamp: "2026-05-01T12:01:00Z"},
			wantStatus:        jobs.StatusFailed,
			wantPhase:         jobs.PhaseComplete,
			wantPhaseSet:      true,
			wantSummary:       "Completed with errors",
			wantSummarySet:    true,
			wantLogs:          "error logs",
			wantFinishedAt:    "2026-05-01T12:01:00Z",
			wantFinishedAtSet: true,
			wantErrorClass:    "transient",
			wantErrorClassSet: true,
			wantFinishesJob:   true,
		},
		{
			name:              "cancelled",
			status:            StatusCancelled,
			options:           ServerStatusJobUpdateOptions{Timestamp: "2026-05-01T12:02:00Z"},
			wantStatus:        jobs.StatusCancelled,
			wantPhase:         jobs.PhaseComplete,
			wantPhaseSet:      true,
			wantSummary:       "Cancelled",
			wantSummarySet:    true,
			wantFinishedAt:    "2026-05-01T12:02:00Z",
			wantFinishedAtSet: true,
			wantFinishesJob:   true,
		},
		{
			name:           "approved",
			status:         StatusApproved,
			wantStatus:     jobs.StatusRunning,
			wantPhase:      jobs.PhaseAptUpgrade,
			wantPhaseSet:   true,
			wantSummary:    "Approval received",
			wantSummarySet: true,
		},
		{
			name:         "active status preserves current phase",
			status:       StatusUpdating,
			options:      ServerStatusJobUpdateOptions{CurrentPhase: " " + jobs.PhaseAptUpdate + " "},
			wantStatus:   jobs.StatusRunning,
			wantPhase:    jobs.PhaseAptUpdate,
			wantPhaseSet: true,
		},
		{
			name:       "unknown status only marks running",
			status:     "custom",
			wantStatus: jobs.StatusRunning,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			update := JobUpdateFromServerStatus(tt.status, tt.options)
			assertStringPointer(t, "Status", update.Status, true, tt.wantStatus)
			assertStringPointer(t, "Phase", update.Phase, tt.wantPhaseSet, tt.wantPhase)
			assertStringPointer(t, "Summary", update.Summary, tt.wantSummarySet, tt.wantSummary)
			assertStringPointer(t, "LogsText", update.LogsText, true, tt.wantLogs)
			assertStringPointer(t, "FinishedAt", update.FinishedAt, tt.wantFinishedAtSet, tt.wantFinishedAt)
			assertStringPointer(t, "ErrorClass", update.ErrorClass, tt.wantErrorClassSet, tt.wantErrorClass)
			if got := ServerStatusFinishesJob(tt.status); got != tt.wantFinishesJob {
				t.Fatalf("ServerStatusFinishesJob(%q) = %t, want %t", tt.status, got, tt.wantFinishesJob)
			}
		})
	}
}

func TestTimelineProjectionFromJob(t *testing.T) {
	tests := []struct {
		name string
		job  jobs.Record
		want TimelineProjection
	}{
		{"succeeded", jobs.Record{Status: jobs.StatusSucceeded}, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}},
		{"failed", jobs.Record{Status: jobs.StatusFailed}, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"cancelled", jobs.Record{Status: jobs.StatusCancelled}, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"interrupted", jobs.Record{Status: jobs.StatusInterrupted}, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"waiting approval", jobs.Record{Status: jobs.StatusWaitingApproval}, TimelineProjection{CurrentPhase: TimelinePhasePendingApproval, State: TimelineStateWaiting}},
		{"queued defaults phase", jobs.Record{Status: jobs.StatusQueued}, TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateQueued}},
		{"running apt update", jobs.Record{Status: jobs.StatusRunning, Phase: jobs.PhaseAptUpdate}, TimelineProjection{CurrentPhase: TimelinePhaseAptUpdate, State: TimelineStateActive}},
		{"unknown status with phase", jobs.Record{Status: "custom", Phase: jobs.PhasePostchecks}, TimelineProjection{CurrentPhase: TimelinePhasePostchecks, State: TimelineStateActive}},
		{"unknown status without phase", jobs.Record{Status: "custom"}, TimelineProjection{State: TimelineStateIdle}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TimelineProjectionFromJob(tt.job); got != tt.want {
				t.Fatalf("TimelineProjectionFromJob(%+v) = %+v, want %+v", tt.job, got, tt.want)
			}
		})
	}
}

func TestTimelineProjectionFromServerStatus(t *testing.T) {
	tests := []struct {
		name   string
		status string
		want   TimelineProjection
	}{
		{"pending approval", StatusPendingApproval, TimelineProjection{CurrentPhase: TimelinePhasePendingApproval, State: TimelineStateWaiting}},
		{"updating", StatusUpdating, TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateActive}},
		{"sudoers", StatusSudoers, TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateActive}},
		{"facts refresh", StatusFactsRefresh, TimelineProjection{CurrentPhase: TimelinePhasePrechecks, State: TimelineStateActive}},
		{"upgrading", StatusUpgrading, TimelineProjection{CurrentPhase: TimelinePhaseUpgrade, State: TimelineStateActive}},
		{"autoremove", StatusAutoremove, TimelineProjection{CurrentPhase: TimelinePhaseUpgrade, State: TimelineStateActive}},
		{"done", StatusDone, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}},
		{"success", "success", TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}},
		{"approved", StatusApproved, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateDone}},
		{"error", StatusError, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"failure", "failure", TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"failed", "failed", TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"cancelled", StatusCancelled, TimelineProjection{CurrentPhase: TimelinePhaseDoneError, State: TimelineStateError}},
		{"normalizes input", " UPGRADING ", TimelineProjection{CurrentPhase: TimelinePhaseUpgrade, State: TimelineStateActive}},
		{"unknown", "custom", TimelineProjection{State: TimelineStateIdle}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TimelineProjectionFromServerStatus(tt.status); got != tt.want {
				t.Fatalf("TimelineProjectionFromServerStatus(%q) = %+v, want %+v", tt.status, got, tt.want)
			}
		})
	}
}

func TestDashboardTimelineJobForStatus(t *testing.T) {
	runningJob := jobs.Record{ID: "job-running", Status: jobs.StatusRunning, Phase: jobs.PhaseAptUpdate}
	failedJob := jobs.Record{ID: "job-failed", Status: jobs.StatusFailed, Phase: jobs.PhasePostchecks}
	doneJob := jobs.Record{ID: "job-done", Status: jobs.StatusSucceeded, Phase: jobs.PhaseComplete}
	unknownJob := jobs.Record{ID: "job-unknown", Status: "custom"}
	tests := []struct {
		name   string
		status string
		job    *jobs.Record
		want   *jobs.Record
	}{
		{"nil job", StatusIdle, nil, nil},
		{"empty job id", StatusIdle, &jobs.Record{Status: jobs.StatusRunning}, nil},
		{"idle server keeps active job", StatusIdle, &runningJob, &runningJob},
		{"active server drops stale terminal job", StatusUpdating, &failedJob, nil},
		{"terminal mismatch drops job", StatusError, &doneJob, nil},
		{"terminal match keeps job", StatusError, &failedJob, &failedJob},
		{"idle job is ignored", StatusIdle, &unknownJob, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DashboardTimelineJobForStatus(tt.status, tt.job); got != tt.want {
				t.Fatalf("DashboardTimelineJobForStatus(%q, %+v) = %+v, want %+v", tt.status, tt.job, got, tt.want)
			}
		})
	}
}

func assertStringPointer(t *testing.T, name string, got *string, wantSet bool, want string) {
	t.Helper()
	if got == nil {
		if wantSet {
			t.Fatalf("%s = nil, want %q", name, want)
		}
		return
	}
	if !wantSet {
		t.Fatalf("%s = %q, want nil", name, *got)
	}
	if *got != want {
		t.Fatalf("%s = %q, want %q", name, *got, want)
	}
}
