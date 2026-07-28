package policies

import (
	"context"
	"strings"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	"debian-updater/internal/servers"
)

func TestPreviewPolicyProjectsBoundedDailyAndWeeklyOccurrences(t *testing.T) {
	tests := []struct {
		name       string
		policy     Policy
		now        time.Time
		wantFirst  string
		wantSecond string
	}{
		{
			name: "daily",
			policy: previewProjectionPolicy(Policy{
				CadenceKind: CadenceDaily,
				TimeLocal:   "14:30",
			}),
			now:        time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			wantFirst:  "2026-05-17T14:30:00.000000000Z",
			wantSecond: "2026-05-18T14:30:00.000000000Z",
		},
		{
			name: "weekly",
			policy: previewProjectionPolicy(Policy{
				CadenceKind: CadenceWeekly,
				TimeLocal:   "09:15",
				Weekdays:    []string{"mon", "wed"},
			}),
			now:        time.Date(2026, 5, 17, 12, 0, 0, 0, time.UTC),
			wantFirst:  "2026-05-18T09:15:00.000000000Z",
			wantSecond: "2026-05-20T09:15:00.000000000Z",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := previewProjectionDeps(time.UTC, nil)
			deps.Now = func() time.Time { return tt.now }
			preview, err := NewService(deps).PreviewPolicy(tt.policy)
			if err != nil {
				t.Fatalf("PreviewPolicy() error = %v", err)
			}
			if len(preview.UpcomingOccurrences) != PreviewOccurrenceLimit {
				t.Fatalf("occurrences = %d, want %d", len(preview.UpcomingOccurrences), PreviewOccurrenceLimit)
			}
			if preview.UpcomingOccurrences[0].ScheduledForUTC != tt.wantFirst || preview.UpcomingOccurrences[1].ScheduledForUTC != tt.wantSecond {
				t.Fatalf("first occurrences = %+v, want %s then %s", preview.UpcomingOccurrences[:2], tt.wantFirst, tt.wantSecond)
			}
			first := preview.UpcomingOccurrences[0]
			if first.LocalCivilTime == "" || first.Timezone != "UTC" || first.Offset != "+00:00" || first.Abbreviation != "UTC" {
				t.Fatalf("first occurrence clock facts = %+v, want complete UTC civil-time facts", first)
			}
			if first.AdmissionOutcome != PreviewAdmissionAdmitted || first.MatchedServerCount != 1 {
				t.Fatalf("first occurrence admission facts = %+v, want one admitted server", first)
			}
		})
	}
}

func TestPreviewPolicyProjectsFixedOffsetAndReconcilesTimezoneChanges(t *testing.T) {
	applicationTime := previewApplicationTime(t, "+05:30")
	deps := previewProjectionDeps(time.FixedZone("+05:30", 5*60*60+30*60), applicationTime)
	deps.Now = func() time.Time { return time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC) }
	service := NewService(deps)
	policy := previewProjectionPolicy(Policy{CadenceKind: CadenceDaily, TimeLocal: "08:00"})

	preview, err := service.PreviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	first := preview.UpcomingOccurrences[0]
	if first.Timezone != "+05:30" || first.Offset != "+05:30" || first.Abbreviation != "+05:30" || first.ScheduledForUTC != "2026-05-17T02:30:00.000000000Z" {
		t.Fatalf("fixed-offset occurrence = %+v", first)
	}

	if _, err := applicationTime.Configure(context.Background(), "UTC"); err != nil {
		t.Fatal(err)
	}
	service.deps.CurrentLocation = func() *time.Location { return time.UTC }
	preview, err = service.PreviewPolicy(policy)
	if err != nil {
		t.Fatal(err)
	}
	first = preview.UpcomingOccurrences[0]
	if first.Timezone != "UTC" || first.Offset != "+00:00" || first.ScheduledForUTC != "2026-05-17T08:00:00.000000000Z" {
		t.Fatalf("timezone-reconciled occurrence = %+v", first)
	}
}

func TestPreviewPolicyReportsCanonicalDSTChoices(t *testing.T) {
	loc, err := time.LoadLocation("America/Toronto")
	if err != nil {
		t.Fatal(err)
	}
	applicationTime := previewApplicationTime(t, "America/Toronto")
	deps := previewProjectionDeps(loc, applicationTime)
	service := NewService(deps)

	deps.Now = func() time.Time { return time.Date(2026, 3, 7, 0, 0, 0, 0, loc) }
	service.deps.Now = deps.Now
	transition, err := service.PreviewPolicy(previewProjectionPolicy(Policy{CadenceKind: CadenceDaily, TimeLocal: "03:30"}))
	if err != nil {
		t.Fatal(err)
	}
	if transition.UpcomingOccurrences[1].DSTStatus != PreviewDSTOffsetChanged || transition.UpcomingOccurrences[1].Offset != "-04:00" {
		t.Fatalf("spring offset transition = %+v, want explicit offset change", transition.UpcomingOccurrences[1])
	}
	if !hasPreviewDiagnostic(transition.InformationalFacts, "dst_offset_changed", "-05:00 to -04:00") {
		t.Fatalf("transition facts = %+v, want offset-change fact", transition.InformationalFacts)
	}

	deps.Now = func() time.Time { return time.Date(2026, 3, 7, 12, 0, 0, 0, loc) }
	service.deps.Now = deps.Now
	spring, err := service.PreviewPolicy(previewProjectionPolicy(Policy{CadenceKind: CadenceDaily, TimeLocal: "02:30"}))
	if err != nil {
		t.Fatal(err)
	}
	if spring.UpcomingOccurrences[0].ScheduledForUTC != "2026-03-09T06:30:00.000000000Z" {
		t.Fatalf("spring first real occurrence = %+v, want day after nonexistent local time", spring.UpcomingOccurrences[0])
	}
	if !hasPreviewDiagnostic(spring.InformationalFacts, "dst_nonexistent_skipped", "2026-03-08") {
		t.Fatalf("spring facts = %+v, want canonical skipped-time fact", spring.InformationalFacts)
	}

	service.deps.Now = func() time.Time { return time.Date(2026, 10, 31, 12, 0, 0, 0, loc) }
	fallback, err := service.PreviewPolicy(previewProjectionPolicy(Policy{CadenceKind: CadenceDaily, TimeLocal: "01:30"}))
	if err != nil {
		t.Fatal(err)
	}
	first := fallback.UpcomingOccurrences[0]
	if first.ScheduledForUTC != "2026-11-01T05:30:00.000000000Z" || first.Offset != "-04:00" || first.Abbreviation != "EDT" {
		t.Fatalf("fallback occurrence = %+v, want earlier EDT occurrence", first)
	}
	if first.DSTStatus != PreviewDSTAmbiguous || first.CanonicalChoice != PreviewCanonicalEarlierFallback {
		t.Fatalf("fallback choice = %+v, want explicit canonical earlier occurrence", first)
	}
}

func TestPreviewPolicySeparatesNoRunEmptyTargetAndValidationDiagnostics(t *testing.T) {
	globalWindow := BlackoutWindow{Weekdays: []string{"sun"}, StartTime: "01:00", EndTime: "03:00"}
	policyWindow := BlackoutWindow{Weekdays: []string{"mon"}, StartTime: "01:00", EndTime: "03:00"}
	deps := previewProjectionDeps(time.UTC, nil)
	deps.Now = func() time.Time { return time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC) }
	deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) { return []BlackoutWindow{globalWindow}, nil }
	service := NewService(deps)

	blockedPolicy := previewProjectionPolicy(Policy{
		CadenceKind:     CadenceDaily,
		TimeLocal:       "02:00",
		PolicyBlackouts: []BlackoutWindow{policyWindow},
	})
	blocked, err := service.PreviewPolicy(blockedPolicy)
	if err != nil {
		t.Fatal(err)
	}
	if blocked.UpcomingOccurrences[0].AdmissionOutcome != PreviewAdmissionBlockedNoRun || len(blocked.UpcomingOccurrences[0].ApplicableNoRunWindows) != 1 || blocked.UpcomingOccurrences[0].ApplicableNoRunWindows[0].Source != NoRunScopeGlobal {
		t.Fatalf("Sunday occurrence = %+v, want global no-run outcome", blocked.UpcomingOccurrences[0])
	}
	if blocked.UpcomingOccurrences[1].AdmissionOutcome != PreviewAdmissionBlockedNoRun || len(blocked.UpcomingOccurrences[1].ApplicableNoRunWindows) != 1 || blocked.UpcomingOccurrences[1].ApplicableNoRunWindows[0].Source != NoRunScopePolicy {
		t.Fatalf("Monday occurrence = %+v, want policy no-run outcome", blocked.UpcomingOccurrences[1])
	}
	if !hasPreviewDiagnostic(blocked.OperationalWarnings, "no_run_window", "") {
		t.Fatalf("operational warnings = %+v, want no-run warning", blocked.OperationalWarnings)
	}

	emptyTarget := blockedPolicy
	emptyTarget.TargetServers = []string{"srv-missing"}
	emptyTarget.TargetTag = ""
	empty, err := service.PreviewPolicy(emptyTarget)
	if err != nil {
		t.Fatal(err)
	}
	if empty.UpcomingOccurrences[0].AdmissionOutcome != PreviewAdmissionNoMatchingServers {
		t.Fatalf("empty target outcome = %q, want %q", empty.UpcomingOccurrences[0].AdmissionOutcome, PreviewAdmissionNoMatchingServers)
	}
	if !hasPreviewDiagnostic(empty.OperationalWarnings, "no_matching_servers", "") {
		t.Fatalf("empty-target warnings = %+v", empty.OperationalWarnings)
	}

	invalid, err := service.PreviewPolicy(Policy{Name: "Invalid draft"})
	if err != nil {
		t.Fatalf("invalid preview should return structured validation, got error %v", err)
	}
	if len(invalid.ValidationErrors) != 1 || invalid.ValidationErrors[0].Code != "invalid_policy" {
		t.Fatalf("validation errors = %+v, want a distinct invalid-policy diagnostic", invalid.ValidationErrors)
	}
	if len(invalid.OperationalWarnings) != 0 || len(invalid.InformationalFacts) != 0 || len(invalid.UpcomingOccurrences) != 0 {
		t.Fatalf("invalid preview leaked non-validation projection data: %+v", invalid)
	}
}

func TestPreviewPolicyProjectsCrossPolicyConflicts(t *testing.T) {
	serversSnapshot := []servers.Server{
		{Name: "srv-a", Tags: []string{"prod"}},
		{Name: "srv-b", Tags: []string{"prod"}},
		{Name: "srv-dev", Tags: []string{"dev"}},
	}
	draft := previewProjectionPolicy(Policy{
		ID:          42,
		TargetTag:   "prod",
		CadenceKind: CadenceDaily,
		TimeLocal:   "02:00",
	})
	allDays := []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

	tests := []struct {
		name            string
		policies        []Policy
		wantConflicts   int
		wantKind        string
		wantShared      []string
		wantEffective   bool
		wantCompeting   string
		wantWarning     bool
		wantSuppressed  bool
		wantWindowCount int
	}{
		{
			name: "no overlap",
			policies: []Policy{
				previewProjectionPolicy(Policy{ID: 7, Name: "Different minute", TargetTag: "prod", TimeLocal: "03:00"}),
			},
		},
		{
			name: "edited policy does not conflict with itself",
			policies: []Policy{
				previewProjectionPolicy(Policy{ID: 42, Name: "Projected policy", TargetTag: "prod", TimeLocal: "02:00"}),
			},
		},
		{
			name: "partial overlap",
			policies: []Policy{
				previewProjectionPolicy(Policy{ID: 7, Name: "Only A", TargetTag: "", TargetServers: []string{"srv-a"}, TimeLocal: "02:00"}),
			},
			wantConflicts:   1,
			wantKind:        PreviewConflictPartial,
			wantShared:      []string{"srv-a"},
			wantEffective:   true,
			wantCompeting:   PreviewAdmissionAdmitted,
			wantWarning:     true,
			wantWindowCount: PreviewOccurrenceLimit,
		},
		{
			name: "full overlap",
			policies: []Policy{
				previewProjectionPolicy(Policy{ID: 7, Name: "Same fleet", TargetTag: "prod", TimeLocal: "02:00"}),
			},
			wantConflicts:   1,
			wantKind:        PreviewConflictFull,
			wantShared:      []string{"srv-a", "srv-b"},
			wantEffective:   true,
			wantCompeting:   PreviewAdmissionAdmitted,
			wantWarning:     true,
			wantWindowCount: PreviewOccurrenceLimit,
		},
		{
			name: "disabled policy is ignored",
			policies: []Policy{
				{
					ID:            7,
					Name:          "Disabled",
					Enabled:       false,
					TargetTag:     "prod",
					PackageScope:  PackageScopeSecurity,
					UpgradeMode:   UpgradeModeStandard,
					ExecutionMode: ExecutionScanOnly,
					CadenceKind:   CadenceDaily,
					TimeLocal:     "02:00",
				},
			},
		},
		{
			name: "no run suppresses competing candidates",
			policies: []Policy{
				previewProjectionPolicy(Policy{
					ID:        7,
					Name:      "Blocked competitor",
					TargetTag: "prod",
					TimeLocal: "02:00",
					PolicyBlackouts: []BlackoutWindow{{
						Weekdays:  allDays,
						StartTime: "01:00",
						EndTime:   "03:00",
					}},
				}),
			},
			wantConflicts:   1,
			wantKind:        PreviewConflictFull,
			wantShared:      []string{"srv-a", "srv-b"},
			wantEffective:   false,
			wantCompeting:   PreviewAdmissionBlockedNoRun,
			wantSuppressed:  true,
			wantWindowCount: PreviewOccurrenceLimit,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := previewProjectionDeps(time.UTC, nil)
			deps.Now = func() time.Time { return time.Date(2026, 5, 17, 0, 0, 0, 0, time.UTC) }
			deps.SnapshotServers = func() []servers.Server { return append([]servers.Server(nil), serversSnapshot...) }
			deps.ListPolicies = func() ([]Policy, error) { return append([]Policy(nil), tt.policies...), nil }

			preview, err := NewService(deps).PreviewPolicy(draft)
			if err != nil {
				t.Fatal(err)
			}
			if len(preview.ScheduleConflicts) != tt.wantConflicts {
				t.Fatalf("schedule conflicts = %+v, want %d", preview.ScheduleConflicts, tt.wantConflicts)
			}
			if tt.wantConflicts == 0 {
				if hasPreviewDiagnostic(preview.OperationalWarnings, "policy_schedule_overlap", "") {
					t.Fatalf("warnings = %+v, want no overlap warning", preview.OperationalWarnings)
				}
				return
			}

			conflict := preview.ScheduleConflicts[0]
			if conflict.PolicyID != 7 || conflict.PolicyName != tt.policies[0].Name || conflict.OverlapKind != tt.wantKind {
				t.Fatalf("conflict identity = %+v", conflict)
			}
			if strings.Join(conflict.SharedServers, ",") != strings.Join(tt.wantShared, ",") {
				t.Fatalf("shared servers = %v, want %v", conflict.SharedServers, tt.wantShared)
			}
			if len(conflict.OccurrenceWindows) != tt.wantWindowCount {
				t.Fatalf("occurrence windows = %+v, want %d", conflict.OccurrenceWindows, tt.wantWindowCount)
			}
			window := conflict.OccurrenceWindows[0]
			if window.LocalCivilTime != "2026-05-17 02:00" ||
				window.WindowStartUTC != "2026-05-17T02:00:00.000000000Z" ||
				window.WindowEndUTC != "2026-05-17T02:01:00.000000000Z" ||
				window.Effective != tt.wantEffective ||
				window.DraftAdmissionOutcome != PreviewAdmissionAdmitted ||
				window.CompetingAdmissionOutcome != tt.wantCompeting {
				t.Fatalf("first conflict window = %+v", window)
			}
			if hasPreviewDiagnostic(preview.OperationalWarnings, "policy_schedule_overlap", "") != tt.wantWarning {
				t.Fatalf("warnings = %+v, want overlap warning %t", preview.OperationalWarnings, tt.wantWarning)
			}
			if hasPreviewDiagnostic(preview.InformationalFacts, "policy_schedule_overlap_suppressed_by_no_run", "") != tt.wantSuppressed {
				t.Fatalf("facts = %+v, want suppressed fact %t", preview.InformationalFacts, tt.wantSuppressed)
			}
		})
	}
}

func previewProjectionPolicy(patch Policy) Policy {
	policy := Policy{
		ID:            42,
		Name:          "Projected policy",
		Enabled:       true,
		TargetTag:     "prod",
		PackageScope:  PackageScopeSecurity,
		UpgradeMode:   UpgradeModeStandard,
		ExecutionMode: ExecutionScanOnly,
		CadenceKind:   CadenceDaily,
		TimeLocal:     "02:00",
	}
	if patch.ID != 0 {
		policy.ID = patch.ID
	}
	if patch.Name != "" {
		policy.Name = patch.Name
	}
	if patch.CadenceKind != "" {
		policy.CadenceKind = patch.CadenceKind
	}
	if patch.TimeLocal != "" {
		policy.TimeLocal = patch.TimeLocal
	}
	if patch.Weekdays != nil {
		policy.Weekdays = patch.Weekdays
	}
	if patch.TargetTag != "" || patch.TargetServers != nil {
		policy.TargetTag = patch.TargetTag
		policy.TargetServers = patch.TargetServers
	}
	if patch.PolicyBlackouts != nil {
		policy.PolicyBlackouts = patch.PolicyBlackouts
	}
	return policy
}

func previewProjectionDeps(loc *time.Location, applicationTime *apptimepkg.Module) ServiceDeps {
	return ServiceDeps{
		SnapshotServers: func() []servers.Server {
			return []servers.Server{{Name: "srv-prod", Tags: []string{"prod", "web"}}}
		},
		LoadOverrides:       func() (map[int64]map[string]bool, error) { return map[int64]map[string]bool{}, nil },
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) { return []BlackoutWindow{}, nil },
		CurrentLocation:     func() *time.Location { return loc },
		ApplicationTime:     applicationTime,
		TimestampLayout:     DefaultTimestampLayout,
	}
}

func previewApplicationTime(t *testing.T, timezone string) *apptimepkg.Module {
	t.Helper()
	module := apptimepkg.New(apptimepkg.Deps{Store: apptimepkg.NewMemoryStore(timezone)})
	if err := module.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	return module
}

func hasPreviewDiagnostic(items []PreviewDiagnostic, code, messagePart string) bool {
	for _, item := range items {
		if item.Code == code && strings.Contains(item.Message, messagePart) {
			return true
		}
	}
	return false
}
