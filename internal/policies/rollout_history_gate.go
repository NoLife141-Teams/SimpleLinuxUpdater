package policies

import "strings"

// Keep failed predecessors authoritative independently of current matching.
// Removed active predecessors must also settle before the occurrence continues.
func (s *Service) rolloutHistoryGate(policyID int64, scheduledFor string, matched []string, runs map[string]Run, reconcile bool) string {
	deps := s.EnsureDeps()
	names := make(map[string]bool, len(matched))
	for _, name := range matched {
		names[strings.ToLower(strings.TrimSpace(name))] = true
	}
	gate := "ready"
	for key, run := range runs {
		if run.PolicyID != policyID || run.ScheduledForUTC != scheduledFor {
			continue
		}
		if reconcile && deps.ReconcileRun != nil && run.JobID != "" && (!isTerminalRunStatus(run.Status) || restartInterruptedRun(run)) {
			updated, err := deps.ReconcileRun(run)
			if err != nil {
				gate = "waiting"
				continue
			}
			run = updated
			runs[key] = run
		}
		switch run.Status {
		case RunFailed, RunSkipped, RunCancelled, RunInterrupted:
			return "failed"
		case RunSucceeded:
		default:
			if !names[strings.ToLower(strings.TrimSpace(run.ServerName))] {
				gate = "waiting"
			}
		}
	}
	return gate
}
