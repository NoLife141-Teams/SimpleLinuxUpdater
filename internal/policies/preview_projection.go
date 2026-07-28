package policies

import (
	"fmt"
	"sort"
	"strings"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	"debian-updater/internal/servers"
)

func newPreviewResponse() PreviewResponse {
	return PreviewResponse{
		MatchedServers:      []PreviewServer{},
		ExcludedServers:     []PreviewServer{},
		DisabledByOverride:  []PreviewServer{},
		UpcomingOccurrences: []PreviewOccurrence{},
		ScheduleConflicts:   []PreviewConflict{},
		ValidationErrors:    []PreviewDiagnostic{},
		OperationalWarnings: []PreviewDiagnostic{},
		InformationalFacts:  []PreviewDiagnostic{},
		Warnings:            []string{},
	}
}

func addPreviewOperationalWarning(response *PreviewResponse, code, message string) {
	if response == nil || hasDiagnosticCode(response.OperationalWarnings, code) {
		return
	}
	response.OperationalWarnings = append(response.OperationalWarnings, PreviewDiagnostic{Code: code, Message: message})
	response.Warnings = append(response.Warnings, message)
}

func addPreviewFact(response *PreviewResponse, code, message string) {
	if response == nil {
		return
	}
	for _, fact := range response.InformationalFacts {
		if fact.Code == code && fact.Message == message {
			return
		}
	}
	response.InformationalFacts = append(response.InformationalFacts, PreviewDiagnostic{Code: code, Message: message})
}

func hasDiagnosticCode(items []PreviewDiagnostic, code string) bool {
	for _, item := range items {
		if item.Code == code {
			return true
		}
	}
	return false
}

func (s *Service) projectPolicyPreviewOccurrences(policy Policy, matched []PreviewServer, globalBlackouts []BlackoutWindow, response *PreviewResponse) {
	deps := s.EnsureDeps()
	loc, timezoneName := previewTimezone(deps)
	now := deps.Now().In(loc).Truncate(time.Minute)
	startDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc)
	addPreviewFact(response, "application_timezone", fmt.Sprintf("Occurrences use the canonical application timezone %s.", timezoneName))
	previousOffset := ""

	for dayOffset := 0; dayOffset < 370 && len(response.UpcomingOccurrences) < PreviewOccurrenceLimit; dayOffset++ {
		day := startDay.AddDate(0, 0, dayOffset)
		if !policyCadenceMatchesDay(policy, day) {
			continue
		}
		slot, occurrenceKind, ok := s.resolvePolicySlotForDay(policy, day)
		if !ok {
			if dayOffset > 0 || previewTimeIsUpcomingToday(policy.TimeLocal, now) {
				addPreviewFact(
					response,
					"dst_nonexistent_skipped",
					fmt.Sprintf("%s %s does not exist in %s; the scheduler skips that local occurrence.", day.Format("2006-01-02"), policy.TimeLocal, timezoneName),
				)
			}
			continue
		}
		if slot.Before(now) {
			continue
		}

		windows := applicablePreviewWindows(s, slot, globalBlackouts, policy.PolicyBlackouts)
		outcome := PreviewAdmissionAdmitted
		switch {
		case !policy.Enabled:
			outcome = PreviewAdmissionPolicyDisabled
		case len(matched) == 0:
			outcome = PreviewAdmissionNoMatchingServers
		case len(windows) > 0:
			outcome = PreviewAdmissionBlockedNoRun
			addPreviewOperationalWarning(response, "no_run_window", "One or more upcoming occurrences are blocked by an applicable no-run window.")
		}

		abbreviation, _ := slot.Zone()
		offset := timezoneOffset(slot)
		dstStatus := PreviewDSTUnambiguous
		canonicalChoice := PreviewCanonicalExact
		if occurrenceKind == apptimepkg.OccurrenceAmbiguous {
			dstStatus = PreviewDSTAmbiguous
			canonicalChoice = PreviewCanonicalEarlierFallback
			addPreviewFact(
				response,
				"dst_fallback_canonical_choice",
				fmt.Sprintf("%s %s is repeated in %s; the scheduler uses the earlier occurrence (%s %s).", day.Format("2006-01-02"), policy.TimeLocal, timezoneName, abbreviation, offset),
			)
		} else if previousOffset != "" && previousOffset != offset {
			dstStatus = PreviewDSTOffsetChanged
			addPreviewFact(
				response,
				"dst_offset_changed",
				fmt.Sprintf("The canonical timezone offset changes from %s to %s at the %s occurrence.", previousOffset, offset, slot.Format("2006-01-02 15:04")),
			)
		}
		response.UpcomingOccurrences = append(response.UpcomingOccurrences, PreviewOccurrence{
			LocalCivilTime:         slot.Format("2006-01-02 15:04"),
			Timezone:               timezoneName,
			Offset:                 offset,
			Abbreviation:           abbreviation,
			ScheduledForUTC:        slot.UTC().Format(deps.TimestampLayout),
			DSTStatus:              dstStatus,
			CanonicalChoice:        canonicalChoice,
			MatchedServerCount:     len(matched),
			ApplicableNoRunWindows: windows,
			AdmissionOutcome:       outcome,
		})
		previousOffset = offset
	}
}

func (s *Service) projectPolicyPreviewConflicts(
	policy Policy,
	matched []PreviewServer,
	serversSnapshot []servers.Server,
	overrides map[int64]map[string]bool,
	globalBlackouts []BlackoutWindow,
	response *PreviewResponse,
) error {
	deps := s.EnsureDeps()
	if response == nil || deps.ListPolicies == nil || len(matched) == 0 || len(response.UpcomingOccurrences) == 0 {
		return nil
	}
	policyList, err := deps.ListPolicies()
	if err != nil {
		return err
	}
	draftServers := previewMatchedServerNames(matched)
	loc, timezoneName := previewTimezone(deps)

	for _, competing := range policyList {
		if !competing.Enabled || (policy.ID != 0 && competing.ID == policy.ID) {
			continue
		}
		if err := s.NormalizePolicy(&competing); err != nil {
			continue
		}
		competingServers := matchedServerNamesForPolicy(s, competing, serversSnapshot, overrides)
		sharedServers := intersectServerNames(draftServers, competingServers)
		if len(sharedServers) == 0 {
			continue
		}

		conflict := PreviewConflict{
			PolicyID:          competing.ID,
			PolicyName:        competing.Name,
			OverlapKind:       previewConflictKind(draftServers, competingServers, sharedServers),
			SharedServers:     sharedServers,
			OccurrenceWindows: []PreviewConflictWindow{},
		}
		for _, occurrence := range response.UpcomingOccurrences {
			slotUTC, err := time.Parse(deps.TimestampLayout, occurrence.ScheduledForUTC)
			if err != nil {
				continue
			}
			slotLocal := slotUTC.In(loc).Truncate(time.Minute)
			if !s.PolicyDueAt(competing, slotLocal) {
				continue
			}
			canonicalSlot, _, ok := s.resolvePolicySlotForDay(
				competing,
				time.Date(slotLocal.Year(), slotLocal.Month(), slotLocal.Day(), 0, 0, 0, 0, loc),
			)
			if !ok || !canonicalSlot.UTC().Equal(slotUTC.UTC()) {
				continue
			}

			competingOutcome := PreviewAdmissionAdmitted
			if len(applicablePreviewWindows(s, canonicalSlot, globalBlackouts, competing.PolicyBlackouts)) > 0 {
				competingOutcome = PreviewAdmissionBlockedNoRun
			}
			effective := occurrence.AdmissionOutcome == PreviewAdmissionAdmitted &&
				competingOutcome == PreviewAdmissionAdmitted
			conflict.OccurrenceWindows = append(conflict.OccurrenceWindows, PreviewConflictWindow{
				LocalCivilTime:            occurrence.LocalCivilTime,
				Timezone:                  timezoneName,
				WindowStartUTC:            slotUTC.UTC().Format(deps.TimestampLayout),
				WindowEndUTC:              slotUTC.UTC().Add(time.Minute).Format(deps.TimestampLayout),
				DraftAdmissionOutcome:     occurrence.AdmissionOutcome,
				CompetingAdmissionOutcome: competingOutcome,
				Effective:                 effective,
			})
		}
		if len(conflict.OccurrenceWindows) == 0 {
			continue
		}

		effective := false
		suppressedByNoRun := false
		for _, window := range conflict.OccurrenceWindows {
			effective = effective || window.Effective
			suppressedByNoRun = suppressedByNoRun ||
				window.DraftAdmissionOutcome == PreviewAdmissionBlockedNoRun ||
				window.CompetingAdmissionOutcome == PreviewAdmissionBlockedNoRun
		}
		if effective {
			addPreviewOperationalWarning(
				response,
				"policy_schedule_overlap",
				"One or more enabled policies target shared servers during the same projected occurrence.",
			)
		}
		if suppressedByNoRun {
			addPreviewFact(
				response,
				"policy_schedule_overlap_suppressed_by_no_run",
				fmt.Sprintf("At least one projected overlap with %q is suppressed by an applicable no-run window.", competing.Name),
			)
		}
		response.ScheduleConflicts = append(response.ScheduleConflicts, conflict)
	}
	sort.Slice(response.ScheduleConflicts, func(i, j int) bool {
		left := strings.ToLower(response.ScheduleConflicts[i].PolicyName)
		right := strings.ToLower(response.ScheduleConflicts[j].PolicyName)
		if left == right {
			return response.ScheduleConflicts[i].PolicyID < response.ScheduleConflicts[j].PolicyID
		}
		return left < right
	})
	return nil
}

func previewMatchedServerNames(items []PreviewServer) []string {
	names := make([]string, 0, len(items))
	for _, item := range items {
		name := strings.TrimSpace(item.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return strings.ToLower(names[i]) < strings.ToLower(names[j])
	})
	return names
}

func intersectServerNames(left, right []string) []string {
	leftByKey := make(map[string]string, len(left))
	for _, name := range left {
		leftByKey[strings.ToLower(strings.TrimSpace(name))] = name
	}
	shared := make([]string, 0)
	for _, name := range right {
		if canonical, ok := leftByKey[strings.ToLower(strings.TrimSpace(name))]; ok {
			shared = append(shared, canonical)
		}
	}
	sort.Slice(shared, func(i, j int) bool {
		return strings.ToLower(shared[i]) < strings.ToLower(shared[j])
	})
	return shared
}

func previewConflictKind(draft, competing, shared []string) string {
	if len(shared) == len(draft) && len(shared) == len(competing) {
		return PreviewConflictFull
	}
	return PreviewConflictPartial
}

func previewTimezone(deps ServiceDeps) (*time.Location, string) {
	if deps.ApplicationTime != nil {
		interpretation := deps.ApplicationTime.Current()
		loc := interpretation.Location
		if loc == nil {
			loc = time.UTC
		}
		name := strings.TrimSpace(interpretation.ResolvedName)
		if name == "" {
			name = strings.TrimSpace(interpretation.DisplayName)
		}
		if name == "" {
			name = loc.String()
		}
		return loc, name
	}
	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.UTC
	}
	name := strings.TrimSpace(loc.String())
	if name == "" || strings.EqualFold(name, "Local") {
		name = "UTC"
	}
	return loc, name
}

func policyCadenceMatchesDay(policy Policy, day time.Time) bool {
	switch policy.CadenceKind {
	case CadenceDaily:
		return true
	case CadenceWeekly:
		return weekdayMatchesLocal(policy.Weekdays, day)
	default:
		return false
	}
}

func previewTimeIsUpcomingToday(raw string, now time.Time) bool {
	minutes, err := ParseTimeLocalMinutes(raw)
	if err != nil {
		return false
	}
	return minutes >= now.Hour()*60+now.Minute()
}

func applicablePreviewWindows(s *Service, slot time.Time, global, policy []BlackoutWindow) []CalendarBlockedWindow {
	windows := make([]CalendarBlockedWindow, 0)
	appendApplicable := func(source string, candidates []BlackoutWindow) {
		for _, window := range candidates {
			if !s.BlackoutApplies(slot, []BlackoutWindow{window}) {
				continue
			}
			windows = append(windows, CalendarBlockedWindow{
				Source:        source,
				Weekdays:      append([]string(nil), window.Weekdays...),
				StartTime:     window.StartTime,
				EndTime:       window.EndTime,
				Overnight:     blackoutWindowOvernight(window),
				AppliesToSlot: true,
			})
		}
	}
	appendApplicable(NoRunScopeGlobal, global)
	appendApplicable(NoRunScopePolicy, policy)
	return windows
}
