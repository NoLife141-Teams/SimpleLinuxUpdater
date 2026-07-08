package policies

import (
	"strings"
	"time"

	"debian-updater/internal/servers"
)

const (
	ScheduleProjectionStateScheduled = "scheduled"

	NoRunScopeGlobal = "global"
	NoRunScopePolicy = "policy"
)

type projectedScheduleCandidate struct {
	policy         Policy
	scheduledLocal time.Time
	scheduledUTC   string
}

func (s *Service) ProjectSchedule(req ScheduleProjectionRequest) (ScheduleProjection, error) {
	deps := s.EnsureDeps()
	now := req.Now
	if now.IsZero() {
		now = deps.Now()
	}
	serverList := append([]servers.Server(nil), req.Servers...)
	if len(serverList) == 0 && deps.SnapshotServers != nil {
		serverList = append([]servers.Server(nil), deps.SnapshotServers()...)
	}
	projection := ScheduleProjection{Servers: map[string]ServerScheduleProjection{}}
	for _, server := range serverList {
		projection.Servers[server.Name] = ServerScheduleProjection{}
	}

	policyList := []Policy{}
	if deps.ListPolicies != nil {
		loaded, err := deps.ListPolicies()
		if err != nil {
			return ScheduleProjection{}, err
		}
		policyList = loaded
	}
	overrides := map[int64]map[string]bool{}
	if deps.LoadOverrides != nil {
		loaded, err := deps.LoadOverrides()
		if err != nil {
			return ScheduleProjection{}, err
		}
		if loaded != nil {
			overrides = loaded
		}
	}
	globalBlackouts := []BlackoutWindow{}
	if deps.LoadGlobalBlackouts != nil {
		loaded, err := deps.LoadGlobalBlackouts()
		if err != nil {
			return ScheduleProjection{}, err
		}
		globalBlackouts = loaded
	}

	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.UTC
	}
	for _, server := range serverList {
		item := projection.Servers[server.Name]
		item.NoRun = s.projectNoRunWindow(server, policyList, overrides, globalBlackouts, now.In(loc))
		projection.Servers[server.Name] = item
	}

	runLimit := req.RunLimit
	if runLimit <= 0 {
		runLimit = DefaultRunsLimit
	}
	if deps.ListRuns != nil {
		runs, err := deps.ListRuns(runLimit)
		if err != nil {
			return ScheduleProjection{}, err
		}
		s.mergePersistedScheduleRuns(projection.Servers, runs, now)
	}
	s.mergeProjectedScheduleRuns(projection.Servers, serverList, policyList, overrides, globalBlackouts, now.In(loc))
	return projection, nil
}

func (s *Service) projectNoRunWindow(server servers.Server, policyList []Policy, overrides map[int64]map[string]bool, globalBlackouts []BlackoutWindow, localNow time.Time) NoRunWindow {
	if s.BlackoutApplies(localNow, globalBlackouts) {
		return NoRunWindow{Active: true, Scope: NoRunScopeGlobal, Reason: RunReasonBlackout}
	}
	for _, policy := range policyList {
		if !s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
			continue
		}
		if s.BlackoutApplies(localNow, policy.PolicyBlackouts) {
			return NoRunWindow{Active: true, Scope: NoRunScopePolicy, Reason: RunReasonBlackout, PolicyName: policy.Name}
		}
	}
	return NoRunWindow{}
}

func (s *Service) mergePersistedScheduleRuns(result map[string]ServerScheduleProjection, runs []Run, now time.Time) {
	deps := s.EnsureDeps()
	cutoff := now.UTC().Truncate(time.Minute)
	for _, run := range runs {
		scheduled, err := parseScheduleProjectionUTC(run.ScheduledForUTC, deps.TimestampLayout)
		if err != nil || scheduled.Before(cutoff) {
			continue
		}
		current := result[run.ServerName]
		if current.NextRun.ScheduledForUTC != "" {
			currentTime, currentErr := parseScheduleProjectionUTC(current.NextRun.ScheduledForUTC, deps.TimestampLayout)
			if currentErr == nil && !scheduled.Before(currentTime) {
				continue
			}
		}
		current.NextRun = ProjectedScheduleRun{
			State:           ScheduleProjectionStateScheduled,
			PolicyName:      run.PolicyName,
			ScheduledForUTC: run.ScheduledForUTC,
			Status:          run.Status,
			Reason:          run.Reason,
			Summary:         run.Summary,
		}
		result[run.ServerName] = current
	}
}

func (s *Service) mergeProjectedScheduleRuns(result map[string]ServerScheduleProjection, serverList []servers.Server, policyList []Policy, overrides map[int64]map[string]bool, globalBlackouts []BlackoutWindow, localNow time.Time) {
	deps := s.EnsureDeps()
	localNow = localNow.Truncate(time.Minute)
	projectedByServer := map[string]projectedScheduleCandidate{}
	for _, server := range serverList {
		for _, policy := range policyList {
			if !s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
				continue
			}
			slotLocal, ok := s.nextScheduleProjectionOccurrenceLocal(policy, localNow, globalBlackouts)
			if !ok {
				continue
			}
			projected := projectedScheduleCandidate{
				policy:         policy,
				scheduledLocal: slotLocal,
				scheduledUTC:   CanonicalScheduledForUTC(slotLocal, deps.TimestampLayout, deps.CurrentLocation),
			}
			if s.projectedScheduleBefore(projected, projectedByServer[server.Name]) {
				projectedByServer[server.Name] = projected
			}
		}
	}
	for serverName, projected := range projectedByServer {
		if strings.TrimSpace(projected.scheduledUTC) == "" {
			continue
		}
		current := result[serverName]
		if current.NextRun.ScheduledForUTC != "" {
			currentTime, err := parseScheduleProjectionUTC(current.NextRun.ScheduledForUTC, deps.TimestampLayout)
			if err == nil && !currentTime.After(projected.scheduledLocal.UTC()) {
				continue
			}
		}
		current.NextRun = ProjectedScheduleRun{
			State:           ScheduleProjectionStateScheduled,
			PolicyName:      projected.policy.Name,
			ScheduledForUTC: projected.scheduledUTC,
			Status:          ScheduleProjectionStateScheduled,
			Summary:         "Scheduled run pending",
		}
		result[serverName] = current
	}
}

func (s *Service) nextScheduleProjectionOccurrenceLocal(policy Policy, fromLocal time.Time, globalBlackouts []BlackoutWindow) (time.Time, bool) {
	deps := s.EnsureDeps()
	minutes, err := ParseTimeLocalMinutes(policy.TimeLocal)
	if err != nil {
		return time.Time{}, false
	}
	hour := minutes / 60
	minute := minutes % 60
	loc := fromLocal.Location()
	if loc == nil {
		loc = deps.CurrentLocation()
	}
	if loc == nil {
		loc = time.UTC
	}
	startDay := time.Date(fromLocal.Year(), fromLocal.Month(), fromLocal.Day(), 0, 0, 0, 0, loc)
	for offset := 0; offset <= 14; offset++ {
		day := startDay.AddDate(0, 0, offset)
		slot := time.Date(day.Year(), day.Month(), day.Day(), hour, minute, 0, 0, loc)
		if slot.Before(fromLocal) {
			continue
		}
		if !s.PolicyDueAt(policy, slot) {
			continue
		}
		if s.BlackoutApplies(slot, globalBlackouts) || s.BlackoutApplies(slot, policy.PolicyBlackouts) {
			continue
		}
		return slot, true
	}
	return time.Time{}, false
}

func (s *Service) projectedScheduleBefore(candidate, current projectedScheduleCandidate) bool {
	if current.scheduledUTC == "" {
		return true
	}
	candidateUTC := candidate.scheduledLocal.UTC()
	currentUTC := current.scheduledLocal.UTC()
	if !candidateUTC.Equal(currentUTC) {
		return candidateUTC.Before(currentUTC)
	}
	return s.ComparePolicyCandidates(
		ScheduledCandidate{Policy: candidate.policy, ScheduledForUTC: candidate.scheduledUTC},
		ScheduledCandidate{Policy: current.policy, ScheduledForUTC: current.scheduledUTC},
	)
}

func parseScheduleProjectionUTC(raw, layout string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if parsed, err := time.Parse(layout, raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339, raw)
}
