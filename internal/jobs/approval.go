package jobs

import "errors"

// ResolvePendingApproval atomically persists a decision for the exact waiting
// job revision. It never replaces logs. Its caller owns pending-plan validation
// and runtime publication; publishing here would re-enter that caller's lock.
func (m *Manager) ResolvePendingApproval(id string, expectedRevision int64, cancelled bool, summary string) (Record, error) {
	if m == nil || m.repo == nil {
		return Record{}, errors.New("job manager is not initialized")
	}
	current, err := m.repo.Get(id)
	if err != nil {
		return Record{}, err
	}
	if current.Status != StatusWaitingApproval || current.Revision != expectedRevision {
		return Record{}, ErrTransitionConflict
	}
	phase := PhaseAptUpgrade
	intent := Intent{Kind: IntentResumeAfterApproval, Phase: &phase, Summary: &summary}
	if cancelled {
		phase = PhaseComplete
		intent.Kind = IntentCancel
	}
	next, _, err := m.applyIntent(current, intent)
	if err != nil {
		return Record{}, err
	}
	updated, err := m.repo.ApplyTransition(next, expectedRevision, true)
	if err != nil {
		return Record{}, err
	}
	if !updated {
		return Record{}, ErrTransitionConflict
	}
	m.notify("job.update")
	return next, nil
}

// CommitPendingApproval persists the waiting phase and its log change before
// the caller exposes the pending plan. The caller owns runtime publication and
// holds its state lock; this operation must not invoke SyncRuntime.
func (m *Manager) CommitPendingApproval(id, appendLog string, replacementLogs *string) (Record, error) {
	if m == nil || m.repo == nil {
		return Record{}, errors.New("job manager is not initialized")
	}
	current, err := m.repo.Get(id)
	if err != nil {
		return Record{}, err
	}
	if current.Kind != KindUpdate || (current.Status != StatusRunning && current.Status != StatusWaitingApproval) {
		return Record{}, ErrTransitionConflict
	}
	summary := "Waiting for approval"
	next, _, err := m.applyIntent(current, Intent{Kind: IntentWaitForApproval, Summary: &summary, AppendLog: appendLog, LogsText: replacementLogs})
	if err != nil {
		return Record{}, err
	}
	var updated bool
	var logResult logAppendResult
	if repo, ok := m.repo.(structuredTransitionRepository); ok {
		updated, logResult, err = repo.applyTransitionWithLogResult(next, current.Revision, true)
	} else {
		updated, err = m.repo.ApplyTransition(next, current.Revision, true)
	}
	if err != nil {
		return Record{}, err
	}
	if !updated {
		return Record{}, ErrTransitionConflict
	}
	m.notifyLogResult(id, logResult)
	m.notify("job.update")
	return next, nil
}
