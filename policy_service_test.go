package main

import (
	"context"
	"errors"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
)

func testPolicyServiceDeps() PolicyServiceDeps {
	return PolicyServiceDeps{
		ListPolicies: func() ([]UpdatePolicy, error) {
			return nil, nil
		},
		LoadOverrides: func() (map[int64]map[string]bool, error) {
			return map[int64]map[string]bool{}, nil
		},
		LoadGlobalBlackouts: func() ([]UpdatePolicyBlackoutWindow, error) {
			return nil, nil
		},
		SnapshotServers: func() []Server {
			return nil
		},
		HandleScheduledRun: func(PolicyScheduledRunRequest) PolicyScheduledRunResult {
			return PolicyScheduledRunResult{Handled: true, Inserted: true}
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

func TestPolicyServiceNormalizePolicyRequiresTarget(t *testing.T) {
	policy := UpdatePolicy{
		Name:          "Nightly",
		Enabled:       true,
		PackageScope:  updatePolicyPackageScopeSecurity,
		ExecutionMode: updatePolicyExecutionScanOnly,
		CadenceKind:   updatePolicyCadenceDaily,
		TimeLocal:     "03:00",
	}

	err := NewPolicyService(testPolicyServiceDeps()).NormalizePolicy(&policy)
	if err == nil {
		t.Fatalf("NormalizePolicy() error = nil, want no-target validation error")
	}
	if !errors.Is(wrapUpdatePolicyValidationError(err), errUpdatePolicyValidation) {
		t.Fatalf("wrapped validation error is not errUpdatePolicyValidation: %v", err)
	}
}

func TestPolicyServiceMatchesServersWithTargetsAndOverrides(t *testing.T) {
	service := NewPolicyService(testPolicyServiceDeps())
	server := Server{Name: "srv-a", Tags: []string{"prod", "db"}}

	tests := []struct {
		name      string
		policy    UpdatePolicy
		overrides map[int64]map[string]bool
		want      bool
	}{
		{
			name:   "explicit server",
			policy: UpdatePolicy{ID: 1, Enabled: true, TargetServers: []string{"SRV-A"}},
			want:   true,
		},
		{
			name:   "legacy target tag",
			policy: UpdatePolicy{ID: 2, Enabled: true, TargetTag: "PROD"},
			want:   true,
		},
		{
			name:   "include tag",
			policy: UpdatePolicy{ID: 3, Enabled: true, IncludeTags: []string{"db"}},
			want:   true,
		},
		{
			name:   "exclude wins",
			policy: UpdatePolicy{ID: 4, Enabled: true, IncludeTags: []string{"prod"}, ExcludeTags: []string{"DB"}},
			want:   false,
		},
		{
			name:      "override disables",
			policy:    UpdatePolicy{ID: 5, Enabled: true, TargetServers: []string{"srv-a"}},
			overrides: map[int64]map[string]bool{5: {"srv-a": true}},
			want:      false,
		},
		{
			name:   "disabled policy",
			policy: UpdatePolicy{ID: 6, Enabled: false, TargetServers: []string{"srv-a"}},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.overrides == nil {
				tt.overrides = map[int64]map[string]bool{}
			}
			got := service.PolicyMatchesServer(tt.policy, server, PolicyMatchContext{Overrides: tt.overrides})
			if got != tt.want {
				t.Fatalf("PolicyMatchesServer() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestPolicyServiceDueAndBlackoutWindowsUseLocalTime(t *testing.T) {
	service := NewPolicyService(testPolicyServiceDeps())
	loc := time.FixedZone("App", -5*60*60)
	slot := time.Date(2026, 1, 5, 23, 30, 0, 0, loc) // Monday

	if !service.PolicyDueAt(UpdatePolicy{
		Enabled:     true,
		CadenceKind: updatePolicyCadenceWeekly,
		TimeLocal:   "23:30",
		Weekdays:    []string{"mon"},
	}, slot) {
		t.Fatalf("PolicyDueAt() = false, want weekly local match")
	}

	overnight := []UpdatePolicyBlackoutWindow{{
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

func TestPolicyServiceProcessDueSendsCandidatesAndPolicySideSkipsToScheduledRunCallback(t *testing.T) {
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	policies := []UpdatePolicy{
		{
			ID:            1,
			Name:          "winner",
			Enabled:       true,
			TargetServers: []string{"srv-win"},
			PackageScope:  updatePolicyPackageScopeFull,
			ExecutionMode: updatePolicyExecutionApprovalRequired,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
			CreatedAt:     "2026-01-01T00:00:00Z",
		},
		{
			ID:            2,
			Name:          "superseded",
			Enabled:       true,
			TargetServers: []string{"srv-win"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
			CreatedAt:     "2026-01-01T00:00:00Z",
		},
		{
			ID:            3,
			Name:          "busy",
			Enabled:       true,
			TargetServers: []string{"srv-busy"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
		},
		{
			ID:            4,
			Name:          "missing",
			Enabled:       true,
			TargetServers: []string{"srv-missing"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
		},
		{
			ID:            5,
			Name:          "blackout",
			Enabled:       true,
			TargetServers: []string{"srv-blackout"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
			PolicyBlackouts: []UpdatePolicyBlackoutWindow{{
				Weekdays:  []string{"mon"},
				StartTime: "02:00",
				EndTime:   "04:00",
			}},
		},
		{
			ID:            6,
			Name:          "disabled",
			Enabled:       false,
			TargetServers: []string{"srv-disabled"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
		},
	}
	servers := []Server{
		{Name: "srv-win"},
		{Name: "srv-busy"},
		{Name: "srv-missing"},
		{Name: "srv-blackout"},
		{Name: "srv-disabled"},
	}

	var handled []PolicyScheduledRunRequest
	deps := testPolicyServiceDeps()
	deps.ListPolicies = func() ([]UpdatePolicy, error) {
		return append([]UpdatePolicy(nil), policies...), nil
	}
	deps.SnapshotServers = func() []Server {
		return append([]Server(nil), servers...)
	}
	deps.HandleScheduledRun = func(req PolicyScheduledRunRequest) PolicyScheduledRunResult {
		handled = append(handled, req)
		return PolicyScheduledRunResult{Handled: true, Inserted: true}
	}

	if err := NewPolicyService(deps).ProcessDueSlot(PolicyScheduleRequest{Now: slot}); err != nil {
		t.Fatalf("ProcessDueSlot() unexpected error: %v", err)
	}

	outcomes := map[string]string{}
	for _, req := range handled {
		outcomes[req.Server.Name+":"+req.Policy.Name] = req.Outcome
	}
	wantOutcomes := map[string]string{
		"srv-win:winner":        "",
		"srv-win:superseded":    updatePolicyRunReasonSuperseded,
		"srv-busy:busy":         "",
		"srv-missing:missing":   "",
		"srv-blackout:blackout": updatePolicyRunReasonBlackout,
	}
	if len(outcomes) != len(wantOutcomes) {
		t.Fatalf("outcomes = %+v, want exactly %+v", outcomes, wantOutcomes)
	}
	for key, want := range wantOutcomes {
		if outcomes[key] != want {
			t.Fatalf("outcome[%s] = %q, want %q; all requests=%+v", key, outcomes[key], want, handled)
		}
	}
	if _, exists := outcomes["srv-disabled:disabled"]; exists {
		t.Fatalf("disabled policy created a run request: %+v", handled)
	}
}

func TestPolicyServiceProcessDueRemembersAndReplaysMissedTicks(t *testing.T) {
	service := NewPolicyService(testPolicyServiceDeps())
	service.ResetMissedTicksForTest()
	t.Cleanup(service.ResetMissedTicksForTest)

	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	var handled []PolicyScheduledRunRequest
	deps := testPolicyServiceDeps()
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	deps.Maintenance = coordinator
	deps.ListPolicies = func() ([]UpdatePolicy, error) {
		return []UpdatePolicy{{
			ID:            7,
			Name:          "maintenance replay",
			Enabled:       true,
			TargetServers: []string{"srv"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
		}}, nil
	}
	deps.SnapshotServers = func() []Server {
		return []Server{{Name: "srv"}}
	}
	deps.HandleScheduledRun = func(req PolicyScheduledRunRequest) PolicyScheduledRunResult {
		handled = append(handled, req)
		return PolicyScheduledRunResult{Handled: true, Inserted: true}
	}

	service = NewPolicyService(deps)
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
	if len(handled) != 1 || handled[0].Outcome != updatePolicyRunReasonMaintenance {
		t.Fatalf("handled replay requests = %+v, want one maintenance skip", handled)
	}
}

func TestPolicyServiceWithoutMaintenanceCoordinatorProcessesDueWork(t *testing.T) {
	slot := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	deps := testPolicyServiceDeps()
	deps.ListPolicies = func() ([]UpdatePolicy, error) {
		return []UpdatePolicy{{
			ID:            8,
			Name:          "nightly",
			Enabled:       true,
			TargetServers: []string{"srv"},
			PackageScope:  updatePolicyPackageScopeSecurity,
			ExecutionMode: updatePolicyExecutionScanOnly,
			CadenceKind:   updatePolicyCadenceDaily,
			TimeLocal:     "03:00",
		}}, nil
	}
	deps.SnapshotServers = func() []Server {
		return []Server{{Name: "srv"}}
	}
	handled := 0
	deps.HandleScheduledRun = func(PolicyScheduledRunRequest) PolicyScheduledRunResult {
		handled++
		return PolicyScheduledRunResult{Handled: true, Inserted: true}
	}
	if err := NewPolicyService(deps).ProcessDue(slot); err != nil {
		t.Fatalf("ProcessDue() unexpected error: %v", err)
	}
	if handled != 1 {
		t.Fatalf("handled = %d, want 1", handled)
	}
}
