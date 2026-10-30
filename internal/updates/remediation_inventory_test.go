package updates

import (
	"context"
	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
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
