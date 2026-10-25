package updates

import (
	"errors"
	"strings"

	"debian-updater/internal/servers"
)

// The caller holds server state until both the waiting job and displayed plan
// are committed. Job persistence deliberately does not re-enter runtime state.
func (r *withActorRunner) commitPendingApproval(snapshot *servers.ServerStatus, previousLogs, updatedLogs string) error {
	if strings.TrimSpace(r.jobID) == "" {
		return nil // Untracked embedded runners have no persisted job to publish.
	}
	jm := r.currentJobManager()
	if jm == nil {
		return errors.New("job manager unavailable")
	}
	var replacement *string
	appendLog := ""
	if strings.HasPrefix(updatedLogs, previousLogs) {
		appendLog = strings.TrimPrefix(updatedLogs, previousLogs)
	} else {
		replacement = &updatedLogs
	}
	generation := snapshot.ApprovalGeneration
	// Automatic approval uses an internal waiting transition, without an operator
	// decision. Generation zero excludes it from actionable notification facts.
	if r.automaticApproval {
		generation = 0
	}
	committed, err := jm.CommitPendingApproval(r.jobID, appendLog, replacement, generation)
	if err != nil {
		return err
	}
	snapshot.JobRevision = committed.Revision
	return nil
}
