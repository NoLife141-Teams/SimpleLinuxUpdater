package policies

import (
	"time"

	"debian-updater/internal/servers"
)

// Prefer an incomplete persisted origin whose wave release horizon overlaps
// the latest occurrence. Never replay unstarted or obsolete historical work.
func (s *Service) unfinishedRolloutSlot(policy Policy, latest, now time.Time, inventory []servers.Server, overrides map[int64]map[string]bool, scopes map[RolloutRunScope][]Run) time.Time {
	deps := s.EnsureDeps()
	selected := latest
	horizon := s.rolloutHorizon(policy, inventory, overrides)
	for scope, runs := range scopes {
		if scope.PolicyID != policy.ID {
			continue
		}
		origin, err := time.Parse(deps.TimestampLayout, scope.ScheduledForUTC)
		if err != nil || !origin.Before(selected) || origin.After(now) || origin.Add(horizon).Before(latest) {
			continue
		}
		if !rolloutOriginMatchesPolicy(policy, origin, runs, deps.TimestampLayout) {
			continue
		}
		present := make(map[string]bool, len(runs))
		for _, run := range runs {
			present[run.ServerName] = true
		}
		for _, server := range inventory {
			if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) && !present[server.Name] {
				selected = origin.In(now.Location())
				break
			}
		}
	}
	return selected
}

func (r *SQLiteRepository) ListRolloutOrigins(policyIDs []int64) ([]RolloutRunScope, error) {
	origins := make([]RolloutRunScope, 0)
	for _, id := range policyIDs {
		rows, err := r.database().Query(`SELECT DISTINCT policy_id, scheduled_for_utc FROM update_policy_runs WHERE policy_id = ? ORDER BY scheduled_for_utc`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var scope RolloutRunScope
			if err := rows.Scan(&scope.PolicyID, &scope.ScheduledForUTC); err != nil {
				rows.Close()
				return nil, err
			}
			origins = append(origins, scope)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return origins, nil
}

func (s *Service) rolloutHorizon(policy Policy, inventory []servers.Server, overrides map[int64]map[string]bool) time.Duration {
	names := make([]string, 0)
	for _, server := range inventory {
		if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
			names = append(names, server.Name)
		}
	}
	batches := BuildRolloutBatches(policy, names)
	horizon := time.Duration(0)
	for _, batch := range batches {
		if delay := time.Duration(batch.ReleaseDelayMinutes) * time.Minute; delay > horizon {
			horizon = delay
		}
	}

	return horizon
}
