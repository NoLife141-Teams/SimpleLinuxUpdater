package updates

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"debian-updater/internal/deferrals"
	"debian-updater/internal/servers"
)

// Build a targeted, simulated transaction for every scope when exceptions are
// active. The unrestricted discovery remains visible to security inventory.
func PlanPackageDeferrals(ctx context.Context, session HostMaintenanceSession, outcome PackageDiscoveryOutcome, rules deferrals.Snapshot) (PackageDiscoveryOutcome, error) {
	outcome = outcome.Clone()
	outcome.UpgradePlan.PackageDeferrals = rules
	if len(rules.Rules) == 0 {
		return outcome, nil
	}
	for i, u := range outcome.PendingUpdates {
		selector := u.InstallPackage
		if selector == "" {
			if strings.Contains(strings.SplitN(u.Raw, " ", 2)[0], "/") {
				selector = aptListMetadataSelector(u).selector
			} else if m := deferralInstallLine.FindStringSubmatch("Inst " + u.Raw); m != nil {
				selector = u.Package
				if !strings.Contains(selector, ":") {
					selector += ":" + m[4]
				}
			}
		}
		if selector == "" {
			selector = u.Package
		}
		outcome.PendingUpdates[i].Package = selector
		outcome.PendingUpdates[i].Deferred = rules.Matches(selector)
		if outcome.PendingUpdates[i].Deferred {
			outcome.UpgradePlan.DeferredPackages = append(outcome.UpgradePlan.DeferredPackages, selector)
			if u.Security {
				outcome.UpgradePlan.DeferredSecurityPackages = append(outcome.UpgradePlan.DeferredSecurityPackages, selector)
			}
		}
	}
	outcome.UpgradePlan.DeferralScopes = map[string]servers.DeferralScopePlan{}
	for _, scope := range []string{ApprovalScopeAll, ApprovalScopeSecurity, ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade} {
		selected := PackagesForApprovalScope(scope, outcome.PendingUpdates)
		p := servers.DeferralScopePlan{SelectedPackages: selected, Transaction: []string{}, NewPackages: []string{}, RemovedPackages: []string{}, DependencyImpacts: []string{}}
		if len(selected) == 0 {
			p.Available = true
			outcome.UpgradePlan.DeferralScopes[scope] = p
			continue
		}
		args := validatedShellEscapedPackageArgs(selected)
		if len(args) == 0 {
			return outcome, fmt.Errorf("invalid package selector in deferral plan")
		}
		command := "dpkg-query -W -f='SLU_INSTALLED_ARCH ${binary:Package} ${Architecture}\\n' && " + ReadOnlyAptCommand("apt-get -s -o APT::Ignore-Hold=false -o APT::Get::allow-change-held-packages=false -o APT::Get::allow-downgrades=false "+deferralInstallOptions(scope, true)+" install -- "+strings.Join(args, " "))
		result, err := session.RunCommand(ctx, HostCommandRequest{Operation: "update.deferral_plan." + scope, Command: command, Effect: HostCommandEffectReadOnly, ReplayPolicy: ReplayNever})
		if err != nil {
			p.BlockReason = "APT cannot demonstrate a safe transaction with these package deferrals. Review dependencies and rescan."
		} else {
			p.Transaction, p.NewPackages, p.RemovedPackages, err = parseDeferralTransaction(result.Stdout + "\n" + result.Stderr)
			if err != nil {
				p.BlockReason = err.Error()
			} else {
				for _, change := range p.Transaction {
					selector, _, _ := strings.Cut(change, "=")
					if rules.Matches(selector) {
						p.DependencyImpacts = append(p.DependencyImpacts, selector)
					}
				}
				if len(p.Transaction) == 0 {
					p.BlockReason = "APT found no executable transaction; review administrator holds/pins and rescan."
				} else if len(p.DependencyImpacts) > 0 {
					p.BlockReason = "Dependency conflict: APT would change deferred package(s): " + strings.Join(p.DependencyImpacts, ", ") + ". Remove or wait for the deferral, or resolve dependencies before rescanning."
				} else {
					p.Available = true
				}
			}
		}
		outcome.UpgradePlan.DeferralScopes[scope] = p
	}
	all := outcome.UpgradePlan.DeferralScopes[ApprovalScopeAll]
	security := outcome.UpgradePlan.DeferralScopes[ApprovalScopeSecurity]
	kept := outcome.UpgradePlan.DeferralScopes[ApprovalScopeSecurityKeptBack]
	full := outcome.UpgradePlan.DeferralScopes[ApprovalScopeFullUpgrade]
	outcome.UpgradePlan.StandardPackageCount = len(all.SelectedPackages)
	outcome.UpgradePlan.StandardSecurityCount = len(security.SelectedPackages)
	outcome.UpgradePlan.KeptBackSecurityPlanAvailable = kept.Available
	outcome.UpgradePlan.KeptBackSecurityPackageCount = len(kept.SelectedPackages)
	outcome.UpgradePlan.KeptBackSecurityNewPackages = kept.NewPackages
	outcome.UpgradePlan.KeptBackSecurityRemovedPackages = kept.RemovedPackages
	outcome.UpgradePlan.FullUpgradePlanAvailable = full.Available
	outcome.UpgradePlan.FullUpgradePackageCount = len(full.Transaction)
	outcome.UpgradePlan.FullUpgradeNewPackages = full.NewPackages
	outcome.UpgradePlan.FullUpgradeRemovedPackages = full.RemovedPackages
	outcome.UpgradePlan.FullUpgradeDiskFactsAvailable = false
	outcome.UpgradePlan.KeptBackDiskFactsAvailable = false
	applyPlanDiskEstimate(&outcome.UpgradePlan)
	return outcome, nil
}

var deferralInstallLine = regexp.MustCompile(`^Inst ([a-z0-9][a-z0-9+.:_-]*) (?:\[([^\]]+)\] )?\(([^ ]+) .*\[([a-z0-9-]+)\]\)$`)
var deferralRemoveLine = regexp.MustCompile(`^Remv ([a-z0-9][a-z0-9+.:_-]*) \[([^\]]+)\](?: .*)?$`)
var debianVersion = regexp.MustCompile(`^[a-zA-Z0-9.+:~_-]+$`)

func parseDeferralTransaction(output string) (changes, newPackages, removals []string, err error) {
	archs := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		f := strings.Fields(line)
		if len(f) == 3 && f[0] == "SLU_INSTALLED_ARCH" {
			archs[f[1]] = f[2]
		}
	}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Inst ") {
			m := deferralInstallLine.FindStringSubmatch(line)
			if m == nil || !debianVersion.MatchString(m[3]) {
				return nil, nil, nil, fmt.Errorf("APT transaction architecture/version unavailable; cannot prove a safe deferred plan")
			}
			selector := m[1]
			if !strings.Contains(selector, ":") {
				selector += ":" + m[4]
			}
			if !deferrals.ValidSelector(selector) {
				return nil, nil, nil, fmt.Errorf("invalid APT transaction selector")
			}
			changes = append(changes, selector+"="+m[3])
			if m[2] == "" {
				newPackages = append(newPackages, selector)
			}
		}
		if strings.HasPrefix(line, "Remv ") {
			m := deferralRemoveLine.FindStringSubmatch(line)
			if m == nil {
				return nil, nil, nil, fmt.Errorf("APT removal identity unavailable")
			}
			selector := m[1]
			if !strings.Contains(selector, ":") {
				if archs[selector] == "" {
					return nil, nil, nil, fmt.Errorf("APT removal architecture unavailable")
				}
				selector += ":" + archs[selector]
			}
			if !deferrals.ValidSelector(selector) {
				return nil, nil, nil, fmt.Errorf("invalid APT removal selector")
			}
			removals = append(removals, selector)
			changes = append(changes, selector+"=-")
		}
	}
	slices.Sort(changes)
	slices.Sort(newPackages)
	slices.Sort(removals)
	return
}

func deferralInstallOptions(scope string, confirmed bool) string {
	if scope == ApprovalScopeAll || scope == ApprovalScopeSecurity {
		return "--only-upgrade --no-remove"
	}
	if !confirmed {
		return "--no-remove"
	}
	return ""
}

func BuildDeferredUpgradeCommand(scope string, plan servers.UpgradePlan, confirmRemovals bool) string {
	p, ok := plan.DeferralScopes[NormalizeApprovalScope(scope)]
	if !ok || !p.Available || len(p.SelectedPackages) == 0 {
		return ""
	}
	args := []string{NormalizeApprovalScope(scope), strconv.FormatInt(plan.PackageDeferrals.Deadline(), 10), strconv.Itoa(len(plan.PackageDeferrals.Selectors()))}
	args = append(args, plan.PackageDeferrals.Selectors()...)
	args = append(args, strconv.Itoa(len(p.Transaction)))
	args = append(args, p.Transaction...)
	remove := "no-remove"
	if confirmRemovals && len(p.RemovedPackages) > 0 {
		remove = "allow-removals"
	}
	args = append(args, remove)
	args = append(args, p.SelectedPackages...)
	command := RootHelperPath + " guarded-install"
	for _, a := range args {
		command += " " + shellQuote(a)
	}
	return `if [ "$(id -u)" -eq 0 ]; then ` + command + `; else sudo -n ` + command + `; fi`
}

func (r *withActorRunner) applyDeferrals(outcome PackageDiscoveryOutcome) (PackageDiscoveryOutcome, error) {
	snapshot, err := r.deps().LoadPackageDeferrals(r.server, r.jobID)
	if err != nil {
		return outcome, err
	}
	return PlanPackageDeferrals(r.maintenanceContext(), r.session, outcome, snapshot)
}

func (r *withActorRunner) guardPackageDeferrals() (func(), error) {
	gate := r.deps().PackageDeferralGate
	if gate != nil {
		gate.RLock()
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			if gate != nil {
				gate.RUnlock()
			}
		})
	}
	current, err := r.deps().LoadPackageDeferrals(r.server, r.jobID)
	if err != nil {
		release()
		return func() {}, err
	}
	r.deferralObservedRevision = current.Revision
	if current.Revision != r.upgradePlan.PackageDeferrals.Revision {
		release()
		return func() {}, fmt.Errorf("package deferrals changed immediately before mutation; rescan and approve the refreshed plan")
	}
	if current.Deadline() > 0 && current.Deadline() <= r.deps().Now().Unix() {
		release()
		return func() {}, fmt.Errorf("package deferral expired; rescan and approve the refreshed plan")
	}
	return release, nil
}

func (r *withActorRunner) deferralDecision() string {
	if r.deferralBlockReason != "" {
		return "blocked"
	}
	if len(r.upgradePlan.PackageDeferrals.Rules) == 0 {
		return "no_active_deferrals"
	}
	if r.upgradeCompleted {
		return "protected_transaction_applied"
	}
	return "no_package_mutation"
}

// Repair and autoremove have no approved package transaction. Until such a
// transaction can be proven, active deferrals prohibit these mutations too.
func (r *withActorRunner) guardUnplannedPackageMutation() (func(), bool) {
	gate := r.deps().PackageDeferralGate
	if gate != nil {
		gate.RLock()
	}
	release := func() {
		if gate != nil {
			gate.RUnlock()
		}
	}
	snapshot, err := r.deps().LoadPackageDeferrals(r.server, r.jobID)
	r.upgradePlan.PackageDeferrals = snapshot
	if err == nil && len(snapshot.Rules) == 0 {
		return release, true
	}
	reason := "Active package deferrals block repair/autoremove because no approved safe package transaction is available. Review deferrals before retrying."
	if err != nil {
		reason = "Package deferrals unavailable; package mutation blocked: " + err.Error()
	}
	r.deferralBlockReason = reason
	if len(r.deferralBlockReason) > 120 {
		r.deferralBlockReason = r.deferralBlockReason[:120]
	}
	r.lastErrClass = "package_deferral_conflict"
	r.setErrorLogs(r.currentLogs() + "\n" + reason)
	return release, false
}
