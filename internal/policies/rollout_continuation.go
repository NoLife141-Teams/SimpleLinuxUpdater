package policies

import (
	"errors"
	"sort"
	"time"

	"debian-updater/internal/servers"
)

// Include incomplete persisted origins whose wave release horizons overlap
// the latest occurrence, without displacing its new canary.
func (s *Service) unfinishedRolloutSlots(policy Policy, latest, now time.Time, inventory []servers.Server, overrides map[int64]map[string]bool, scopes map[RolloutRunScope][]Run) []time.Time {
	deps := s.EnsureDeps()
	slots := []time.Time{latest}
	horizon := s.rolloutHorizon(policy, inventory, overrides)
	for scope, runs := range scopes {
		if scope.PolicyID != policy.ID {
			continue
		}
		origin, err := time.Parse(deps.TimestampLayout, scope.ScheduledForUTC)
		if err != nil || !origin.Before(latest) || origin.After(now) || origin.Add(horizon).Before(latest) {
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
				slots = append(slots, origin.In(now.Location()))
				break
			}
		}
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].Before(slots[j]) })
	return slots
}

func (r *SQLiteRepository) ListRolloutOrigins(ranges []RolloutOriginRange) ([]RolloutRunScope, error) {
	origins := make([]RolloutRunScope, 0)
	for _, interval := range ranges {
		if interval.PolicyID <= 0 || interval.FromUTC == "" || interval.BeforeUTC == "" || interval.FromUTC >= interval.BeforeUTC {
			return nil, errors.New("rollout origin range requires policy ID and increasing UTC bounds")
		}
		rows, err := r.database().Query(rolloutOriginsQuery, interval.PolicyID, interval.FromUTC, interval.BeforeUTC)
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

const rolloutOriginsQuery = `SELECT DISTINCT policy_id, scheduled_for_utc
 FROM update_policy_runs
 WHERE policy_id = ? AND scheduled_for_utc >= ? AND scheduled_for_utc < ?
 ORDER BY scheduled_for_utc`
