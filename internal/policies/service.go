package policies

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"debian-updater/internal/servers"
)

var ErrPolicyNotFound = errors.New("policy not found")

type ServiceDeps struct {
	ListPolicies             func() ([]Policy, error)
	LoadOverrides            func() (map[int64]map[string]bool, error)
	LoadGlobalBlackouts      func() ([]BlackoutWindow, error)
	SnapshotServers          func() []servers.Server
	CurrentStatusSnapshot    func(string) *servers.ServerStatus
	CreateRun                func(Run) (Run, bool, error)
	ExecuteRun               func(Run, Policy, servers.Server)
	AuditWithActor           func(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any)
	CurrentLocation          func() *time.Location
	CurrentMaintenanceActive func() bool
	JobTimestampNow          func() string
	MarkInterruptedRuns      func() error
	TryBackupRestoreReadLock func() bool
	UnlockBackupRestoreRead  func()
	Now                      func() time.Time
	Logf                     func(string, ...any)
	StatusInProgress         func(string) bool
	TimestampLayout          string
}

type ScheduleRequest struct {
	Now               time.Time
	MaintenanceActive bool
}

type MatchContext struct {
	Overrides map[int64]map[string]bool
}

type SchedulerOptions struct {
	TickInterval time.Duration
}

type ScheduledCandidate struct {
	Policy          Policy
	Server          servers.Server
	ScheduledForUTC string
}

type Service struct {
	deps          ServiceDeps
	schedulerOnce sync.Once
	tickMu        sync.Mutex
	missedTickMu  sync.Mutex
	missedTicks   map[string]time.Time
}

func NewService(deps ServiceDeps) *Service {
	return &Service{
		deps:        deps.withDefaults(),
		missedTicks: map[string]time.Time{},
	}
}

func (s *Service) EnsureDeps() ServiceDeps {
	if s == nil {
		return ServiceDeps{}.withDefaults()
	}
	return s.deps.withDefaults()
}

func (d ServiceDeps) withDefaults() ServiceDeps {
	if d.CurrentLocation == nil {
		d.CurrentLocation = func() *time.Location { return time.Local }
	}
	if d.CurrentMaintenanceActive == nil {
		d.CurrentMaintenanceActive = func() bool { return false }
	}
	if d.JobTimestampNow == nil {
		d.JobTimestampNow = func() string { return time.Now().UTC().Format(DefaultTimestampLayout) }
	}
	if d.TryBackupRestoreReadLock == nil {
		d.TryBackupRestoreReadLock = func() bool { return true }
	}
	if d.UnlockBackupRestoreRead == nil {
		d.UnlockBackupRestoreRead = func() {}
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logf == nil {
		d.Logf = log.Printf
	}
	if d.StatusInProgress == nil {
		d.StatusInProgress = defaultStatusInProgress
	}
	if strings.TrimSpace(d.TimestampLayout) == "" {
		d.TimestampLayout = DefaultTimestampLayout
	}
	return d
}

func (o SchedulerOptions) WithDefaults() SchedulerOptions {
	if o.TickInterval <= 0 {
		o.TickInterval = DefaultSchedulerTickInterval
	}
	return o
}

func (s *Service) StartScheduler(ctx context.Context, options SchedulerOptions) {
	deps := s.EnsureDeps()
	options = options.WithDefaults()
	s.schedulerOnce.Do(func() {
		if deps.MarkInterruptedRuns != nil {
			if err := deps.MarkInterruptedRuns(); err != nil {
				deps.Logf("failed to mark interrupted policy runs: %v", err)
			}
		}
		if err := s.ProcessDue(deps.Now()); err != nil {
			deps.Logf("scheduled policy tick failed: %v", err)
		}
		go func() {
			ticker := time.NewTicker(options.TickInterval)
			defer ticker.Stop()
			for {
				select {
				case tick := <-ticker.C:
					if err := s.ProcessDue(tick); err != nil {
						deps.Logf("scheduled policy tick failed: %v", err)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

func (s *Service) NormalizePolicy(policy *Policy) error {
	if policy == nil {
		return errors.New("policy is required")
	}
	policy.Name = truncateString(policy.Name, 255)
	policy.Name = strings.TrimSpace(policy.Name)
	if policy.Name == "" {
		return errors.New("name is required")
	}
	policy.TargetTag = strings.TrimSpace(policy.TargetTag)
	policy.IncludeTags = NormalizeStringList(policy.IncludeTags)
	policy.ExcludeTags = NormalizeStringList(policy.ExcludeTags)
	policy.TargetServers = NormalizeStringList(policy.TargetServers)
	if policy.TargetTag == "" && len(policy.IncludeTags) == 0 && len(policy.TargetServers) == 0 {
		return errors.New("at least one target tag, included tag, or explicit server is required")
	}
	switch strings.ToLower(strings.TrimSpace(policy.PackageScope)) {
	case PackageScopeSecurity:
		policy.PackageScope = PackageScopeSecurity
	case PackageScopeFull:
		policy.PackageScope = PackageScopeFull
	default:
		return errors.New("package_scope must be 'security' or 'full'")
	}
	switch strings.ToLower(strings.TrimSpace(policy.UpgradeMode)) {
	case "", UpgradeModeStandard:
		policy.UpgradeMode = UpgradeModeStandard
	case UpgradeModeFull:
		policy.UpgradeMode = UpgradeModeFull
	default:
		return errors.New("upgrade_mode must be 'standard' or 'full'")
	}
	switch strings.ToLower(strings.TrimSpace(policy.ExecutionMode)) {
	case ExecutionScanOnly:
		policy.ExecutionMode = ExecutionScanOnly
	case ExecutionApprovalRequired:
		policy.ExecutionMode = ExecutionApprovalRequired
	case ExecutionAutoApply:
		policy.ExecutionMode = ExecutionAutoApply
	default:
		return errors.New("execution_mode must be 'scan_only', 'approval_required', or 'auto_apply'")
	}
	switch strings.ToLower(strings.TrimSpace(policy.CadenceKind)) {
	case CadenceDaily:
		policy.CadenceKind = CadenceDaily
	case CadenceWeekly:
		policy.CadenceKind = CadenceWeekly
	default:
		return errors.New("cadence_kind must be 'daily' or 'weekly'")
	}
	timeLocal, err := NormalizeTimeLocal(policy.TimeLocal)
	if err != nil {
		return err
	}
	policy.TimeLocal = timeLocal
	weekdays, err := NormalizeWeekdays(policy.Weekdays)
	if err != nil {
		return err
	}
	if policy.CadenceKind == CadenceWeekly && len(weekdays) == 0 {
		return errors.New("weekly policies require at least one weekday")
	}
	if policy.CadenceKind == CadenceDaily {
		weekdays = []string{}
	}
	policy.Weekdays = weekdays
	policyBlackouts, err := NormalizeBlackouts(policy.PolicyBlackouts)
	if err != nil {
		return err
	}
	policy.PolicyBlackouts = policyBlackouts
	if policy.ExecutionMode == ExecutionApprovalRequired {
		if policy.ApprovalTimeoutMinutes <= 0 {
			policy.ApprovalTimeoutMinutes = DefaultApprovalTimeoutMinutes
		}
	} else {
		policy.ApprovalTimeoutMinutes = 0
	}
	return nil
}

func (s *Service) PolicyMatchesServer(policy Policy, server servers.Server, ctx MatchContext) bool {
	if !policy.Enabled {
		return false
	}
	if len(policy.ExcludeTags) > 0 && ServerHasAnyTag(server, policy.ExcludeTags) {
		return false
	}
	if perPolicy := ctx.Overrides[policy.ID]; perPolicy != nil && perPolicy[server.Name] {
		return false
	}
	if StringListContainsFold(policy.TargetServers, server.Name) {
		return true
	}
	if strings.TrimSpace(policy.TargetTag) != "" && ServerHasTag(server, policy.TargetTag) {
		return true
	}
	if len(policy.IncludeTags) > 0 && ServerHasAnyTag(server, policy.IncludeTags) {
		return true
	}
	if strings.TrimSpace(policy.TargetTag) == "" && len(policy.IncludeTags) == 0 && len(policy.TargetServers) == 0 {
		return true
	}
	return false
}

func (s *Service) EnrichPoliciesWithMatches(policies []Policy) []Policy {
	deps := s.EnsureDeps()
	if deps.SnapshotServers == nil || deps.LoadOverrides == nil {
		return policies
	}
	serversSnapshot := deps.SnapshotServers()
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return policies
	}
	for i := range policies {
		matched := make([]string, 0)
		for _, server := range serversSnapshot {
			if s.PolicyMatchesServer(policies[i], server, MatchContext{Overrides: overrides}) {
				matched = append(matched, server.Name)
			}
		}
		sort.Strings(matched)
		policies[i].MatchedServers = matched
	}
	return policies
}

func (s *Service) PreviewPolicy(policy Policy) (PreviewResponse, error) {
	if err := s.NormalizePolicy(&policy); err != nil {
		return PreviewResponse{}, err
	}
	deps := s.EnsureDeps()
	serversSnapshot := []servers.Server{}
	if deps.SnapshotServers != nil {
		serversSnapshot = deps.SnapshotServers()
	}
	overrides := map[int64]map[string]bool{}
	if deps.LoadOverrides != nil {
		loaded, err := deps.LoadOverrides()
		if err != nil {
			return PreviewResponse{}, err
		}
		overrides = loaded
	}

	response := PreviewResponse{
		MatchedServers:     []PreviewServer{},
		ExcludedServers:    []PreviewServer{},
		DisabledByOverride: []PreviewServer{},
		Warnings:           []string{},
	}
	if !policy.Enabled {
		response.Warnings = append(response.Warnings, "Policy is disabled; matched servers will not run until it is enabled.")
	}

	foundServers := make(map[string]struct{}, len(serversSnapshot))
	for _, server := range serversSnapshot {
		foundServers[strings.ToLower(strings.TrimSpace(server.Name))] = struct{}{}
		reason := policyPreviewExclusionReason(policy, server, overrides)
		item := policyPreviewServer(server, reason)
		switch reason {
		case "":
			response.MatchedServers = append(response.MatchedServers, item)
		case "disabled_by_override":
			response.DisabledByOverride = append(response.DisabledByOverride, item)
		default:
			response.ExcludedServers = append(response.ExcludedServers, item)
		}
	}
	sortPreviewServers(response.MatchedServers)
	sortPreviewServers(response.ExcludedServers)
	sortPreviewServers(response.DisabledByOverride)

	for _, name := range policy.TargetServers {
		if _, ok := foundServers[strings.ToLower(strings.TrimSpace(name))]; !ok {
			response.Warnings = append(response.Warnings, fmt.Sprintf("Explicit server %q is not in the current inventory.", name))
		}
	}
	if len(response.MatchedServers) == 0 {
		response.Warnings = append(response.Warnings, "No current server would be targeted by this policy.")
	}
	return response, nil
}

func (s *Service) Calendar(options CalendarOptions) (CalendarResponse, error) {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil || deps.LoadOverrides == nil || deps.LoadGlobalBlackouts == nil {
		return CalendarResponse{}, errors.New("policy service dependencies are incomplete")
	}
	days := options.Days
	if days <= 0 {
		days = 14
	}
	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.Local
	}
	now := deps.Now()
	start := options.Start
	if start.IsZero() {
		start = now
	}
	start = start.In(loc)
	startDate := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, loc)
	endDate := startDate.AddDate(0, 0, days-1)

	policies, err := deps.ListPolicies()
	if err != nil {
		return CalendarResponse{}, err
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return CalendarResponse{}, err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return CalendarResponse{}, err
	}
	serversSnapshot := []servers.Server{}
	if deps.SnapshotServers != nil {
		serversSnapshot = deps.SnapshotServers()
	}

	response := CalendarResponse{
		Days:        days,
		StartDate:   startDate.Format("2006-01-02"),
		EndDate:     endDate.Format("2006-01-02"),
		GeneratedAt: now.UTC().Format(deps.TimestampLayout),
		Policies:    []CalendarPolicy{},
	}
	foundPolicy := options.PolicyID <= 0
	for _, policy := range policies {
		if options.PolicyID > 0 && policy.ID != options.PolicyID {
			continue
		}
		foundPolicy = true
		matchedServers := matchedServerNamesForPolicy(s, policy, serversSnapshot, overrides)
		calendarPolicy := CalendarPolicy{
			ID:             policy.ID,
			Name:           policy.Name,
			Enabled:        policy.Enabled,
			CadenceKind:    policy.CadenceKind,
			TimeLocal:      policy.TimeLocal,
			Weekdays:       append([]string(nil), policy.Weekdays...),
			MatchedServers: matchedServers,
			Days:           make([]CalendarDay, 0, days),
		}
		for offset := 0; offset < days; offset++ {
			dayStart := startDate.AddDate(0, 0, offset)
			slotLocal := policySlotForDay(policy, dayStart)
			day := CalendarDay{
				Date:           dayStart.Format("2006-01-02"),
				Weekday:        weekdayToken(dayStart),
				TimezoneOffset: timezoneOffset(dayStart),
				AllowedSlots:   []CalendarSlot{},
				BlockedWindows: []CalendarBlockedWindow{},
			}
			if policy.Enabled && s.PolicyDueAt(policy, slotLocal) {
				blockedByGlobal := s.BlackoutApplies(slotLocal, globalBlackouts)
				blockedByPolicy := s.BlackoutApplies(slotLocal, policy.PolicyBlackouts)
				if !blockedByGlobal && !blockedByPolicy {
					day.AllowedSlots = append(day.AllowedSlots, CalendarSlot{
						TimeLocal:       policy.TimeLocal,
						ScheduledForUTC: CanonicalScheduledForUTC(slotLocal, deps.TimestampLayout, deps.CurrentLocation),
						TimezoneOffset:  timezoneOffset(slotLocal),
						ExecutionMode:   policy.ExecutionMode,
						PackageScope:    policy.PackageScope,
						UpgradeMode:     policy.UpgradeMode,
						MatchedServers:  append([]string(nil), matchedServers...),
					})
				} else {
					if blockedByGlobal {
						day.BlockedReasons = append(day.BlockedReasons, "global_blackout")
					}
					if blockedByPolicy {
						day.BlockedReasons = append(day.BlockedReasons, "policy_blackout")
					}
				}
			}
			day.BlockedWindows = append(day.BlockedWindows, calendarWindowsForDay(s, dayStart, slotLocal, globalBlackouts, "global")...)
			day.BlockedWindows = append(day.BlockedWindows, calendarWindowsForDay(s, dayStart, slotLocal, policy.PolicyBlackouts, "policy")...)
			calendarPolicy.Days = append(calendarPolicy.Days, day)
		}
		response.Policies = append(response.Policies, calendarPolicy)
	}
	if !foundPolicy {
		return CalendarResponse{}, ErrPolicyNotFound
	}
	return response, nil
}

func (s *Service) PolicyDueAt(policy Policy, slotLocal time.Time) bool {
	minutes, err := ParseTimeLocalMinutes(policy.TimeLocal)
	if err != nil {
		return false
	}
	if slotLocal.Hour()*60+slotLocal.Minute() != minutes {
		return false
	}
	switch policy.CadenceKind {
	case CadenceDaily:
		return true
	case CadenceWeekly:
		return weekdayMatchesLocal(policy.Weekdays, slotLocal)
	default:
		return false
	}
}

func matchedServerNamesForPolicy(s *Service, policy Policy, serversSnapshot []servers.Server, overrides map[int64]map[string]bool) []string {
	matched := make([]string, 0)
	for _, server := range serversSnapshot {
		if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
			matched = append(matched, server.Name)
		}
	}
	sort.Strings(matched)
	return matched
}

func policySlotForDay(policy Policy, dayStart time.Time) time.Time {
	minutes, err := ParseTimeLocalMinutes(policy.TimeLocal)
	if err != nil {
		minutes = 0
	}
	return time.Date(dayStart.Year(), dayStart.Month(), dayStart.Day(), minutes/60, minutes%60, 0, 0, dayStart.Location())
}

func weekdayToken(t time.Time) string {
	token, _ := NormalizeWeekdayToken(t.Weekday().String())
	return token
}

func timezoneOffset(t time.Time) string {
	_, offset := t.Zone()
	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}
	hours := offset / 3600
	minutes := (offset % 3600) / 60
	return fmt.Sprintf("%s%02d:%02d", sign, hours, minutes)
}

func calendarWindowsForDay(s *Service, dayStart time.Time, slotLocal time.Time, windows []BlackoutWindow, source string) []CalendarBlockedWindow {
	if len(windows) == 0 {
		return []CalendarBlockedWindow{}
	}
	out := make([]CalendarBlockedWindow, 0, len(windows))
	for _, window := range windows {
		if !blackoutWindowTouchesDate(window, dayStart) {
			continue
		}
		out = append(out, CalendarBlockedWindow{
			Source:        source,
			Weekdays:      append([]string(nil), window.Weekdays...),
			StartTime:     window.StartTime,
			EndTime:       window.EndTime,
			Overnight:     blackoutWindowOvernight(window),
			AppliesToSlot: s.BlackoutApplies(slotLocal, []BlackoutWindow{window}),
		})
	}
	return out
}

func blackoutWindowTouchesDate(window BlackoutWindow, dayStart time.Time) bool {
	startMinutes, startErr := ParseTimeLocalMinutes(window.StartTime)
	endMinutes, endErr := ParseTimeLocalMinutes(window.EndTime)
	if startErr != nil || endErr != nil || startMinutes == endMinutes {
		return false
	}
	currentWeekday := weekdayToken(dayStart)
	for _, weekday := range window.Weekdays {
		if startMinutes < endMinutes {
			if weekday == currentWeekday {
				return true
			}
			continue
		}
		if weekday == currentWeekday || NextWeekdayToken(weekday) == currentWeekday {
			return true
		}
	}
	return false
}

func blackoutWindowOvernight(window BlackoutWindow) bool {
	startMinutes, startErr := ParseTimeLocalMinutes(window.StartTime)
	endMinutes, endErr := ParseTimeLocalMinutes(window.EndTime)
	return startErr == nil && endErr == nil && startMinutes > endMinutes
}

func policyPreviewExclusionReason(policy Policy, server servers.Server, overrides map[int64]map[string]bool) string {
	if len(policy.ExcludeTags) > 0 && ServerHasAnyTag(server, policy.ExcludeTags) {
		return "excluded_tag"
	}
	if !policyTargetsServer(policy, server) {
		return "no_target_match"
	}
	if perPolicy := overrides[policy.ID]; perPolicy != nil && perPolicy[server.Name] {
		return "disabled_by_override"
	}
	return ""
}

func policyTargetsServer(policy Policy, server servers.Server) bool {
	if StringListContainsFold(policy.TargetServers, server.Name) {
		return true
	}
	if strings.TrimSpace(policy.TargetTag) != "" && ServerHasTag(server, policy.TargetTag) {
		return true
	}
	return len(policy.IncludeTags) > 0 && ServerHasAnyTag(server, policy.IncludeTags)
}

func policyPreviewServer(server servers.Server, reason string) PreviewServer {
	tags := NormalizeStringList(server.Tags)
	return PreviewServer{
		Name:   server.Name,
		Tags:   tags,
		Reason: reason,
	}
}

func sortPreviewServers(items []PreviewServer) {
	sort.Slice(items, func(i, j int) bool {
		return strings.ToLower(items[i].Name) < strings.ToLower(items[j].Name)
	})
}

func (s *Service) BlackoutApplies(slotLocal time.Time, windows []BlackoutWindow) bool {
	if len(windows) == 0 {
		return false
	}
	minutesOfDay := slotLocal.Hour()*60 + slotLocal.Minute()
	currentWeekday, _ := NormalizeWeekdayToken(slotLocal.Weekday().String())
	for _, window := range windows {
		startMinutes, startErr := ParseTimeLocalMinutes(window.StartTime)
		endMinutes, endErr := ParseTimeLocalMinutes(window.EndTime)
		if startErr != nil || endErr != nil || startMinutes == endMinutes {
			continue
		}
		for _, weekday := range window.Weekdays {
			if startMinutes < endMinutes {
				if weekday == currentWeekday && minutesOfDay >= startMinutes && minutesOfDay < endMinutes {
					return true
				}
				continue
			}
			if weekday == currentWeekday && minutesOfDay >= startMinutes {
				return true
			}
			if NextWeekdayToken(weekday) == currentWeekday && minutesOfDay < endMinutes {
				return true
			}
		}
	}
	return false
}

func (s *Service) CandidatePriority(policy Policy) [3]int {
	modeRank := 99
	switch policy.ExecutionMode {
	case ExecutionApprovalRequired:
		modeRank = 0
	case ExecutionAutoApply:
		modeRank = 1
	case ExecutionScanOnly:
		modeRank = 2
	}
	scopeRank := 1
	if policy.PackageScope == PackageScopeFull {
		scopeRank = 0
	}
	return [3]int{modeRank, scopeRank, int(policy.ID)}
}

func (s *Service) ComparePolicyCandidates(a, b ScheduledCandidate) bool {
	pa := s.CandidatePriority(a.Policy)
	pb := s.CandidatePriority(b.Policy)
	for i := 0; i < len(pa); i++ {
		if pa[i] == pb[i] {
			continue
		}
		return pa[i] < pb[i]
	}
	if a.Policy.CreatedAt == b.Policy.CreatedAt {
		return a.Policy.ID < b.Policy.ID
	}
	return a.Policy.CreatedAt < b.Policy.CreatedAt
}

func (s *Service) CreateSkippedRun(policy Policy, serverName, scheduledForUTC, reason, summary string) {
	deps := s.EnsureDeps()
	if deps.CreateRun == nil {
		return
	}
	run := Run{
		PolicyID:        policy.ID,
		PolicyName:      policy.Name,
		ServerName:      serverName,
		ScheduledForUTC: scheduledForUTC,
		ExecutionMode:   policy.ExecutionMode,
		PackageScope:    policy.PackageScope,
		UpgradeMode:     policy.UpgradeMode,
		Status:          RunSkipped,
		Reason:          reason,
		Summary:         summary,
		ResultJSON:      "{}",
		FinishedAt:      deps.JobTimestampNow(),
	}
	createdRun, inserted, err := deps.CreateRun(run)
	if err != nil || !inserted {
		return
	}
	if deps.AuditWithActor == nil {
		return
	}
	deps.AuditWithActor(
		"system",
		"",
		"schedule.run.skipped",
		"server",
		serverName,
		"ignored",
		summary,
		map[string]any{
			"policy_id":         policy.ID,
			"policy_name":       policy.Name,
			"reason":            reason,
			"scheduled_for_utc": scheduledForUTC,
			"run_id":            createdRun.ID,
		},
	)
}

func MissedTickKey(t time.Time, layout string) string {
	if strings.TrimSpace(layout) == "" {
		layout = DefaultTimestampLayout
	}
	return t.UTC().Truncate(time.Minute).Format(layout)
}

func (s *Service) RememberMissedTick(now time.Time) {
	deps := s.EnsureDeps()
	slotUTC := now.UTC().Truncate(time.Minute)
	key := MissedTickKey(slotUTC, deps.TimestampLayout)
	s.missedTickMu.Lock()
	defer s.missedTickMu.Unlock()
	if s.missedTicks == nil {
		s.missedTicks = map[string]time.Time{}
	}
	if _, exists := s.missedTicks[key]; exists {
		return
	}
	s.missedTicks[key] = slotUTC
}

func (s *Service) PendingMissedTicks() []time.Time {
	s.missedTickMu.Lock()
	defer s.missedTickMu.Unlock()
	ticks := make([]time.Time, 0, len(s.missedTicks))
	for _, tick := range s.missedTicks {
		ticks = append(ticks, tick)
	}
	sort.Slice(ticks, func(i, j int) bool {
		return ticks[i].Before(ticks[j])
	})
	return ticks
}

func (s *Service) ForgetMissedTick(tick time.Time) {
	deps := s.EnsureDeps()
	s.missedTickMu.Lock()
	defer s.missedTickMu.Unlock()
	delete(s.missedTicks, MissedTickKey(tick, deps.TimestampLayout))
}

func (s *Service) ResetMissedTicksForTest() {
	s.missedTickMu.Lock()
	defer s.missedTickMu.Unlock()
	s.missedTicks = map[string]time.Time{}
}

func (s *Service) ProcessDueSlot(req ScheduleRequest) error {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil || deps.LoadOverrides == nil || deps.LoadGlobalBlackouts == nil || deps.SnapshotServers == nil || deps.CurrentStatusSnapshot == nil || deps.CreateRun == nil || deps.ExecuteRun == nil {
		return errors.New("policy service dependencies are incomplete")
	}
	policies, err := deps.ListPolicies()
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return nil
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return err
	}
	slotLocal := req.Now.In(deps.CurrentLocation()).Truncate(time.Minute)
	scheduledForUTC := CanonicalScheduledForUTC(slotLocal, deps.TimestampLayout, deps.CurrentLocation)
	serversSnapshot := deps.SnapshotServers()

	candidatesByServer := make(map[string][]ScheduledCandidate)
	for _, policy := range policies {
		if !policy.Enabled || !s.PolicyDueAt(policy, slotLocal) {
			continue
		}
		for _, server := range serversSnapshot {
			if !s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
				continue
			}
			if req.MaintenanceActive {
				s.CreateSkippedRun(policy, server.Name, scheduledForUTC, RunReasonMaintenance, "Maintenance mode active; scheduled run skipped")
				continue
			}
			if s.BlackoutApplies(slotLocal, globalBlackouts) || s.BlackoutApplies(slotLocal, policy.PolicyBlackouts) {
				s.CreateSkippedRun(policy, server.Name, scheduledForUTC, RunReasonBlackout, "Scheduled run skipped due to blackout window")
				continue
			}
			candidatesByServer[server.Name] = append(candidatesByServer[server.Name], ScheduledCandidate{
				Policy:          policy,
				Server:          server,
				ScheduledForUTC: scheduledForUTC,
			})
		}
	}

	var queueErrs []error
	for serverName, candidates := range candidatesByServer {
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			return s.ComparePolicyCandidates(candidates[i], candidates[j])
		})
		winner := candidates[0]
		for _, skipped := range candidates[1:] {
			s.CreateSkippedRun(skipped.Policy, serverName, skipped.ScheduledForUTC, RunReasonSuperseded, "Scheduled run superseded by higher-priority policy")
		}

		runtimeStatus := deps.CurrentStatusSnapshot(serverName)
		if runtimeStatus == nil {
			s.CreateSkippedRun(winner.Policy, serverName, winner.ScheduledForUTC, RunReasonMissing, "Scheduled run skipped because the server was missing")
			continue
		}
		if deps.StatusInProgress(runtimeStatus.Status) {
			s.CreateSkippedRun(winner.Policy, serverName, winner.ScheduledForUTC, RunReasonBusy, "Scheduled run skipped because the server is busy")
			continue
		}

		run, inserted, err := deps.CreateRun(Run{
			PolicyID:        winner.Policy.ID,
			PolicyName:      winner.Policy.Name,
			ServerName:      serverName,
			ScheduledForUTC: winner.ScheduledForUTC,
			ExecutionMode:   winner.Policy.ExecutionMode,
			PackageScope:    winner.Policy.PackageScope,
			UpgradeMode:     winner.Policy.UpgradeMode,
			Status:          RunQueued,
			Summary:         "Scheduled run queued",
			ResultJSON:      "{}",
		})
		if err != nil {
			queueErr := fmt.Errorf(
				"queue scheduled run failed: policy_id=%d policy_name=%q server=%q scheduled_for_utc=%q: %w",
				winner.Policy.ID,
				winner.Policy.Name,
				serverName,
				winner.ScheduledForUTC,
				err,
			)
			deps.Logf("processDueUpdatePolicies: %v", queueErr)
			queueErrs = append(queueErrs, queueErr)
			continue
		}
		if !inserted {
			continue
		}
		deps.ExecuteRun(run, winner.Policy, winner.Server)
	}
	if len(queueErrs) > 0 {
		return fmt.Errorf("scheduled policy queue encountered %d error(s): %w", len(queueErrs), errors.Join(queueErrs...))
	}
	return nil
}

func (s *Service) ProcessDue(now time.Time) error {
	deps := s.EnsureDeps()
	s.tickMu.Lock()
	defer s.tickMu.Unlock()
	if !deps.TryBackupRestoreReadLock() {
		s.RememberMissedTick(now)
		return nil
	}
	defer deps.UnlockBackupRestoreRead()

	for _, missedTick := range s.PendingMissedTicks() {
		if err := s.ProcessDueSlot(ScheduleRequest{Now: missedTick, MaintenanceActive: true}); err != nil {
			return err
		}
		s.ForgetMissedTick(missedTick)
	}
	return s.ProcessDueSlot(ScheduleRequest{Now: now, MaintenanceActive: deps.CurrentMaintenanceActive()})
}

func weekdayMatchesLocal(weekdays []string, t time.Time) bool {
	if len(weekdays) == 0 {
		return true
	}
	token, _ := NormalizeWeekdayToken(t.Weekday().String())
	for _, candidate := range weekdays {
		if candidate == token {
			return true
		}
	}
	return false
}

func defaultStatusInProgress(status string) bool {
	switch strings.TrimSpace(status) {
	case "updating", "pending_approval", "approved", "upgrading", "autoremove", "sudoers", "facts_refresh":
		return true
	default:
		return false
	}
}

func truncateString(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
