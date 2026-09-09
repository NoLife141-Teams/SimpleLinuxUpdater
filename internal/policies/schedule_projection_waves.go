package policies

import (
	"time"

	"debian-updater/internal/servers"
)

func (s *Service) nextWaveScheduleProjection(policy Policy, serverName string, inventory []servers.Server, overrides map[int64]map[string]bool, blackouts []BlackoutWindow, now time.Time, runs map[string]Run, scopes map[RolloutRunScope][]Run) (projectedScheduleCandidate, bool) {
	deps := s.EnsureDeps()
	names := make([]string, 0)
	for _, server := range inventory {
		if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
			names = append(names, server.Name)
		}
	}
	batches := BuildRolloutBatches(policy, names)
	batchIndex := -1
	for i, batch := range batches {
		for _, name := range batch.Servers {
			if name == serverName {
				batchIndex = i
			}
		}
	}
	if batchIndex < 0 {
		return projectedScheduleCandidate{}, false
	}

	origins := make([]time.Time, 0)
	if latest, due := s.rolloutScheduledSlot(policy, now); due {
		origins = s.unfinishedRolloutSlots(policy, latest, now, inventory, overrides, scopes)
	}
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	for offset := 0; offset <= 14; offset++ {
		origin, ok := s.policySlotForDay(policy, day.AddDate(0, 0, offset))
		if ok && !origin.Before(now) && s.PolicyDueAt(policy, origin) {
			origins = append(origins, origin)
		}
	}

	best := projectedScheduleCandidate{}
	for _, origin := range origins {
		stamp := CanonicalScheduledForUTC(origin, deps.TimestampLayout, deps.CurrentLocation)
		if origin.Before(now) && !rolloutOriginMatchesPolicy(policy, origin, scopes[RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: stamp}], deps.TimestampLayout) {
			continue
		}
		if _, exists := runs[rolloutRunKey(policy.ID, stamp, serverName)]; exists {
			continue
		}
		// A future wave cannot start if a known no-run window will skip
		// its canary or an earlier wave at that batch's release time.
		blockedPredecessor := false
		if !origin.Before(now) {
			for _, previous := range batches[:batchIndex] {
				at := origin.Add(time.Duration(previous.ReleaseDelayMinutes) * time.Minute)
				if s.BlackoutApplies(at, blackouts) || s.BlackoutApplies(at, policy.PolicyBlackouts) {
					blockedPredecessor = true
					break
				}
			}
		}
		if blockedPredecessor {
			continue
		}
		gate := rolloutGateState(policy.ID, stamp, batches[:batchIndex], runs)
		historyGate := s.rolloutHistoryGate(policy.ID, stamp, names, runs, false)
		if gate == "failed" || historyGate == "failed" {
			continue
		}
		release := origin.Add(time.Duration(batches[batchIndex].ReleaseDelayMinutes) * time.Minute)
		if release.Before(now) {
			release = now
		}
		if s.BlackoutApplies(release, blackouts) || s.BlackoutApplies(release, policy.PolicyBlackouts) {
			continue
		}
		candidate := projectedScheduleCandidate{
			policy: policy, scheduledLocal: release,
			scheduledUTC: CanonicalScheduledForUTC(release, deps.TimestampLayout, deps.CurrentLocation),
		}
		if gate == "waiting" || historyGate == "waiting" {
			candidate.summary = "Wave release subject to preceding batch success"
			candidate.reason = "rollout_waiting"
			if origin.Before(now) {
				candidate.summary = "Wave waiting for preceding batch success"
			}
		}
		if s.projectedScheduleBefore(candidate, best) {
			best = candidate
		}
	}
	return best, best.scheduledUTC != ""
}
