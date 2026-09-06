package policies

import (
	"strings"
	"time"
)

// reconciledRolloutGateStateAt keeps the persisted run projection converged
// with its authoritative job, then evaluates that projection as it was known at
// the historical scheduler instant. A terminal state reached after asOf cannot
// retroactively release a rollout wave that was still waiting at that instant.
func (s *Service) reconciledRolloutGateStateAt(policyID int64, scheduledForUTC string, previous []RolloutBatch, runs map[string]Run, asOf time.Time) string {
	_ = s.reconciledRolloutGateState(policyID, scheduledForUTC, previous, runs)
	return s.rolloutGateStateAt(policyID, scheduledForUTC, previous, runs, asOf)
}

func (s *Service) rolloutGateStateAt(policyID int64, scheduledForUTC string, previous []RolloutBatch, runs map[string]Run, asOf time.Time) string {
	deps := s.EnsureDeps()
	asOfUTC := asOf.UTC()
	for _, batch := range previous {
		for _, serverName := range batch.Servers {
			run, ok := runs[rolloutRunKey(policyID, scheduledForUTC, serverName)]
			if !ok {
				return "waiting"
			}
			if isTerminalRunStatus(run.Status) {
				finishedRaw := strings.TrimSpace(run.FinishedAt)
				finishedAt, known := parsePolicyInstant(finishedRaw, deps.TimestampLayout)
				if !known {
					deps.Logf(
						"historical rollout gate waiting because terminal run %d has unknown finished_at %q",
						run.ID,
						finishedRaw,
					)
					return "waiting"
				}
				if finishedAt.After(asOfUTC) {
					return "waiting"
				}
			}
			switch run.Status {
			case RunSucceeded:
				continue
			case RunFailed, RunSkipped, RunCancelled, RunInterrupted:
				return "failed"
			default:
				return "waiting"
			}
		}
	}
	return "ready"
}

// rolloutHistoryExistedAt requires persisted temporal evidence that an older
// rollout occurrence had actually materialized by the recovered scheduler
// instant. Recovery-generated rows are created later and therefore cannot make
// a wholly missed older rollout retroactively compete with another policy.
func rolloutHistoryExistedAt(runs []Run, asOf time.Time, timestampLayout string) bool {
	asOfUTC := asOf.UTC()
	for _, run := range runs {
		for _, raw := range []string{run.CreatedAt, run.StartedAt, run.FinishedAt} {
			raw = strings.TrimSpace(raw)
			if raw == "" {
				continue
			}
			instant, ok := parsePolicyInstant(raw, timestampLayout)
			if ok && !instant.After(asOfUTC) {
				return true
			}
		}
	}
	return false
}
