package updates

import (
	"fmt"
	"strings"

	"debian-updater/internal/servers"
)

const (
	ApprovalScopeAll              = "all"
	ApprovalScopeSecurity         = "security"
	ApprovalScopeSecurityKeptBack = "security_kept_back"
	ApprovalScopeFullUpgrade      = "full_upgrade"
)

const (
	ApprovalReasonAllowed                             = "allowed"
	ApprovalReasonNoKeptBackSecurityUpdates           = "no_kept_back_security_updates"
	ApprovalReasonKeptBackSecuritySimulationRequired  = "kept_back_security_simulation_required"
	ApprovalReasonKeptBackSecurityRemovalsUnconfirmed = "kept_back_security_removals_unconfirmed"
	ApprovalReasonFullUpgradeSimulationRequired       = "full_upgrade_simulation_required"
	ApprovalReasonFullUpgradeRemovalsUnconfirmed      = "full_upgrade_removals_unconfirmed"
	ApprovalReasonNoSecurityUpdates                   = "no_security_updates"
	ApprovalReasonNoKeptBackSecurityUpgrades          = "no_kept_back_security_upgrades"
	ApprovalReasonSecurityCommandUnavailable          = "security_command_unavailable"
	ApprovalReasonKeptBackSecurityCommandUnavailable  = "kept_back_security_command_unavailable"
	ApprovalReasonManualApprovalRequired              = "manual_approval_required"
	ApprovalReasonNoAutoApprovalScope                 = "no_auto_approval_scope"
)

const (
	ApprovalCommandModeAptUpgrade       = "apt_upgrade"
	ApprovalCommandModeAptFullUpgrade   = "apt_full_upgrade"
	ApprovalCommandModeSecurityUpgrade  = "security_upgrade"
	ApprovalCommandModeKeptBackInstall  = "kept_back_security_install"
	ApprovalCommandModeNoUpgradeSkipped = "no_upgrade_skipped"
)

type ApprovalScopeOptions struct {
	ConfirmRemovals bool
}

type ApprovalScopeStateOptions struct {
	ApproveStatus   string
	ClearPending    bool
	ConfirmRemovals bool
}

type ApprovalScopeInterpretation struct {
	Scope            string
	SelectedPackages []string
	CommandMode      string
	Command          string

	Allowed    bool
	Blocked    bool
	Skipped    bool
	ReasonCode string

	Message         string
	BodyMessage     string
	RemovedPackages []string

	AuditStatus  string
	AuditMessage string
	AuditMeta    map[string]any

	JobSummary     string
	SuccessMessage string

	RunnerApprovalLog string
	RunnerCommandLog  string
	RunnerErrorLog    string
	SkipUpgrade       bool

	StateOptions ApprovalScopeStateOptions
}

type ApprovalScopeAvailability struct {
	CanApproveAll              bool
	CanApproveSecurity         bool
	CanApproveKeptBackSecurity bool
	CanApproveFull             bool
}

func NormalizeApprovalScope(scope string) string {
	normalized := strings.ToLower(strings.TrimSpace(scope))
	switch normalized {
	case ApprovalScopeSecurity, ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade:
		return normalized
	default:
		return ApprovalScopeAll
	}
}

func EvaluateManualApproval(status *servers.ServerStatus, scope string, opts ApprovalScopeOptions) ApprovalScopeInterpretation {
	normalized := NormalizeApprovalScope(scope)
	result := allowedManualApproval(status, normalized, opts)
	if status == nil {
		return result
	}
	switch normalized {
	case ApprovalScopeSecurityKeptBack:
		packages := KeptBackSecurityPackagesFromPendingUpdates(status.PendingUpdates)
		result.SelectedPackages = packages
		if len(packages) == 0 {
			return blockedApproval(normalized, ApprovalReasonNoKeptBackSecurityUpdates, "ignored", "No kept-back security updates pending", "No kept-back security updates pending", nil)
		}
		if !status.UpgradePlan.KeptBackSecurityPlanAvailable {
			return blockedApproval(normalized, ApprovalReasonKeptBackSecuritySimulationRequired, "blocked", "Kept-back security upgrade requires a fresh targeted simulation", "Kept-back security upgrade requires a fresh package scan", nil)
		}
		if len(status.UpgradePlan.KeptBackSecurityRemovedPackages) > 0 && !opts.ConfirmRemovals {
			return blockedApproval(normalized, ApprovalReasonKeptBackSecurityRemovalsUnconfirmed, "blocked", "Kept-back security upgrade requires package removal confirmation", "Kept-back security upgrade may remove packages; confirmation required", status.UpgradePlan.KeptBackSecurityRemovedPackages)
		}
		result.AuditMeta = map[string]any{
			"scope":             normalized,
			"approved_packages": append([]string(nil), packages...),
			"new_packages":      append([]string(nil), status.UpgradePlan.KeptBackSecurityNewPackages...),
			"removed_packages":  append([]string(nil), status.UpgradePlan.KeptBackSecurityRemovedPackages...),
		}
	case ApprovalScopeFullUpgrade:
		result.SelectedPackages = FullUpgradePackagesFromPendingUpdates(status.PendingUpdates)
		if !status.UpgradePlan.FullUpgradePlanAvailable {
			return blockedApproval(normalized, ApprovalReasonFullUpgradeSimulationRequired, "blocked", "Full upgrade requires a fresh full-upgrade simulation", "Full upgrade requires a fresh package scan", nil)
		}
		if len(status.UpgradePlan.FullUpgradeRemovedPackages) > 0 && !opts.ConfirmRemovals {
			return blockedApproval(normalized, ApprovalReasonFullUpgradeRemovalsUnconfirmed, "blocked", "Full upgrade requires package removal confirmation", "Full upgrade would remove packages; confirmation required", status.UpgradePlan.FullUpgradeRemovedPackages)
		}
		result.AuditMeta = map[string]any{
			"scope":            normalized,
			"removed_packages": append([]string(nil), status.UpgradePlan.FullUpgradeRemovedPackages...),
		}
	case ApprovalScopeSecurity:
		result.SelectedPackages = SecurityPackagesFromPendingUpdates(status.PendingUpdates)
	default:
		result.SelectedPackages = PackageNamesFromPendingUpdates(status.PendingUpdates)
	}
	return result
}

func InterpretApprovedScope(scope string, pending []servers.PendingUpdate, plan servers.UpgradePlan, opts ApprovalScopeOptions) ApprovalScopeInterpretation {
	normalized := NormalizeApprovalScope(scope)
	selected := PackagesForApprovalScope(normalized, pending)
	result := ApprovalScopeInterpretation{
		Scope:            normalized,
		SelectedPackages: selected,
		Allowed:          true,
		ReasonCode:       ApprovalReasonAllowed,
		StateOptions: ApprovalScopeStateOptions{
			ClearPending:    true,
			ConfirmRemovals: opts.ConfirmRemovals,
		},
	}
	if normalized == ApprovalScopeSecurity && len(selected) == 0 {
		result.Allowed = false
		result.Skipped = true
		result.SkipUpgrade = true
		result.CommandMode = ApprovalCommandModeNoUpgradeSkipped
		result.ReasonCode = ApprovalReasonNoSecurityUpdates
		result.RunnerApprovalLog = "\nApproval received: security-only upgrade.\nNo security upgrades detected in pending package set; skipped upgrade."
		return result
	}
	if normalized == ApprovalScopeSecurityKeptBack && len(selected) == 0 {
		result.Allowed = false
		result.Skipped = true
		result.SkipUpgrade = true
		result.CommandMode = ApprovalCommandModeNoUpgradeSkipped
		result.ReasonCode = ApprovalReasonNoKeptBackSecurityUpgrades
		result.RunnerApprovalLog = "\nApproval received: kept-back security upgrade.\nNo kept-back security upgrades detected in pending package set; skipped upgrade."
		return result
	}
	switch normalized {
	case ApprovalScopeSecurity:
		result.RunnerApprovalLog = fmt.Sprintf("\nApproval received: security-only upgrade (%d package(s)).", len(selected))
		result.Command = BuildSelectedUpgradeCmd(selected)
		result.CommandMode = ApprovalCommandModeSecurityUpgrade
		result.RunnerCommandLog = "\nRunning security-only apt upgrade..."
		if result.Command == "" {
			return blockedRunner(result, ApprovalReasonSecurityCommandUnavailable, "could not build security-only apt command from approved package set")
		}
	case ApprovalScopeSecurityKeptBack:
		result.RunnerApprovalLog = fmt.Sprintf("\nApproval received: kept-back security upgrade (%d package(s)).", len(selected))
		if !plan.KeptBackSecurityPlanAvailable {
			return blockedRunner(result, ApprovalReasonKeptBackSecuritySimulationRequired, "kept-back security upgrade requires a fresh targeted simulation")
		}
		if len(plan.KeptBackSecurityRemovedPackages) > 0 && !opts.ConfirmRemovals {
			return blockedRunner(result, ApprovalReasonKeptBackSecurityRemovalsUnconfirmed, "kept-back security upgrade would remove packages but removal confirmation was not recorded")
		}
		result.Command = BuildSelectedInstallCmd(selected)
		result.CommandMode = ApprovalCommandModeKeptBackInstall
		result.RunnerCommandLog = "\nRunning kept-back security apt install..."
		if result.Command == "" {
			return blockedRunner(result, ApprovalReasonKeptBackSecurityCommandUnavailable, "could not build kept-back security apt command from approved package set")
		}
	case ApprovalScopeFullUpgrade:
		result.RunnerApprovalLog = fmt.Sprintf("\nApproval received: full upgrade (%d package(s), %d new, %d remove).", len(selected), len(plan.FullUpgradeNewPackages), len(plan.FullUpgradeRemovedPackages))
		if !plan.FullUpgradePlanAvailable {
			return blockedRunner(result, ApprovalReasonFullUpgradeSimulationRequired, "full upgrade requires a successful full-upgrade simulation")
		}
		if len(plan.FullUpgradeRemovedPackages) > 0 && !opts.ConfirmRemovals {
			return blockedRunner(result, ApprovalReasonFullUpgradeRemovalsUnconfirmed, "full upgrade would remove packages but removal confirmation was not recorded")
		}
		result.Command = AptFullUpgradeCmd
		result.CommandMode = ApprovalCommandModeAptFullUpgrade
		result.RunnerCommandLog = "\nRunning apt full-upgrade..."
	default:
		result.RunnerApprovalLog = "\nApproval received: all pending upgrades."
		result.Command = AptUpgradeCmd
		result.CommandMode = ApprovalCommandModeAptUpgrade
		result.RunnerCommandLog = "\nRunning apt upgrade..."
	}
	return result
}

func EvaluateAutoApproval(scope string, pending []servers.PendingUpdate, plan servers.UpgradePlan) ApprovalScopeInterpretation {
	if strings.TrimSpace(scope) == "" {
		return ApprovalScopeInterpretation{
			Scope:      "",
			Allowed:    false,
			ReasonCode: ApprovalReasonNoAutoApprovalScope,
		}
	}
	normalized := NormalizeApprovalScope(scope)
	if normalized == ApprovalScopeSecurityKeptBack {
		return manualAutoApproval(normalized, ApprovalReasonManualApprovalRequired, "\nKept-back security updates require manual approval after targeted simulation.")
	}
	if normalized == ApprovalScopeFullUpgrade && !plan.FullUpgradePlanAvailable {
		return manualAutoApproval(normalized, ApprovalReasonFullUpgradeSimulationRequired, "\nScheduled full-upgrade requires a successful full-upgrade simulation before it can run.")
	}
	if normalized == ApprovalScopeFullUpgrade && len(plan.FullUpgradeRemovedPackages) > 0 {
		return manualAutoApproval(normalized, ApprovalReasonFullUpgradeRemovalsUnconfirmed, "\nScheduled full-upgrade requires manual confirmation because package removals were detected.")
	}
	return ApprovalScopeInterpretation{
		Scope:            normalized,
		SelectedPackages: PackagesForApprovalScope(normalized, pending),
		Allowed:          true,
		ReasonCode:       ApprovalReasonAllowed,
		StateOptions: ApprovalScopeStateOptions{
			ApproveStatus:   "approved",
			ConfirmRemovals: false,
		},
	}
}

func PackagesForApprovalScope(scope string, pending []servers.PendingUpdate) []string {
	switch NormalizeApprovalScope(scope) {
	case ApprovalScopeSecurity:
		return SecurityPackagesFromPendingUpdates(pending)
	case ApprovalScopeSecurityKeptBack:
		return KeptBackSecurityPackagesFromPendingUpdates(pending)
	case ApprovalScopeFullUpgrade:
		return FullUpgradePackagesFromPendingUpdates(pending)
	default:
		return PackageNamesFromPendingUpdates(pending)
	}
}

func ApprovalAvailability(status *servers.ServerStatus) ApprovalScopeAvailability {
	if status == nil || strings.ToLower(strings.TrimSpace(status.Status)) != "pending_approval" {
		return ApprovalScopeAvailability{}
	}
	standardPackages := 0
	standardSecurity := 0
	keptBackSecurity := 0
	plan := status.UpgradePlan
	if plan.StandardPackageCount > 0 || plan.KeptBackPackageCount > 0 || plan.FullUpgradePackageCount > 0 {
		standardPackages = plan.StandardPackageCount
		standardSecurity = plan.StandardSecurityCount
		keptBackSecurity = plan.TotalSecurityCount - plan.StandardSecurityCount
		if keptBackSecurity < 0 {
			keptBackSecurity = 0
		}
		return ApprovalScopeAvailability{
			CanApproveAll:              standardPackages > 0,
			CanApproveSecurity:         standardSecurity > 0,
			CanApproveKeptBackSecurity: keptBackSecurity > 0 && plan.KeptBackSecurityPlanAvailable,
			CanApproveFull:             plan.FullUpgradePlanAvailable && (plan.KeptBackPackageCount > 0 || len(plan.FullUpgradeNewPackages) > 0 || len(plan.FullUpgradeRemovedPackages) > 0),
		}
	}
	keptBackPackages := 0
	for _, update := range status.PendingUpdates {
		if update.RequiresFull || update.KeptBack {
			keptBackPackages++
			if update.Security {
				keptBackSecurity++
			}
			continue
		}
		standardPackages++
		if update.Security {
			standardSecurity++
		}
	}
	_ = keptBackPackages
	return ApprovalScopeAvailability{
		CanApproveAll:      standardPackages > 0,
		CanApproveSecurity: standardSecurity > 0,
	}
}

func allowedManualApproval(status *servers.ServerStatus, scope string, opts ApprovalScopeOptions) ApprovalScopeInterpretation {
	confirmRemovals := opts.ConfirmRemovals
	result := ApprovalScopeInterpretation{
		Scope:          scope,
		Allowed:        true,
		ReasonCode:     ApprovalReasonAllowed,
		AuditStatus:    "success",
		AuditMeta:      map[string]any{"scope": scope},
		StateOptions:   ApprovalScopeStateOptions{ApproveStatus: "approved", ConfirmRemovals: confirmRemovals},
		SuccessMessage: approvalSuccessMessage(scope),
		JobSummary:     approvalJobSummary(scope),
		AuditMessage:   approvalSuccessMessage(scope),
		Message:        approvalSuccessMessage(scope),
		BodyMessage:    approvalSuccessMessage(scope),
	}
	if status == nil {
		return result
	}
	return result
}

func blockedApproval(scope, reason, auditStatus, auditMessage, bodyMessage string, removed []string) ApprovalScopeInterpretation {
	meta := map[string]any{"scope": scope}
	if len(removed) > 0 {
		meta["removed_packages"] = append([]string(nil), removed...)
	}
	return ApprovalScopeInterpretation{
		Scope:           scope,
		Allowed:         false,
		Blocked:         auditStatus == "blocked",
		ReasonCode:      reason,
		Message:         auditMessage,
		BodyMessage:     bodyMessage,
		RemovedPackages: append([]string(nil), removed...),
		AuditStatus:     auditStatus,
		AuditMessage:    auditMessage,
		AuditMeta:       meta,
	}
}

func blockedRunner(result ApprovalScopeInterpretation, reason, message string) ApprovalScopeInterpretation {
	result.Allowed = false
	result.Blocked = true
	result.ReasonCode = reason
	result.Message = message
	result.RunnerErrorLog = "\nError: " + message
	result.Command = ""
	return result
}

func manualAutoApproval(scope, reason, logMessage string) ApprovalScopeInterpretation {
	return ApprovalScopeInterpretation{
		Scope:            scope,
		Allowed:          false,
		ReasonCode:       reason,
		RunnerCommandLog: logMessage,
	}
}

func approvalSuccessMessage(scope string) string {
	switch scope {
	case ApprovalScopeSecurity:
		return "Security updates approved"
	case ApprovalScopeSecurityKeptBack:
		return "Kept-back security updates approved"
	case ApprovalScopeFullUpgrade:
		return "Full upgrade approved"
	default:
		return "All pending updates approved"
	}
}

func approvalJobSummary(scope string) string {
	switch scope {
	case ApprovalScopeSecurity:
		return "Security updates approved"
	case ApprovalScopeSecurityKeptBack:
		return "Kept-back security updates approved"
	case ApprovalScopeFullUpgrade:
		return "Full upgrade approved"
	default:
		return "All pending updates approved"
	}
}
