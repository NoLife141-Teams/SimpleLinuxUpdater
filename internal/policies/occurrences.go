package policies

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"debian-updater/internal/servers"
)

// Occurrence is the durable admission of a schedule. It contains configuration
// and names only, never inventory credentials. Its batches and policy revision
// are immutable; live availability, overrides and no-run windows still apply.
type Occurrence struct {
	Policy          Policy         `json:"policy"`
	Key             string         `json:"key"`
	ScheduledForUTC string         `json:"scheduled_for_utc"`
	AcceptedAt      string         `json:"accepted_at"`
	Batches         []RolloutBatch `json:"batches"`
	BlockReason     string         `json:"block_reason,omitempty"`
	Closed          bool           `json:"closed"`
}

type OccurrenceStore interface {
	AcceptOccurrence(Occurrence) (Occurrence, error)
	ListOpenOccurrences() ([]Occurrence, error)
	CloseOccurrence(int64, string) error
}

func normalizedCanaryMode(mode string) string {
	if mode == CanaryExplicit {
		return CanaryExplicit
	}
	return CanaryAutomatic
}

func ensureOccurrenceSchema(db *sql.DB) error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS update_policy_occurrences (
 policy_id INTEGER NOT NULL, occurrence_key TEXT NOT NULL, scheduled_for_utc TEXT NOT NULL,
 policy_json TEXT NOT NULL, batches_json TEXT NOT NULL, block_reason TEXT NOT NULL DEFAULT '',
 accepted_at TEXT NOT NULL, closed INTEGER NOT NULL DEFAULT 0,
 PRIMARY KEY(policy_id, occurrence_key))`)
	return err
}

func (r *SQLiteRepository) AcceptOccurrence(o Occurrence) (Occurrence, error) {
	if o.Policy.ID <= 0 || o.Key == "" || o.ScheduledForUTC == "" {
		return Occurrence{}, errors.New("occurrence identity is required")
	}
	// Consumption and the complete member list commit together before any job
	// admission. INSERT OR IGNORE makes retries use the original UTC and revision.
	_, err := r.database().Exec(`INSERT INTO update_policy_occurrences
 (policy_id, occurrence_key, scheduled_for_utc, policy_json, batches_json, block_reason, accepted_at)
 VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(policy_id, occurrence_key) DO NOTHING`,
		o.Policy.ID, o.Key, o.ScheduledForUTC, r.json(o.Policy), r.json(o.Batches), o.BlockReason, r.now())
	if err != nil {
		return Occurrence{}, err
	}
	return scanOccurrence(r.database().QueryRow(`SELECT occurrence_key, scheduled_for_utc, policy_json, batches_json, block_reason, accepted_at, closed FROM update_policy_occurrences WHERE policy_id = ? AND occurrence_key = ?`, o.Policy.ID, o.Key))
}

func scanOccurrence(row interface{ Scan(...any) error }) (Occurrence, error) {
	var o Occurrence
	var p, b string
	var closed int
	if err := row.Scan(&o.Key, &o.ScheduledForUTC, &p, &b, &o.BlockReason, &o.AcceptedAt, &closed); err != nil {
		return o, err
	}
	if err := json.Unmarshal([]byte(p), &o.Policy); err != nil {
		return o, err
	}
	if err := json.Unmarshal([]byte(b), &o.Batches); err != nil {
		return o, err
	}
	o.Closed = closed != 0
	return o, nil
}

func (r *SQLiteRepository) ListOpenOccurrences() ([]Occurrence, error) {
	rows, err := r.database().Query(`SELECT occurrence_key, scheduled_for_utc, policy_json, batches_json, block_reason, accepted_at, closed FROM update_policy_occurrences WHERE closed = 0 ORDER BY scheduled_for_utc, policy_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Occurrence{}
	for rows.Next() {
		o, err := scanOccurrence(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
func (r *SQLiteRepository) CloseOccurrence(policyID int64, key string) error {
	_, err := r.database().Exec(`UPDATE update_policy_occurrences SET closed = 1 WHERE policy_id = ? AND occurrence_key = ?`, policyID, key)
	return err
}
func (r *SQLiteRepository) enrichOnceConsumption(policy *Policy) error {
	err := r.database().QueryRow(`SELECT accepted_at, scheduled_for_utc, block_reason FROM update_policy_occurrences WHERE policy_id = ? AND occurrence_key = 'once'`, policy.ID).Scan(&policy.OnceConsumedAt, &policy.OnceScheduledForUTC, &policy.OnceAdmissionReason)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	return err
}

func canaryUnavailable(policy Policy, inventory []servers.Server, overrides map[int64]map[string]bool) string {
	if policy.RolloutMode != RolloutCanaryWaves || policy.CanaryMode != CanaryExplicit {
		return ""
	}
	problems := []string{}
	for _, name := range policy.CanaryServers {
		reason := RunReasonMissing
		for _, server := range inventory {
			if strings.EqualFold(name, server.Name) {
				reason = policyPreviewExclusionReason(policy, server, overrides)
				break
			}
		}
		if reason != "" {
			problems = append(problems, fmt.Sprintf("%s (%s)", name, reason))
		}
	}
	if len(problems) == 0 {
		return ""
	}
	return RunReasonCanaryUnavailable + ": " + strings.Join(problems, ", ")
}

func newOccurrence(s *Service, policy Policy, origin time.Time, inventory []servers.Server, overrides map[int64]map[string]bool) Occurrence {
	deps := s.EnsureDeps()
	policy.MatchedServers = nil
	policy.OnceAdmissionReason = ""
	policy.OnceConsumedAt = ""
	policy.OnceScheduledForUTC = ""
	names := matchedServerNamesForPolicy(s, policy, inventory, overrides)
	reason := canaryUnavailable(policy, inventory, overrides)
	// Keep unavailable named canaries investigable even if no eligible target
	// remains. No replacement is selected and no batch is allowed to start.
	if reason != "" {
		names = NormalizeStringList(append(names, policy.CanaryServers...))
	}
	if len(names) == 0 {
		reason = RunReasonNoMatch
	}
	key := origin.UTC().Format(deps.TimestampLayout)
	if policy.CadenceKind == CadenceOnce {
		key = "once"
	}
	return Occurrence{Policy: policy, Key: key, ScheduledForUTC: origin.UTC().Format(deps.TimestampLayout), Batches: BuildRolloutBatches(policy, names), BlockReason: reason}
}

type schedulePlan struct {
	policy     Policy
	origin     time.Time
	batches    []RolloutBatch
	servers    map[string]servers.Server
	reasons    map[string]string
	occurrence *Occurrence
}

func (s *Service) occurrencePlan(o Occurrence, now time.Time, inventory []servers.Server, overrides map[int64]map[string]bool) (schedulePlan, bool, error) {
	origin, err := parseScheduleProjectionUTC(o.ScheduledForUTC, s.EnsureDeps().TimestampLayout)
	if err != nil {
		return schedulePlan{}, false, err
	}
	if o.Closed || origin.After(now) {
		return schedulePlan{}, false, nil
	}
	plan := schedulePlan{policy: o.Policy, origin: origin.In(now.Location()), batches: o.Batches, servers: map[string]servers.Server{}, reasons: map[string]string{}, occurrence: &o}
	for _, batch := range o.Batches {
		for _, name := range batch.Servers {
			server := servers.Server{Name: name}
			reason := RunReasonMissing
			for _, candidate := range inventory {
				if strings.EqualFold(candidate.Name, name) {
					server = candidate
					reason = ""
					break
				}
			}
			if server.Disabled {
				reason = RunReasonDisabled
			}
			if overrides[o.Policy.ID][server.Name] {
				reason = RunReasonOverride
			}
			if o.BlockReason != "" {
				reason = o.BlockReason
			}
			plan.servers[name] = server
			plan.reasons[name] = reason
		}
	}
	return plan, true, nil
}

// schedulePlans shares the dispatch/gate path between legacy stores and durable
// manifests. Existing custom repositories can keep their original contracts.
func (s *Service) schedulePlans(policies []Policy, inventory []servers.Server, overrides map[int64]map[string]bool, now time.Time, scopes map[RolloutRunScope][]Run) ([]schedulePlan, error) {
	deps := s.EnsureDeps()
	plans := []schedulePlan{}
	seen := map[RolloutRunScope]bool{}
	if deps.Occurrences != nil {
		open, err := deps.Occurrences.ListOpenOccurrences()
		if err != nil {
			return nil, err
		}
		for _, o := range open {
			plan, ok, err := s.occurrencePlan(o, now, inventory, overrides)
			if err != nil {
				return nil, err
			}
			if ok {
				plans = append(plans, plan)
				seen[RolloutRunScope{PolicyID: o.Policy.ID, ScheduledForUTC: o.ScheduledForUTC}] = true
			}
		}
	}
	for _, policy := range policies {
		if !policy.Enabled || policy.OnceConsumedAt != "" {
			continue
		}
		if deps.Occurrences != nil && (policy.CadenceKind == CadenceOnce || policy.RolloutMode == RolloutCanaryWaves) {
			canonical, ok := s.policySlotForDay(policy, now)
			if s.PolicyDueAt(policy, now) && ok && canonical.Equal(now) {
				o, err := deps.Occurrences.AcceptOccurrence(newOccurrence(s, policy, canonical, inventory, overrides))
				if err != nil {
					return nil, err
				}
				scope := RolloutRunScope{PolicyID: o.Policy.ID, ScheduledForUTC: o.ScheduledForUTC}
				if !seen[scope] {
					plan, ok, err := s.occurrencePlan(o, now, inventory, overrides)
					if err != nil {
						return nil, err
					}
					if ok {
						plans = append(plans, plan)
						seen[scope] = true
					}
				}
			}
			if policy.CadenceKind == CadenceOnce {
				continue
			}
			// Pre-migration runs have no manifest. Preserve their existing
			// conservative continuation, bounded by persisted origin evidence.
		}

		origin, due := s.rolloutScheduledSlot(policy, now)
		if !due {
			continue
		}
		origins := []time.Time{origin}
		if policy.RolloutMode == RolloutCanaryWaves {
			origins = s.unfinishedRolloutSlots(policy, origin, now, inventory, overrides, scopes)
		}
		for _, origin := range origins {
			stamp := CanonicalScheduledForUTC(origin, deps.TimestampLayout, deps.CurrentLocation)
			if seen[RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: stamp}] {
				continue
			}
			if deps.Occurrences != nil && policy.RolloutMode == RolloutCanaryWaves && len(scopes[RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: stamp}]) == 0 {
				continue
			}
			if policy.RolloutMode == RolloutCanaryWaves && origin.Before(now) && !rolloutOriginMatchesPolicy(policy, origin, scopes[RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: stamp}], deps.TimestampLayout) {
				continue
			}
			o := newOccurrence(s, policy, origin, inventory, overrides)
			if deps.Occurrences != nil && policy.RolloutMode == RolloutCanaryWaves {
				// An upgraded database cannot recover unrecorded pre-upgrade
				// targets. Freeze its first observed eligible set and retain every
				// historical predecessor as the gate before any remaining member.
				previous := scopes[RolloutRunScope{PolicyID: policy.ID, ScheduledForUTC: stamp}]
				priorNames := []string{}
				names := []string{}
				for _, batch := range o.Batches {
					names = append(names, batch.Servers...)
				}
				for _, run := range previous {
					priorNames = append(priorNames, run.ServerName)
					names = append(names, run.ServerName)
				}
				frozen := policy
				frozen.CanaryMode = CanaryExplicit
				frozen.CanaryServers = NormalizeStringList(priorNames)
				o.Batches = BuildRolloutBatches(frozen, NormalizeStringList(names))
				var err error
				o, err = deps.Occurrences.AcceptOccurrence(o)
				if err != nil {
					return nil, err
				}
			}
			plan, ok, err := s.occurrencePlan(o, now, inventory, overrides)
			if err != nil {
				return nil, err
			}
			if ok {
				if deps.Occurrences == nil || policy.RolloutMode != RolloutCanaryWaves {
					plan.occurrence = nil
				}
				plans = append(plans, plan)
			}
		}
	}
	return plans, nil
}

// RunRolloutPosition exposes the accepted batch position to Scheduled Run.
func RunRolloutPosition(policy Policy, serverName string) (string, int) {
	batches := policy.AcceptedBatches
	if len(batches) == 0 {
		batches = BuildRolloutBatches(policy, policy.MatchedServers)
	}
	for _, batch := range batches {
		if StringListContainsFold(batch.Servers, serverName) {
			return batch.Stage, batch.Index
		}
	}
	return "", 0
}

func (s *Service) projectAcceptedOccurrences(result map[string]ServerScheduleProjection, inventory []servers.Server, overrides map[int64]map[string]bool, blackouts []BlackoutWindow, now time.Time) error {
	deps := s.EnsureDeps()
	if deps.Occurrences == nil {
		return nil
	}
	open, err := deps.Occurrences.ListOpenOccurrences()
	if err != nil {
		return err
	}
	scopes := []RolloutRunScope{}
	for _, o := range open {
		scopes = append(scopes, RolloutRunScope{PolicyID: o.Policy.ID, ScheduledForUTC: o.ScheduledForUTC})
	}
	if len(scopes) == 0 {
		return nil
	}
	if deps.ListRolloutRuns == nil {
		return errors.New("policy occurrence history dependency is incomplete")
	}
	history, err := deps.ListRolloutRuns(scopes)
	if err != nil {
		return err
	}
	runs := map[string]Run{}
	for _, run := range history {
		runs[rolloutRunKey(run.PolicyID, run.ScheduledForUTC, run.ServerName)] = run
	}
	for _, o := range open {
		plan, ok, err := s.occurrencePlan(o, now, inventory, overrides)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		for index, batch := range plan.batches {
			gate := s.reconciledRolloutGateState(plan.policy.ID, o.ScheduledForUTC, plan.batches[:index], runs)
			if gate == "failed" || o.BlockReason != "" {
				continue
			}
			at := plan.origin.Add(time.Duration(batch.ReleaseDelayMinutes) * time.Minute)
			if at.Before(now) {
				at = now
			}
			if s.BlackoutApplies(at, blackouts) || s.BlackoutApplies(at, plan.policy.PolicyBlackouts) {
				continue
			}
			for _, name := range batch.Servers {
				if _, exists := runs[rolloutRunKey(plan.policy.ID, o.ScheduledForUTC, name)]; exists || plan.reasons[name] != "" {
					continue
				}
				current := result[name]
				if current.NextRun.ScheduledForUTC != "" {
					previous, err := parseScheduleProjectionUTC(current.NextRun.ScheduledForUTC, deps.TimestampLayout)
					if err == nil && !previous.After(at) {
						continue
					}
				}
				summary, reason := "Accepted rollout pending", ""
				if gate == "waiting" {
					summary, reason = "Wave waiting for preceding batch success", "rollout_waiting"
				}
				current.NextRun = ProjectedScheduleRun{State: ScheduleProjectionStateScheduled, PolicyName: plan.policy.Name, ScheduledForUTC: at.UTC().Format(deps.TimestampLayout), Status: ScheduleProjectionStateScheduled, Summary: summary, Reason: reason}
				result[name] = current
			}
		}
	}
	return nil
}
