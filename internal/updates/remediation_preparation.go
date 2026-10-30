package updates

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"debian-updater/internal/servers"
)

func (r *withActorRunner) readRemediationSources(discovery PackageDiscoveryOutcome, scopes map[string]RemediationScopePreparation, installed []servers.PendingUpdate, distro string) (RemediationSourcePreparation, error) {
	requests := map[string]servers.PendingUpdate{}
	for _, p := range discovery.PendingUpdates {
		selectors := PackageDiscoverySelectors(p)
		selector := selectors[len(selectors)-1]
		selector = strings.TrimSuffix(selector, ":all")
		requests[selector] = servers.PendingUpdate{Package: selector}
	}
	for _, p := range scopes {
		for _, change := range p.Packages {
			selector := RemediationSimulationSelector(change)
			requests[selector] = servers.PendingUpdate{Package: selector}
		}
	}
	packages := make([]servers.PendingUpdate, 0, len(requests))
	for _, p := range requests {
		packages = append(packages, p)
	}
	slices.SortFunc(packages, func(a, b servers.PendingUpdate) int { return strings.Compare(a.Package, b.Package) })
	metadata, err := collectHostVulnerabilityInventory(r.maintenanceContext(), r.session, packages)
	if err != nil {
		return RemediationSourcePreparation{}, err
	}
	if metadata.InstalledDigest != installedInventoryDigest(installed) || metadata.DistroID+" "+metadata.DistroVersion != distro {
		return RemediationSourcePreparation{}, fmt.Errorf("installed inventory changed during source/candidate collection")
	}
	// Observation clocks and index ages are presentation facts. The identity binds
	// current source/candidate mappings, provenance, holds/pins and release files.
	slices.Sort(metadata.ReleaseProofs)
	identity := struct {
		Distro, Inventory string
		Packages          map[string]packageVulnerabilityInventory
		Holds             map[string]bool
		Releases          []string
	}{distro, metadata.InstalledDigest, metadata.Packages, metadata.Holds, metadata.ReleaseProofs}
	body, err := json.Marshal(identity)
	if err != nil {
		return RemediationSourcePreparation{}, err
	}
	hash := sha256.Sum256(body)
	out := RemediationSourcePreparation{Digest: hex.EncodeToString(hash[:]), Packages: map[string]RemediationPackageSource{}}
	for selector, p := range metadata.Packages {
		installedEcosystem, _ := vulnerabilityEcosystemForReleaseOrigin(metadata.DistroID, metadata.DistroVersion, p.InstalledReleaseOrigin, p.InstalledReleaseSigned)
		candidateEcosystem, _ := vulnerabilityEcosystemForReleaseOrigin(metadata.DistroID, metadata.DistroVersion, p.CandidateReleaseOrigin, p.CandidateReleaseSigned)
		out.Packages[selector] = RemediationPackageSource{InstalledEcosystem: installedEcosystem, CandidateEcosystem: candidateEcosystem, InstalledVersion: p.InstalledBinaryVersion, InstalledSourcePackage: p.InstalledSourcePackage, InstalledSourceVersion: p.InstalledSourceVersion, InstalledProvenanceVerified: p.InstalledReleaseSigned, CandidateVersion: p.CandidateBinaryVersion, CandidateSourcePackage: p.CandidateSourcePackage, CandidateSourceVersion: p.CandidateSourceVersion, CandidateProvenanceVerified: p.CandidateReleaseSigned}
	}
	for scope, p := range scopes {
		for _, change := range p.Packages {
			selector := RemediationSimulationSelector(change)
			source, ok := out.Packages[selector]
			if !ok || source.InstalledVersion != change.CurrentVersion || source.CandidateVersion != change.CandidateVersion || source.CandidateSourcePackage == "" || source.CandidateSourceVersion == "" {
				p.Available = false
			}
		}
		scopes[scope] = p
	}
	return out, nil
}

// Architecture-independent binaries have one bare dpkg identity. Native and
// foreign binaries retain the architecture evidenced by their APT simulation.
func RemediationSimulationSelector(p servers.SimulatedPackage) string {
	selector := p.Package
	if p.Architecture == "all" {
		base, _, _ := strings.Cut(selector, ":")
		return base
	}
	if !strings.Contains(selector, ":") && p.Architecture != "" {
		selector += ":" + p.Architecture
	}
	return selector
}

// Simulate the same four existing maintenance operations. Download-only APT
// output supplies disk/URI facts but does not contain the Inst/Remv records
// needed to bind dependency versions and origins or explain the real scope.
func (r *withActorRunner) readRemediationScopes(discovery PackageDiscoveryOutcome) (map[string]RemediationScopePreparation, error) {
	out := map[string]RemediationScopePreparation{}
	for _, scope := range []string{ApprovalScopeAll, ApprovalScopeSecurity, ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade} {
		selected := PackagesForApprovalScope(scope, discovery.PendingUpdates)
		if len(discovery.UpgradePlan.PackageDeferrals.Rules) > 0 {
			protected := discovery.UpgradePlan.DeferralScopes[scope]
			if !protected.Available {
				out[scope] = RemediationScopePreparation{}
				continue
			}
			selected = protected.SelectedPackages
		}
		if len(selected) == 0 {
			out[scope] = RemediationScopePreparation{}
			continue
		}
		command := ""
		switch scope {
		case ApprovalScopeAll:
			command = AptListUpgradableCmd
		case ApprovalScopeFullUpgrade:
			command = ReadOnlyAptCommand("apt-get -s full-upgrade")
		default:
			command = ReadOnlyAptCommand("apt-get -s " + deferralInstallOptions(scope, true) + " install -- " + strings.Join(validatedShellEscapedPackageArgs(selected), " "))
		}
		if len(discovery.UpgradePlan.PackageDeferrals.Rules) > 0 {
			command = ReadOnlyAptCommand("apt-get -s -o APT::Ignore-Hold=false -o APT::Get::allow-change-held-packages=false -o APT::Get::allow-downgrades=false " + deferralInstallOptions(scope, true) + " install -- " + strings.Join(validatedShellEscapedPackageArgs(selected), " "))
		}
		result, err := r.session.RunCommand(r.maintenanceContext(), HostCommandRequest{Operation: "remediation.simulate." + scope, Command: command, Effect: HostCommandEffectReadOnly, ReplayPolicy: ReplayNever})
		if err != nil {
			if r.maintenanceContext().Err() != nil {
				return nil, r.maintenanceContext().Err()
			}
			out[scope] = RemediationScopePreparation{}
			continue
		}
		output := result.Stdout + "\n" + result.Stderr
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Inst ") {
				m := deferralInstallLine.FindStringSubmatch(line)
				if m == nil || !debianVersion.MatchString(m[3]) {
					return nil, fmt.Errorf("%s simulation package identity unavailable", scope)
				}
			}
		}
		digest, packages := packageSimulationIdentity(output)
		p := RemediationScopePreparation{Available: digest != "", Digest: digest, Packages: packages, Added: []string{}, Removed: []string{}, Extra: []string{}}
		wanted := map[string]bool{}
		selectedNames := map[string]bool{}
		for _, name := range selected {
			selectedNames[name] = true
		}
		for _, update := range discovery.PendingUpdates {
			if selectedNames[update.Package] || selectedNames[update.InstallPackage] {
				for _, selector := range PackageDiscoverySelectors(update) {
					wanted[selector] = true
				}
			}
		}
		for _, pkg := range packages {
			if pkg.CurrentVersion == "" {
				p.Added = append(p.Added, pkg.Package)
			}
			if !wanted[pkg.Package] && !wanted[RemediationSimulationSelector(pkg)] {
				p.Extra = append(p.Extra, pkg.Package)
			}
		}
		for _, line := range strings.Split(output, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "Remv ") {
				m := deferralRemoveLine.FindStringSubmatch(line)
				if m == nil {
					return nil, fmt.Errorf("%s simulation removal identity unavailable", scope)
				}
				p.Removed = append(p.Removed, m[1])
			}
		}
		out[scope] = p
	}
	return out, nil
}
