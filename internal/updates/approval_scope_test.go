package updates

import (
	"reflect"
	"testing"

	"debian-updater/internal/servers"
)

func TestApprovalScopeNormalize(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ApprovalScopeAll},
		{"wat", ApprovalScopeAll},
		{" ALL ", ApprovalScopeAll},
		{"security", ApprovalScopeSecurity},
		{" SECURITY_KEPT_BACK ", ApprovalScopeSecurityKeptBack},
		{"full_upgrade", ApprovalScopeFullUpgrade},
	}
	for _, tt := range tests {
		if got := NormalizeApprovalScope(tt.in); got != tt.want {
			t.Fatalf("NormalizeApprovalScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestApprovalScopeManualEvaluation(t *testing.T) {
	pending := []servers.PendingUpdate{
		{Package: "openssl", Security: true},
		{Package: "linux-image-amd64", InstallPackage: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
	}

	t.Run("kept back without packages is ignored", func(t *testing.T) {
		got := EvaluateManualApproval(&servers.ServerStatus{Name: "srv", PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true}}}, ApprovalScopeSecurityKeptBack, ApprovalScopeOptions{})
		if got.Allowed || got.AuditStatus != "ignored" || got.BodyMessage != "No kept-back security updates pending" || got.ReasonCode != ApprovalReasonNoKeptBackSecurityUpdates {
			t.Fatalf("manual kept-back no packages = %+v", got)
		}
	})

	t.Run("kept back requires fresh targeted simulation", func(t *testing.T) {
		got := EvaluateManualApproval(&servers.ServerStatus{Name: "srv", PendingUpdates: pending}, ApprovalScopeSecurityKeptBack, ApprovalScopeOptions{})
		if got.Allowed || !got.Blocked || got.AuditMessage != "Kept-back security upgrade requires a fresh targeted simulation" || got.BodyMessage != "Kept-back security upgrade requires a fresh package scan" {
			t.Fatalf("manual kept-back stale simulation = %+v", got)
		}
	})

	t.Run("kept back removals require confirmation", func(t *testing.T) {
		status := &servers.ServerStatus{
			Name:           "srv",
			PendingUpdates: pending,
			UpgradePlan: servers.UpgradePlan{
				KeptBackSecurityPlanAvailable:   true,
				KeptBackSecurityRemovedPackages: []string{"obsolete-kernel"},
			},
		}
		got := EvaluateManualApproval(status, ApprovalScopeSecurityKeptBack, ApprovalScopeOptions{})
		if got.Allowed || got.BodyMessage != "Kept-back security upgrade may remove packages; confirmation required" || !reflect.DeepEqual(got.RemovedPackages, []string{"obsolete-kernel"}) {
			t.Fatalf("manual kept-back removals = %+v", got)
		}
	})

	t.Run("kept back success includes audit metadata and state options", func(t *testing.T) {
		status := &servers.ServerStatus{
			Name:           "srv",
			PendingUpdates: pending,
			UpgradePlan: servers.UpgradePlan{
				KeptBackSecurityPlanAvailable: true,
				KeptBackSecurityNewPackages:   []string{"linux-image-6.1.0-39-amd64"},
			},
		}
		got := EvaluateManualApproval(status, ApprovalScopeSecurityKeptBack, ApprovalScopeOptions{ConfirmRemovals: true})
		if !got.Allowed || got.JobSummary != "Kept-back security updates approved" || !got.StateOptions.ConfirmRemovals {
			t.Fatalf("manual kept-back success = %+v", got)
		}
		if !reflect.DeepEqual(got.AuditMeta["approved_packages"], []string{"linux-image-amd64"}) {
			t.Fatalf("approved_packages meta = %#v", got.AuditMeta["approved_packages"])
		}
	})

	t.Run("full upgrade requires fresh simulation", func(t *testing.T) {
		got := EvaluateManualApproval(&servers.ServerStatus{Name: "srv", PendingUpdates: pending}, ApprovalScopeFullUpgrade, ApprovalScopeOptions{})
		if got.Allowed || got.AuditMessage != "Full upgrade requires a fresh full-upgrade simulation" || got.BodyMessage != "Full upgrade requires a fresh package scan" {
			t.Fatalf("manual full stale simulation = %+v", got)
		}
	})

	t.Run("full upgrade removals require confirmation", func(t *testing.T) {
		status := &servers.ServerStatus{
			Name:           "srv",
			PendingUpdates: pending,
			UpgradePlan: servers.UpgradePlan{
				FullUpgradePlanAvailable:   true,
				FullUpgradeRemovedPackages: []string{"old-lib"},
			},
		}
		got := EvaluateManualApproval(status, ApprovalScopeFullUpgrade, ApprovalScopeOptions{})
		if got.Allowed || got.BodyMessage != "Full upgrade would remove packages; confirmation required" || !reflect.DeepEqual(got.RemovedPackages, []string{"old-lib"}) {
			t.Fatalf("manual full removals = %+v", got)
		}
	})
}

func TestApprovalScopeRunnerInterpretation(t *testing.T) {
	pending := []servers.PendingUpdate{
		{Package: "openssl", Security: true},
		{Package: "bash", Security: false},
		{Package: "linux-image-amd64", InstallPackage: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
	}

	t.Run("security chooses selected upgrade command", func(t *testing.T) {
		got := InterpretApprovedScope(ApprovalScopeSecurity, pending, servers.UpgradePlan{}, ApprovalScopeOptions{})
		wantCmd := BuildSelectedUpgradeCmd([]string{"openssl"})
		if !got.Allowed || got.CommandMode != ApprovalCommandModeSecurityUpgrade || got.Command != wantCmd || !reflect.DeepEqual(got.SelectedPackages, []string{"openssl"}) {
			t.Fatalf("security interpretation = %+v, want cmd %q", got, wantCmd)
		}
	})

	t.Run("security with no packages skips upgrade", func(t *testing.T) {
		got := InterpretApprovedScope(ApprovalScopeSecurity, []servers.PendingUpdate{{Package: "bash"}}, servers.UpgradePlan{}, ApprovalScopeOptions{})
		if !got.SkipUpgrade || got.ReasonCode != ApprovalReasonNoSecurityUpdates || got.RunnerApprovalLog != "\nApproval received: security-only upgrade.\nNo security upgrades detected in pending package set; skipped upgrade." {
			t.Fatalf("security skip = %+v", got)
		}
	})

	t.Run("kept back chooses selected install command", func(t *testing.T) {
		got := InterpretApprovedScope(ApprovalScopeSecurityKeptBack, pending, servers.UpgradePlan{KeptBackSecurityPlanAvailable: true}, ApprovalScopeOptions{})
		wantCmd := BuildSelectedInstallCmd([]string{"linux-image-amd64"})
		if !got.Allowed || got.CommandMode != ApprovalCommandModeKeptBackInstall || got.Command != wantCmd {
			t.Fatalf("kept-back interpretation = %+v, want cmd %q", got, wantCmd)
		}
	})

	t.Run("runner preserves full upgrade guardrails", func(t *testing.T) {
		got := InterpretApprovedScope(ApprovalScopeFullUpgrade, pending, servers.UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradeRemovedPackages: []string{"old-lib"}}, ApprovalScopeOptions{})
		if got.Allowed || got.ReasonCode != ApprovalReasonFullUpgradeRemovalsUnconfirmed || got.RunnerErrorLog != "\nError: full upgrade would remove packages but removal confirmation was not recorded" {
			t.Fatalf("full removal guard = %+v", got)
		}
		confirmed := InterpretApprovedScope(ApprovalScopeFullUpgrade, pending, servers.UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradeRemovedPackages: []string{"old-lib"}}, ApprovalScopeOptions{ConfirmRemovals: true})
		if !confirmed.Allowed || confirmed.Command != AptFullUpgradeCmd || confirmed.CommandMode != ApprovalCommandModeAptFullUpgrade {
			t.Fatalf("confirmed full interpretation = %+v", confirmed)
		}
	})
}

func TestApprovalScopeAutoApproval(t *testing.T) {
	pending := []servers.PendingUpdate{{Package: "openssl", Security: true}}

	if got := EvaluateAutoApproval("", pending, servers.UpgradePlan{}); got.Allowed || got.ReasonCode != ApprovalReasonNoAutoApprovalScope {
		t.Fatalf("empty auto approval = %+v", got)
	}
	if got := EvaluateAutoApproval("wat", pending, servers.UpgradePlan{}); !got.Allowed || got.Scope != ApprovalScopeAll || !reflect.DeepEqual(got.SelectedPackages, []string{"openssl"}) {
		t.Fatalf("unknown auto approval = %+v", got)
	}
	if got := EvaluateAutoApproval(ApprovalScopeSecurityKeptBack, pending, servers.UpgradePlan{}); got.Allowed || got.RunnerCommandLog != "\nKept-back security updates require manual approval after targeted simulation." {
		t.Fatalf("kept-back auto approval = %+v", got)
	}
	if got := EvaluateAutoApproval(ApprovalScopeFullUpgrade, pending, servers.UpgradePlan{}); got.Allowed || got.RunnerCommandLog != "\nScheduled full-upgrade requires a successful full-upgrade simulation before it can run." {
		t.Fatalf("full stale auto approval = %+v", got)
	}
	if got := EvaluateAutoApproval(ApprovalScopeFullUpgrade, pending, servers.UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradeRemovedPackages: []string{"old-lib"}}); got.Allowed || got.RunnerCommandLog != "\nScheduled full-upgrade requires manual confirmation because package removals were detected." {
		t.Fatalf("full removals auto approval = %+v", got)
	}
	if got := EvaluateAutoApproval(ApprovalScopeSecurity, pending, servers.UpgradePlan{}); !got.Allowed || got.Scope != ApprovalScopeSecurity || !reflect.DeepEqual(got.SelectedPackages, []string{"openssl"}) {
		t.Fatalf("security auto approval = %+v", got)
	}
}

func TestApprovalScopeAvailability(t *testing.T) {
	status := &servers.ServerStatus{
		Status: "pending_approval",
		UpgradePlan: servers.UpgradePlan{
			StandardPackageCount:          1,
			StandardSecurityCount:         1,
			KeptBackPackageCount:          1,
			TotalSecurityCount:            2,
			FullUpgradePlanAvailable:      true,
			FullUpgradeNewPackages:        []string{"linux-image-6.1.0-39-amd64"},
			KeptBackSecurityPlanAvailable: true,
		},
	}
	got := ApprovalAvailability(status)
	if !got.CanApproveAll || !got.CanApproveSecurity || !got.CanApproveKeptBackSecurity || !got.CanApproveFull {
		t.Fatalf("plan availability = %+v", got)
	}

	fallback := ApprovalAvailability(&servers.ServerStatus{
		Status: "pending_approval",
		PendingUpdates: []servers.PendingUpdate{
			{Package: "openssl", Security: true},
			{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
		},
	})
	if !fallback.CanApproveAll || !fallback.CanApproveSecurity || fallback.CanApproveKeptBackSecurity || fallback.CanApproveFull {
		t.Fatalf("fallback availability = %+v", fallback)
	}

	if got := ApprovalAvailability(&servers.ServerStatus{Status: "updating"}); got.CanApproveAll || got.CanApproveSecurity || got.CanApproveKeptBackSecurity || got.CanApproveFull {
		t.Fatalf("non-pending availability = %+v", got)
	}
}
