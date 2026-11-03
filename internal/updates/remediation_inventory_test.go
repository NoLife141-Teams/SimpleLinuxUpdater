package updates

import (
	"context"
	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestUbuntu2504CoverageUnavailableNeverQueriesOrReportsClean(t *testing.T) {
	scanner := NewOSVVulnerabilityScanner(OSVVulnerabilityScannerDeps{})
	session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
		if r.Effect != HostCommandEffectReadOnly {
			t.Fatal("mutation")
		}
		switch r.Operation {
		case "vulnerability_scan.installed_inventory":
			return HostCommandResult{Stdout: "__SLU_OS__\tubuntu\t25.04\nexample-package\t1\tsource-example\t1\n__SLU_INVENTORY_END__\n"}, nil
		case "vulnerability_scan.runtime_context":
			return HostCommandResult{}, nil
		default:
			t.Fatalf("unavailable coverage attempted query preparation %s", r.Operation)
			return HostCommandResult{}, nil
		}
	}}
	scan, err := scanner.ScanInstalled(vulnerabilities.WithScanIdentity(context.Background(), "example-host", "example-scan"), session)
	if err != nil || scan.State != "unavailable" || scan.CoverageReason != vulnerabilities.CoverageSourceUnavailable || scan.Unknown != 1 || scan.Evaluated != 0 {
		t.Fatalf("%+v %v", scan, err)
	}
}

func TestPackageSimulationIdentityKeepsVersionsWithoutPublishingOriginsOrOutput(t *testing.T) {
	output := "Inst new-dependency (2 https://synthetic:private@example.invalid/repo [amd64])\nRemv old-runtime [1]\n"
	digest, packages := packageSimulationIdentity(output)
	if digest == "" || len(packages) != 1 || packages[0].Package != "new-dependency" || packages[0].CandidateVersion != "2" || packages[0].CurrentVersion != "" {
		t.Fatalf("missing simulated dependency identity: %q %+v", digest, packages)
	}
	raw, _ := json.Marshal(packages)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "https:") || strings.Contains(string(raw), "Inst ") {
		t.Fatalf("simulation published raw source output: %s", raw)
	}
	reordered, _ := packageSimulationIdentity("Remv old-runtime [1]\n  Inst  new-dependency  (2 https://synthetic:private@example.invalid/repo [amd64])\n")
	if reordered != digest {
		t.Fatal("formatting changed transaction identity")
	}
	changed, _ := packageSimulationIdentity(strings.ReplaceAll(output, "old-runtime [1]", "old-runtime [2]"))
	if changed == digest {
		t.Fatal("removal version drift was ignored")
	}
	plan := servers.UpgradePlan{FullUpgradeSimulationPackages: packages, KeptBackSecuritySimulationPackages: packages}
	clone := servers.CloneUpgradePlan(plan)
	clone.FullUpgradeSimulationPackages[0].CandidateVersion = "other"
	clone.KeptBackSecuritySimulationPackages[0].Package = "other"
	if plan.FullUpgradeSimulationPackages[0].CandidateVersion != "2" || plan.KeptBackSecuritySimulationPackages[0].Package != "new-dependency" {
		t.Fatal("simulation snapshots share mutable state")
	}
}

func TestRemediationDiscoveryRejectsNewDependencyVersionAndOriginDrift(t *testing.T) {
	for _, scope := range []string{ApprovalScopeFullUpgrade, ApprovalScopeSecurityKeptBack} {
		for _, change := range []struct{ version, origin string }{{"3", "Debian:stable"}, {"2", "Other:stable"}} {
			t.Run(scope+"/"+change.version+"/"+change.origin, func(t *testing.T) {
				makeDiscovery := func(version, origin string) PackageDiscoveryOutcome {
					output := fmt.Sprintf("The following NEW packages will be installed:\n  new-dependency\nInst addressable [1] (2 Debian:stable [amd64])\nInst new-dependency (%s %s [amd64])\n", version, origin)
					pending := []servers.PendingUpdate{{Package: "addressable", CurrentVersion: "1", CandidateVersion: "2"}}
					plan := BuildUpgradePlan(pending, output, true)
					if scope == ApprovalScopeSecurityKeptBack {
						plan = BuildUpgradePlan(pending, "", false)
						ApplyKeptBackSecuritySimulation(&plan, output)
					}
					return PackageDiscoveryOutcome{PendingUpdates: pending, UpgradePlan: plan}
				}
				if remediationDiscoveryEqual(makeDiscovery("2", "Debian:stable"), makeDiscovery(change.version, change.origin)) {
					t.Fatal("new dependency changed without renewed approval")
				}
			})
		}
	}
}
func TestRemediationWholeDiscoveryDetectsSourceAndUnselectedCandidateDrift(t *testing.T) {
	base := PackageDiscoveryOutcome{PendingUpdates: []servers.PendingUpdate{{Package: "example-package", CurrentVersion: "1", CandidateVersion: "2", SourcePackage: "source-example", InstalledSourceVersion: "1", CandidateSourceVersion: "2"}}}
	for _, kind := range []string{"source_package", "installed_source", "candidate_source", "candidate"} {
		t.Run(kind, func(t *testing.T) {
			fresh := base
			fresh.PendingUpdates = append([]servers.PendingUpdate{}, base.PendingUpdates...)
			switch kind {
			case "source_package":
				fresh.PendingUpdates[0].SourcePackage = "other-source"
			case "installed_source":
				fresh.PendingUpdates[0].InstalledSourceVersion = "1.1"
			case "candidate_source":
				fresh.PendingUpdates[0].CandidateSourceVersion = "2.1"
			case "candidate":
				fresh.PendingUpdates[0].CandidateVersion = "3"
			}
			if remediationDiscoveryEqual(base, fresh) {
				t.Fatal("drift accepted")
			}
		})
	}
}

func TestRemediationScopesCaptureActualCommandsAndSanitizeDependencyEvidence(t *testing.T) {
	discovery := PackageDiscoveryOutcome{PendingUpdates: []servers.PendingUpdate{{Package: "security-package", CurrentVersion: "1", CandidateVersion: "2", Security: true}, {Package: "kept-package", CurrentVersion: "1", CandidateVersion: "2", Security: true, KeptBack: true, RequiresFull: true}}}
	var drift bool
	r := &withActorRunner{session: &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, request HostCommandRequest) (HostCommandResult, error) {
		if request.Effect != HostCommandEffectReadOnly || !strings.Contains(request.Command, "apt-get -s") || strings.Contains(request.Command, "--download-only") {
			t.Fatal("not a read-only simulation of the canonical scope", request)
		}
		v, origin := "2", "https://synthetic:private@repo.invalid/old"
		if drift {
			v, origin = "3", "https://synthetic:private@repo.invalid/new"
		}
		return HostCommandResult{Stdout: fmt.Sprintf("Inst security-package [1] (2 Debian:stable [amd64])\nInst ordinary-dependency [1] (%s %s [amd64])\nInst new-dependency (1 Debian:stable [amd64])\n", v, origin)}, nil
	}}}
	before, err := r.readRemediationScopes(discovery)
	if err != nil {
		t.Fatal(err)
	}
	drift = true
	after, err := r.readRemediationScopes(discovery)
	if err != nil {
		t.Fatal(err)
	}
	for scope, p := range before {
		if !p.Available || p.Digest == "" || p.Digest == after[scope].Digest || len(p.Packages) != 3 || len(p.Extra) < 1 || len(p.Added) != 1 {
			t.Fatalf("scope %s omitted actual dependency changes: %+v", scope, p)
		}
	}
	raw, _ := json.Marshal(before)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "https:") || strings.Contains(string(raw), "Inst ") {
		t.Fatalf("raw output leaked: %s", raw)
	}
}

func TestRemediationSourceIdentityUsesGeneratedMetadataCommandForNewDependency(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("shell fixture requires sh")
	}
	dir := t.TempDir()
	for name, body := range map[string]string{
		"dpkg":       "printf 'amd64\\n'",
		"dpkg-query": "printf 'existing:amd64\\tsource-existing\\t1\\t1\\tinstalled\\n'",
		"apt-get":    "exit 0",
		"apt-mark":   "exit 0",
		"find":       "exit 0",
		"stat":       "exit 0",
		"apt-cache": `case "$1" in
policy) [ "$#" -gt 1 ] || exit 0; printf 'new-dependency:amd64:\n  Installed: (none)\n  Candidate: 2\n  Version table:\n     2 500\n';;
show) printf 'Package: new-dependency\nVersion: 2\nSource: source-dependency (%s)\n\n' "$SLU_SYNTH_SOURCE_VERSION";;
*) exit 3;;
esac`,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	osRelease := filepath.Join(dir, "os-release")
	if err := os.WriteFile(osRelease, []byte("ID=debian\nVERSION_ID=12\nVERSION_CODENAME=bookworm\n"), 0600); err != nil {
		t.Fatal(err)
	}
	sourceVersion := "2"
	r := &withActorRunner{session: &HostMaintenanceSessionFuncs{RunCommandFunc: func(ctx context.Context, request HostCommandRequest) (HostCommandResult, error) {
		if request.Effect != HostCommandEffectReadOnly || request.Operation != "cve_enrichment.package_inventory" {
			t.Fatal(request)
		}
		command := strings.ReplaceAll(request.Command, "/etc/os-release", osRelease)
		cmd := exec.CommandContext(ctx, "sh", "-c", command)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "SLU_SYNTH_SOURCE_VERSION="+sourceVersion)
		output, err := cmd.CombinedOutput()
		return HostCommandResult{Stdout: string(output)}, err
	}}}
	installed := []servers.PendingUpdate{{Package: "existing:amd64", CurrentVersion: "1", SourcePackage: "source-existing", InstalledSourceVersion: "1"}}
	scopes := map[string]RemediationScopePreparation{"full_upgrade": {Available: true, Packages: []servers.SimulatedPackage{{Package: "new-dependency", Architecture: "amd64", CandidateVersion: "2"}}}}
	before, err := r.readRemediationSources(PackageDiscoveryOutcome{}, scopes, installed, "debian 12")
	if err != nil {
		t.Fatal(err)
	}
	sourceVersion = "3"
	after, err := r.readRemediationSources(PackageDiscoveryOutcome{}, scopes, installed, "debian 12")
	if err != nil {
		t.Fatal(err)
	}
	p := after.Packages["new-dependency:amd64"]
	if !scopes["full_upgrade"].Available || p.InstalledVersion != "" || p.CandidateVersion != "2" || p.CandidateSourcePackage != "source-dependency" || p.CandidateSourceVersion != "3" || before.Digest == after.Digest {
		t.Fatalf("same-binary-version source drift escaped generated collection: %+v", after)
	}
}

func TestRemediationSourceCollectionUsesNativeForeignAndAllBinaryIdentities(t *testing.T) {
	for _, tc := range []struct{ name, pending, install, raw, simulated, arch, installed, selector string }{
		{"native", "binary", "", "binary/stable 2 amd64 [upgradable from: 1]", "binary", "amd64", "binary:amd64", "binary:amd64"},
		{"foreign", "binary", "binary:i386", "binary/stable 2 i386 [upgradable from: 1]", "binary:i386", "i386", "binary:i386", "binary:i386"},
		{"all", "all-package", "", "all-package/stable 2 all [upgradable from: 1]", "all-package", "all", "all-package", "all-package"},
		{"explicit all", "all-package:all", "", "", "all-package", "all", "all-package", "all-package"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installed := []servers.PendingUpdate{{Package: tc.installed, CurrentVersion: "1", SourcePackage: "source-example", InstalledSourceVersion: "1"}}
			digest := installedInventoryDigest(installed)
			r := &withActorRunner{session: &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, request HostCommandRequest) (HostCommandResult, error) {
				if request.Effect != HostCommandEffectReadOnly || !strings.Contains(request.Command, tc.selector) || strings.Contains(request.Command, ":all") || tc.arch == "i386" && strings.Contains(request.Command, "binary:amd64") {
					t.Fatal("wrong binary identity requested", request.Command)
				}
				return HostCommandResult{Stdout: fmt.Sprintf("__SLU_OS__\tdebian\t12\n__SLU_INVENTORY_DIGEST__\t%s\n__SLU_PACKAGE__\t%s\t%s\tsource-example\t1\t1\tDebian\tverified\tbinary\t2\tsource-example (2)\tDebian\tverified\n", digest, tc.selector, tc.installed)}, nil
			}}}
			discovery := PackageDiscoveryOutcome{PendingUpdates: []servers.PendingUpdate{{Package: tc.pending, InstallPackage: tc.install, Raw: tc.raw, CurrentVersion: "1", CandidateVersion: "2"}}}
			scopes := map[string]RemediationScopePreparation{"all": {Available: true, Packages: []servers.SimulatedPackage{{Package: tc.simulated, Architecture: tc.arch, CurrentVersion: "1", CandidateVersion: "2"}}}}
			result, err := r.readRemediationSources(discovery, scopes, installed, "debian 12")
			if err != nil || !scopes["all"].Available || result.Packages[tc.selector].InstalledVersion != "1" {
				t.Fatalf("valid binary transaction unavailable: %+v %v", result, err)
			}
		})
	}
}

func TestRemediationSourceIdentityBindsVerifiedReleaseProofs(t *testing.T) {
	installed := []servers.PendingUpdate{{Package: "binary:amd64", CurrentVersion: "1", SourcePackage: "source-example", InstalledSourceVersion: "1"}}
	proof := "/var/lib/apt/lists/synthetic_InRelease|/usr/share/keyrings/debian-archive-keyring.gpg|" + strings.Repeat("a", 64)
	r := &withActorRunner{session: &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, request HostCommandRequest) (HostCommandResult, error) {
		if request.Effect != HostCommandEffectReadOnly {
			t.Fatal(request)
		}
		return HostCommandResult{Stdout: fmt.Sprintf("__SLU_OS__\tdebian\t12\n__SLU_INVENTORY_DIGEST__\t%s\n__SLU_PACKAGE__\tbinary:amd64\tbinary:amd64\tsource-example\t1\t1\tDebian\tverified\tbinary\t2\tsource-example (2)\tDebian\tverified\n__SLU_RELEASE__\t%s\n", installedInventoryDigest(installed), proof)}, nil
	}}}
	discovery := PackageDiscoveryOutcome{PendingUpdates: []servers.PendingUpdate{{Package: "binary:amd64"}}}
	read := func() RemediationSourcePreparation {
		t.Helper()
		result, err := r.readRemediationSources(discovery, nil, installed, "debian 12")
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	before := read()
	if read().Digest != before.Digest {
		t.Fatal("unchanged release proof did not preserve plan identity")
	}
	for _, changed := range []string{
		strings.Replace(proof, strings.Repeat("a", 64), strings.Repeat("b", 64), 1),
		strings.Replace(proof, "synthetic_InRelease", "other_InRelease", 1),
		strings.Replace(proof, "debian-archive-keyring.gpg", "other-keyring.gpg", 1),
	} {
		proof = changed
		after := read()
		if after.Digest == before.Digest {
			t.Fatal("release signature proof changed without invalidating plan identity")
		}
		public, err := json.Marshal(after)
		if err != nil || strings.Contains(string(public), "/var/lib/apt/") || strings.Contains(string(public), "/usr/share/keyrings/") {
			t.Fatalf("release paths exposed in public proof DTO: %s %v", public, err)
		}
	}
}
