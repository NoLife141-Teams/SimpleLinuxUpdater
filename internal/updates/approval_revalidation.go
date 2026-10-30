package updates

import (
	"cmp"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"debian-updater/internal/servers"
)

func approvalPackageSetsEqual(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(slices.Compact(a), slices.Compact(b))
}

func approvalRemovals(scope string, plan servers.UpgradePlan) []string {
	if scope == ApprovalScopeFullUpgrade {
		return plan.FullUpgradeRemovedPackages
	}
	if scope == ApprovalScopeSecurityKeptBack {
		return plan.KeptBackSecurityRemovedPackages
	}
	return nil
}

func approvalNewPackages(scope string, plan servers.UpgradePlan) []string {
	if scope == ApprovalScopeFullUpgrade {
		return plan.FullUpgradeNewPackages
	}
	if scope == ApprovalScopeSecurityKeptBack {
		return plan.KeptBackSecurityNewPackages
	}
	return nil
}

// Compare the package facts shown for the selected operation. CVE enrichment
// is deliberately excluded: it may finish while an approval is pending, but
// it does not change the package that APT will install.
type approvalPackageIdentity struct {
	Package, InstallPackage, CurrentVersion, CandidateVersion, Source, Raw string
	SourcePackage, InstalledSourceVersion, CandidateSourceVersion          string
	Security, KeptBack, RequiresFull                                       bool
}

func approvalSelectedPackagesEqual(scope string, previous, fresh []servers.PendingUpdate) bool {
	selectedBefore := PackagesForApprovalScope(scope, previous)
	selectedAfter := PackagesForApprovalScope(scope, fresh)
	if !approvalPackageSetsEqual(selectedBefore, selectedAfter) {
		return false
	}
	identities := func(updates []servers.PendingUpdate, selected []string) []approvalPackageIdentity {
		wanted := make(map[string]struct{}, len(selected))
		for _, name := range selected {
			wanted[name] = struct{}{}
		}
		result := make([]approvalPackageIdentity, 0, len(selected))
		for _, update := range updates {
			selector := update.Package
			if NormalizeApprovalScope(scope) == ApprovalScopeSecurityKeptBack && strings.TrimSpace(update.InstallPackage) != "" {
				selector = strings.TrimSpace(update.InstallPackage)
			}
			if _, ok := wanted[selector]; !ok {
				continue
			}
			result = append(result, approvalPackageIdentity{
				Package: update.Package, InstallPackage: update.InstallPackage,
				SourcePackage: update.SourcePackage, InstalledSourceVersion: update.InstalledSourceVersion, CandidateSourceVersion: update.CandidateSourceVersion,
				CurrentVersion: update.CurrentVersion, CandidateVersion: update.CandidateVersion,
				Source: update.Source, Raw: update.Raw,
				Security: update.Security, KeptBack: update.KeptBack, RequiresFull: update.RequiresFull,
			})
		}
		slices.SortFunc(result, func(a, b approvalPackageIdentity) int {
			return cmp.Or(strings.Compare(a.Package, b.Package), strings.Compare(a.InstallPackage, b.InstallPackage),
				strings.Compare(a.CurrentVersion, b.CurrentVersion), strings.Compare(a.CandidateVersion, b.CandidateVersion),
				strings.Compare(a.Source, b.Source), strings.Compare(a.Raw, b.Raw))
		})
		return result
	}
	return slices.Equal(identities(previous, selectedBefore), identities(fresh, selectedAfter))
}

func (r *withActorRunner) revalidateApproval(previous PackageDiscoveryOutcome) (PackageDiscoveryOutcome, bool, error) {
	ctx := r.maintenanceContext()
	if r.session == nil && !r.setupSSH("update.ssh_dial") {
		return previous, false, fmt.Errorf("approval reconnect failed")
	}
	checks := r.session.RunUpdatePrechecks(ctx)
	r.precheckResults = append(r.precheckResults, checks.Results...)
	if !checks.AllPassed {
		r.prechecksPassed = false
		r.precheckFailed = checks.FailedCheck
		return previous, false, fmt.Errorf("approval revalidation pre-check failed: %s", checks.FailedCheck)
	}
	result, err := r.session.DiscoverPackages(ctx, HostOperationRequest{Operation: "update.revalidate_approval"})
	r.listUpgradableAttempts += result.Attempts
	if err != nil {
		return previous, false, err
	}
	fresh, err := r.applyDeferrals(result.Outcome)
	if err != nil {
		return previous, false, err
	}
	disk := r.session.RunPlanDiskPrecheck(ctx, fresh.UpgradePlan)
	r.precheckResults = append(r.precheckResults, disk)
	if !disk.Passed {
		r.prechecksPassed = false
		r.precheckFailed = disk.Name
		return fresh, false, fmt.Errorf("approval revalidation disk pre-check failed: %s", disk.Details)
	}
	changed := previous.UpgradePlan.PackageDeferrals.Revision != fresh.UpgradePlan.PackageDeferrals.Revision || !reflect.DeepEqual(previous.UpgradePlan.DeferralScopes, fresh.UpgradePlan.DeferralScopes) || !approvalPackageSetsEqual(approvalRemovals(r.approvalScope, previous.UpgradePlan), approvalRemovals(r.approvalScope, fresh.UpgradePlan)) ||
		!approvalPackageSetsEqual(approvalNewPackages(r.approvalScope, previous.UpgradePlan), approvalNewPackages(r.approvalScope, fresh.UpgradePlan)) ||
		!approvalSelectedPackagesEqual(r.approvalScope, previous.PendingUpdates, fresh.PendingUpdates)
	return fresh, changed, nil
}

// A remediation approval binds the complete observed package transaction, including
// scopes not currently selected. A changed candidate or dependency requires new consent.
func remediationDiscoveryEqual(a, b PackageDiscoveryOutcome) bool {
	if !approvalSelectedPackagesEqual(ApprovalScopeFullUpgrade, a.PendingUpdates, b.PendingUpdates) {
		return false
	}
	ap, bp := a.UpgradePlan, b.UpgradePlan
	return ap.PackageDeferrals.Revision == bp.PackageDeferrals.Revision && reflect.DeepEqual(ap.DeferralScopes, bp.DeferralScopes) &&
		ap.FullUpgradePlanAvailable == bp.FullUpgradePlanAvailable && ap.KeptBackSecurityPlanAvailable == bp.KeptBackSecurityPlanAvailable &&
		approvalPackageSetsEqual(ap.FullUpgradeNewPackages, bp.FullUpgradeNewPackages) && approvalPackageSetsEqual(ap.FullUpgradeRemovedPackages, bp.FullUpgradeRemovedPackages) &&
		approvalPackageSetsEqual(ap.KeptBackSecurityNewPackages, bp.KeptBackSecurityNewPackages) && approvalPackageSetsEqual(ap.KeptBackSecurityRemovedPackages, bp.KeptBackSecurityRemovedPackages)
}

func (r *withActorRunner) readRemediationInventory() ([]servers.PendingUpdate, string, error) {
	if r.session == nil {
		if !r.setupSSH("update.ssh_dial") {
			return nil, "", fmt.Errorf("inventory reconnect failed")
		}
	}
	result, err := r.session.RunCommand(r.maintenanceContext(), HostCommandRequest{Operation: "remediation.installed_inventory", Command: BuildInstalledPackageInventoryCmd(), Effect: HostCommandEffectReadOnly, ReplayPolicy: ReplayNever})
	if err != nil {
		return nil, "", err
	}
	packages, err := parseInstalledPackages(result.Stdout)
	if err != nil {
		return nil, "", err
	}
	distro := installedInventoryDistro(result.Stdout)
	if distro == "" {
		return nil, "", fmt.Errorf("distribution identity unavailable")
	}
	slices.SortFunc(packages, func(a, b servers.PendingUpdate) int { return strings.Compare(a.Package, b.Package) })
	return packages, distro, nil
}
