package policies

import (
	"context"
	"strings"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

func testServiceDeps() ServiceDeps {
	return ServiceDeps{
		ListPolicies: func() ([]Policy, error) {
			return nil, nil
		},
		LoadOverrides: func() (map[int64]map[string]bool, error) {
			return map[int64]map[string]bool{}, nil
		},
		LoadGlobalBlackouts: func() ([]BlackoutWindow, error) {
			return nil, nil
		},
		SnapshotServers: func() []servers.Server {
			return nil
		},
		HandleScheduledRun: func(ScheduledRunRequest) ScheduledRunResult {
			return ScheduledRunResult{Handled: true, Inserted: true}
		},
		CurrentLocation: func() *time.Location {
			return time.UTC
		},
		MarkInterruptedRuns: func() error {
			return nil
		},
		Now: func() time.Time {
			return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
		},
		Logf: func(string, ...any) {},
	}
}

func TestServiceNormalizePolicyRequiresTarget(t *testing.T) {
	policy := Policy{
		Name:          "Nightly",
		Enabled:       true,
		PackageScope:  PackageScopeSecurity,
		ExecutionMode: ExecutionScanOnly,
		CadenceKind:   CadenceDaily,
		TimeLocal:     "03:00",
	}

	err := NewService(testServiceDeps()).NormalizePolicy(&policy)
	if err == nil {
		t.Fatalf("NormalizePolicy() error = nil, want no-target validation error")
	}
}

func TestServiceMatchesServersWithTargetsAndOverrides(t *testing.T) {
	service := NewService(testServiceDeps())
	server := servers.Server{Name: "srv-a", Tags: []string{"prod", "db"}}

	tests := []struct {
		name      string
		policy    Policy
		overrides map[int64]map[string]bool
		want      bool
	}{
		{name: "explicit server", policy: Policy{ID: 1, Enabled: true, TargetServers: []string{"SRV-A"}}, want: true},
		{name: "legacy target tag", policy: Policy{ID: 2, Enabled: true, TargetTag: "PROD"}, want: true},
		{name: "include tag", policy: Policy{ID: 3, Enabled: true, IncludeTags: []string{"db"}}, want: true},
		{name: "exclude wins", policy: Policy{ID: 4, Enabled: true, IncludeTags: []string{"prod"}, ExcludeTags: []string{"DB"}}, want: false},
		{name: "override disables", policy: Policy{ID: 5, Enabled: true, TargetServers: []string{"srv-a"}}, overrides: map[int64]map[string]bool{5: {"srv-a": true}}, want: false},
		{name: "disabled policy", policy: Policy{ID: 6, Enabled: false, TargetServers: []string{"srv-a"}}, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.overrides == nil {
				tt.overrides = map[int64]map[string]bool{}
			}
			got := service.PolicyMatchesServer(tt.policy, server, MatchContext{Overrides: tt.overrides})
			if got != tt.want {
				t.Fatalf("PolicyMatchesServer() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestServicePreviewPolicyClassifiesMatchesExclusionsOverridesAndWarnings(t *testing.T) {
	deps := testServiceDeps()
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{
			{Name: "srv-web", Tags: []string{"prod", "web"}},
			{Name: "srv-db", Tags: []string{"prod", "db"}},
			{Name: "srv-hold", Tags: []string{"prod", "hold"}},
			{Name: "srv-dev", Tags: []string{"dev"}},
			{Name: "srv-explicit", Tags: []string{"batch"}},
		}
	}
	deps.LoadOverrides = func() (map[int64]map[string]bool, error) {
		return map[int64]map[string]bool{42: {"srv-db": true}}, nil
	}
	service := NewService(deps)

	preview, err := service.PreviewPolicy(Policy{
		ID:            42,
		Name:          "Preview",
		Enabled:       false,
		TargetTag:     "prod",
		IncludeTags:   []string{"web"},
		ExcludeTags:   []string{"hold"},
		TargetServers: []string{"srv-explicit", "srv-missing"},
		PackageScope:  PackageScopeSecurity,
		ExecutionMode: ExecutionScanOnly,
		CadenceKind:   CadenceDaily,
		TimeLocal:     "02:00",
	})
	if err != nil {
		t.Fatalf("PreviewPolicy() error = %v", err)
	}
	if got := previewServerNames(preview.MatchedServers); len(got) != 2 || got[0] != "srv-explicit" || got[1] != "srv-web" {
		t.Fatalf("matched = %+v, want [srv-explicit srv-web]", got)
	}
	if got := previewServerNames(preview.DisabledByOverride); len(got) != 1 || got[0] != "srv-db" {
		t.Fatalf("disabled_by_override = %+v, want [srv-db]", got)
	}
	reasons := map[string]string{}
	for _, item := range preview.ExcludedServers {
		reasons[item.Name] = item.Reason
	}
	if reasons["srv-hold"] != "excluded_tag" || reasons["srv-dev"] != "no_target_match" {
		t.Fatalf("excluded reasons = %+v, want hold tag and dev no match", reasons)
	}
	joinedWarnings := strings.Join(preview.Warnings, "\n")
	if !strings.Contains(joinedWarnings, "Policy is disabled") || !strings.Contains(joinedWarnings, "srv-missing") {
		t.Fatalf("warnings = %+v, want disabled and missing explicit server warnings", preview.Warnings)
	}
}

func TestServiceDueAndBlackoutWindowsUseLocalTime(t *testing.T) {
	service := NewService(testServiceDeps())
	loc := time.FixedZone("App", -5*60*60)
	slot := time.Date(2026, 1, 5, 23, 30, 0, 0, loc)

	if !service.PolicyDueAt(Policy{
		Enabled:     true,
		CadenceKind: CadenceWeekly,
		TimeLocal:   "23:30",
		Weekdays:    []string{"mon"},
	}, slot) {
		t.Fatalf("PolicyDueAt() = false, want weekly local match")
	}

	overnight := []BlackoutWindow{{
		Weekdays:  []string{"mon"},
		StartTime: "22:00",
		EndTime:   "02:00",
	}}
	if !service.BlackoutApplies(slot, overnight) {
		t.Fatalf("BlackoutApplies(Monday 23:30) = false, want true")
	}
	tuesdayEarly := time.Date(2026, 1, 6, 1, 30, 0, 0, loc)
	if !service.BlackoutApplies(tuesdayEarly, overnight) {
		t.Fatalf("BlackoutApplies(Tuesday 01:30) = false, want overnight carryover")
	}
}

func previewServerNames(items []PreviewServer) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name)
	}
	return out
}

func TestServiceProjectScheduleProjectsNextRunsAndNoRunWindows(t *testing.T) {
	now := time.Date(2026, 1, 5, 1, 0, 0, 0, time.UTC)
	serverList := []servers.Server{
		{Name: "srv-win"},
		{Name: "srv-policy-blackout"},
		{Name: "srv-global"},
		{Name: "srv-override"},
	}
	policyList := []Policy{
		{ID: 1, Name: "winner", Enabled: true, TargetServers: []string{"srv-win"}, PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired, CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Name: "superseded", Enabled: true, TargetServers: []string{"srv-win"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 3, Name: "policy blackout", Enabled: true, TargetServers: []string{"srv-policy-blackout"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00", PolicyBlackouts: []BlackoutWindow{{Weekdays: []string{"mon"}, StartTime: "00:00", EndTime: "02:00"}}},
		{ID: 4, Name: "global target", Enabled: true, TargetServers: []string{"srv-global"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
		{ID: 5, Name: "disabled by override", Enabled: true, TargetServers: []string{"srv-override"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
	}
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return append([]Policy(nil), policyList...), nil }
	deps.LoadOverrides = func() (map[int64]map[string]bool, error) {
		return map[int64]map[string]bool{5: {"srv-override": true}}, nil
	}
	deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) {
		return []BlackoutWindow{{Weekdays: []string{"mon"}, StartTime: "00:00", EndTime: "02:00"}}, nil
	}

	projection, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{
		Now:     now,
		Servers: serverList,
	})
	if err != nil {
		t.Fatalf("ProjectSchedule() error = %v", err)
	}
	win := projection.Servers["srv-win"]
	if win.NextRun.PolicyName != "winner" || win.NextRun.State != ScheduleProjectionStateScheduled || win.NextRun.ScheduledForUTC != "2026-01-05T03:00:00.000000000Z" {
		t.Fatalf("srv-win next run = %+v, want winner at 03:00", win.NextRun)
	}
	policyBlackout := projection.Servers["srv-policy-blackout"].NoRun
	if !policyBlackout.Active || policyBlackout.Scope != NoRunScopeGlobal {
		t.Fatalf("srv-policy-blackout no-run = %+v, want active global blackout while global window is active", policyBlackout)
	}
	global := projection.Servers["srv-global"].NoRun
	if !global.Active || global.Scope != NoRunScopeGlobal {
		t.Fatalf("srv-global no-run = %+v, want active global blackout", global)
	}
	if got := projection.Servers["srv-override"].NextRun; got.ScheduledForUTC != "" || got.PolicyName != "" {
		t.Fatalf("srv-override next run = %+v, want disabled by override", got)
	}

	deps.LoadGlobalBlackouts = func() ([]BlackoutWindow, error) { return []BlackoutWindow{}, nil }
	projection, err = NewService(deps).ProjectSchedule(ScheduleProjectionRequest{
		Now:     now,
		Servers: serverList,
	})
	if err != nil {
		t.Fatalf("ProjectSchedule(policy blackout only) error = %v", err)
	}
	policyBlackout = projection.Servers["srv-policy-blackout"].NoRun
	if !policyBlackout.Active || policyBlackout.Scope != NoRunScopePolicy || policyBlackout.PolicyName != "policy blackout" {
		t.Fatalf("srv-policy-blackout no-run = %+v, want active policy blackout", policyBlackout)
	}
}

func TestServiceProjectScheduleMergesPersistedAndProjectedRuns(t *testing.T) {
	now := time.Date(2026, 1, 5, 1, 0, 0, 0, time.UTC)
	serverList := []servers.Server{
		{Name: "srv-persisted"},
		{Name: "srv-projected"},
		{Name: "srv-past"},
	}
	policyList := []Policy{
		{ID: 1, Name: "projected-persisted", Enabled: true, TargetServers: []string{"srv-persisted"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
		{ID: 2, Name: "projected-wins", Enabled: true, TargetServers: []string{"srv-projected"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
		{ID: 3, Name: "past-ignored", Enabled: true, TargetServers: []string{"srv-past"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
	}
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return append([]Policy(nil), policyList...), nil }
	deps.ListRuns = func(limit int) ([]Run, error) {
		if limit != 42 {
			t.Fatalf("ListRuns limit = %d, want request limit", limit)
		}
		return []Run{
			{PolicyName: "persisted earlier", ServerName: "srv-persisted", ScheduledForUTC: "2026-01-05T02:30:00Z", Status: RunQueued, Summary: "Persisted run pending"},
			{PolicyName: "persisted later", ServerName: "srv-projected", ScheduledForUTC: "2026-01-05T04:00:00Z", Status: RunQueued, Summary: "Later persisted run"},
			{PolicyName: "past persisted", ServerName: "srv-past", ScheduledForUTC: "2026-01-05T00:30:00Z", Status: RunQueued, Summary: "Past persisted run"},
		}, nil
	}

	projection, err := NewService(deps).ProjectSchedule(ScheduleProjectionRequest{
		Now:      now,
		Servers:  serverList,
		RunLimit: 42,
	})
	if err != nil {
		t.Fatalf("ProjectSchedule() error = %v", err)
	}
	persisted := projection.Servers["srv-persisted"].NextRun
	if persisted.PolicyName != "persisted earlier" || persisted.ScheduledForUTC != "2026-01-05T02:30:00Z" || persisted.Status != RunQueued {
		t.Fatalf("srv-persisted next run = %+v, want earlier persisted run", persisted)
	}
	projected := projection.Servers["srv-projected"].NextRun
	if projected.PolicyName != "projected-wins" || projected.ScheduledForUTC != "2026-01-05T03:00:00.000000000Z" || projected.Status != ScheduleProjectionStateScheduled {
		t.Fatalf("srv-projected next run = %+v, want earlier projected run", projected)
	}
	past := projection.Servers["srv-past"].NextRun
	if past.PolicyName != "past-ignored" || past.ScheduledForUTC != "2026-01-05T03:00:00.000000000Z" {
		t.Fatalf("srv-past next run = %+v, want past persisted run ignored", past)
	}
}

func TestServiceProcessDueSendsCandidatesAndPolicySideSkipsToScheduledRunCallback(t *testing.T) {
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	policies := []Policy{
		{ID: 1, Name: "winner", Enabled: true, TargetServers: []string{"srv-win"}, PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired, CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 2, Name: "superseded", Enabled: true, TargetServers: []string{"srv-win"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: 3, Name: "busy", Enabled: true, TargetServers: []string{"srv-busy"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
		{ID: 4, Name: "missing", Enabled: true, TargetServers: []string{"srv-missing"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
		{ID: 5, Name: "blackout", Enabled: true, TargetServers: []string{"srv-blackout"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00", PolicyBlackouts: []BlackoutWindow{{Weekdays: []string{"mon"}, StartTime: "02:00", EndTime: "04:00"}}},
		{ID: 6, Name: "disabled", Enabled: false, TargetServers: []string{"srv-disabled"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"},
	}
	serverList := []servers.Server{{Name: "srv-win"}, {Name: "srv-busy"}, {Name: "srv-missing"}, {Name: "srv-blackout"}, {Name: "srv-disabled"}}

	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return append([]Policy(nil), policies...), nil }
	deps.SnapshotServers = func() []servers.Server { return append([]servers.Server(nil), serverList...) }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	if err := NewService(deps).ProcessDueSlot(ScheduleRequest{Now: slot}); err != nil {
		t.Fatalf("ProcessDueSlot() unexpected error: %v", err)
	}
	outcomes := map[string]string{}
	for _, req := range handled {
		outcomes[req.Server.Name+":"+req.Policy.Name] = req.Outcome
	}
	wantOutcomes := map[string]string{
		"srv-win:winner":        "",
		"srv-win:superseded":    RunReasonSuperseded,
		"srv-busy:busy":         "",
		"srv-missing:missing":   "",
		"srv-blackout:blackout": RunReasonBlackout,
	}
	if len(outcomes) != len(wantOutcomes) {
		t.Fatalf("outcomes = %+v, want exactly %+v", outcomes, wantOutcomes)
	}
	for key, want := range wantOutcomes {
		if outcomes[key] != want {
			t.Fatalf("outcome[%s] = %q, want %q; all requests=%+v", key, outcomes[key], want, handled)
		}
	}
}

func TestServiceProcessDueRemembersAndReplaysMissedTicks(t *testing.T) {
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	deps.Maintenance = coordinator
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{{ID: 7, Name: "maintenance replay", Enabled: true, TargetServers: []string{"srv"}, PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly, CadenceKind: CadenceDaily, TimeLocal: "03:00"}}, nil
	}
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv"}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	exclusive, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	if err := service.ProcessDue(slot); err != nil {
		t.Fatalf("ProcessDue(blocked) unexpected error: %v", err)
	}
	if got := service.PendingMissedTicks(); len(got) != 1 {
		t.Fatalf("missed ticks = %v, want one tick", got)
	}

	if err := exclusive.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := service.ProcessDue(slot.Add(time.Minute)); err != nil {
		t.Fatalf("ProcessDue(replay) unexpected error: %v", err)
	}
	if got := service.PendingMissedTicks(); len(got) != 0 {
		t.Fatalf("missed ticks after replay = %v, want none", got)
	}
	if len(handled) != 1 || handled[0].Outcome != RunReasonMaintenance {
		t.Fatalf("handled replay requests = %+v, want one maintenance skip", handled)
	}
}
