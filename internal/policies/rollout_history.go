package policies

import (
	"errors"
	"time"

	"debian-updater/internal/servers"
)

// Dispatch and read projections use the same complete, occurrence-scoped
// history; a bounded dashboard recent-runs list cannot establish rollout gates.
func (s *Service) loadRolloutHistory(policies []Policy, serversSnapshot []servers.Server, overrides map[int64]map[string]bool, slotLocal time.Time) (map[string]Run, map[RolloutRunScope][]Run, error) {
	deps := s.EnsureDeps()
	rolloutScopes := make([]RolloutRunScope, 0)
	for _, policy := range policies {
		if !policy.Enabled || policy.RolloutMode != RolloutCanaryWaves {
			continue
		}
		rolloutSlot, rolloutDue := s.rolloutScheduledSlot(policy, slotLocal)
		if !rolloutDue {
			continue
		}
		rolloutScopes = append(rolloutScopes, RolloutRunScope{
			PolicyID:        policy.ID,
			ScheduledForUTC: CanonicalScheduledForUTC(rolloutSlot, deps.TimestampLayout, deps.CurrentLocation),
		})
	}
	// Include persisted origins so a newer cadence occurrence cannot hide an
	// unfinished wave. History remains scoped to enabled wave policies.
	if deps.ListRolloutOrigins != nil {
		ranges := make([]RolloutOriginRange, 0)
		for _, policy := range policies {
			if policy.Enabled && policy.RolloutMode == RolloutCanaryWaves {
				latest, due := s.rolloutScheduledSlot(policy, slotLocal)
				horizon := s.rolloutHorizon(policy, serversSnapshot, overrides)
				if !due || horizon <= 0 {
					continue
				}
				ranges = append(ranges, RolloutOriginRange{PolicyID: policy.ID, FromUTC: CanonicalScheduledForUTC(latest.Add(-horizon), deps.TimestampLayout, deps.CurrentLocation), BeforeUTC: CanonicalScheduledForUTC(latest, deps.TimestampLayout, deps.CurrentLocation)})
			}
		}
		origins, originErr := deps.ListRolloutOrigins(ranges)
		if originErr != nil {
			return nil, nil, originErr
		}
		for _, scope := range origins {
			for _, policy := range policies {
				if policy.ID != scope.PolicyID || !policy.Enabled || policy.RolloutMode != RolloutCanaryWaves {
					continue
				}
				latest, due := s.rolloutScheduledSlot(policy, slotLocal)
				origin, parseErr := time.Parse(deps.TimestampLayout, scope.ScheduledForUTC)
				if !due || parseErr != nil || !origin.Before(latest) || origin.Add(s.rolloutHorizon(policy, serversSnapshot, overrides)).Before(latest) {
					continue
				}
				rolloutScopes = append(rolloutScopes, scope)
			}
		}
	}
	if len(rolloutScopes) > 0 && deps.ListRolloutRuns == nil {
		return nil, nil, errors.New("policy rollout history dependency is incomplete")
	}
	rolloutRuns := []Run{}
	if len(rolloutScopes) > 0 {
		var err error
		rolloutRuns, err = deps.ListRolloutRuns(rolloutScopes)
		if err != nil {
			return nil, nil, err
		}
	}
	runByKey := make(map[string]Run, len(rolloutRuns))
	runsByScope := make(map[RolloutRunScope][]Run)
	for _, run := range rolloutRuns {
		runByKey[rolloutRunKey(run.PolicyID, run.ScheduledForUTC, run.ServerName)] = run
		scope := RolloutRunScope{PolicyID: run.PolicyID, ScheduledForUTC: run.ScheduledForUTC}
		runsByScope[scope] = append(runsByScope[scope], run)
	}

	return runByKey, runsByScope, nil
}
