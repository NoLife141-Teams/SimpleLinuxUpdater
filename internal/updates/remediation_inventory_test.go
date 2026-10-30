package updates

import (
	"context"
	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
	"encoding/json"
	"fmt"
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
