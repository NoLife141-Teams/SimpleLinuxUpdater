package updates

import (
	"fmt"
	"slices"

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
	fresh := result.Outcome
	disk := r.session.RunPlanDiskPrecheck(ctx, fresh.UpgradePlan)
	r.precheckResults = append(r.precheckResults, disk)
	if !disk.Passed {
		r.prechecksPassed = false
		r.precheckFailed = disk.Name
		return fresh, false, fmt.Errorf("approval revalidation disk pre-check failed: %s", disk.Details)
	}
	changed := !approvalPackageSetsEqual(approvalRemovals(r.approvalScope, previous.UpgradePlan), approvalRemovals(r.approvalScope, fresh.UpgradePlan)) ||
		!approvalPackageSetsEqual(approvalNewPackages(r.approvalScope, previous.UpgradePlan), approvalNewPackages(r.approvalScope, fresh.UpgradePlan)) ||
		!approvalPackageSetsEqual(PackagesForApprovalScope(r.approvalScope, previous.PendingUpdates), PackagesForApprovalScope(r.approvalScope, fresh.PendingUpdates))
	return fresh, changed, nil
}
