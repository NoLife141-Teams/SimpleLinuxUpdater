package observability

import (
	"debian-updater/internal/vulnerabilities"
	"fmt"
	"testing"
)

func remediationFinding(pkg, id string, fix bool) DashboardVulnerabilityFinding {
	a := &vulnerabilities.FixAvailability{CandidateState: "affected", Scopes: []vulnerabilities.ScopeInstallability{{Scope: "all", State: "simulated"}}}
	d := "still_affected"
	if fix {
		a.CandidateState = "addresses"
		d = "fixed_by_candidate"
	}
	return DashboardVulnerabilityFinding{Finding: vulnerabilities.Finding{Package: pkg, ID: id, InstalledVersion: "1", CandidateVersion: "2", Source: "synthetic", Disposition: d, Availability: a}, EvidenceState: "current", Freshness: "fresh", Qualification: vulnerabilities.QualificationConfirmed, AvailabilityFreshness: "fresh"}
}
func TestRemediationHundredCVEsFiveCandidatesWholeServerFilters(t *testing.T) {
	info := DashboardVulnerabilityInfo{State: "complete", Coverage: "complete", Freshness: "fresh"}
	for i := 0; i < 100; i++ {
		pkg := fmt.Sprintf("package-%03d", i)
		if i < 20 {
			pkg = fmt.Sprintf("candidate-%d", i/4)
		}
		info.Findings = append(info.Findings, remediationFinding(pkg, fmt.Sprintf("CVE-2026-%04d", i), i < 20))
	}
	snapshot := DashboardSummaryResponse{Servers: []DashboardServerSummary{{Name: "example-host", Risk: DashboardRiskInfo{Vulnerability: info}}}}
	for _, q := range []RemediationQuery{{Server: "example-host"}, {Server: "example-host", Search: "candidate-1", PageSize: 1}, {Server: "example-host", State: "update_candidate", Page: 2, PageSize: 1}, {Server: "example-host", Package: "candidate-0"}} {
		p := BuildRemediationPlan(snapshot, q)
		h := p.Selected
		if h.CVEs != 100 || h.PotentialCVEs != 20 || h.RemainingCVEs != 80 || h.CandidatePackages != 5 {
			t.Fatalf("%+v: %+v", q, h)
		}
	}
	// A shared CVE on an untreated package cannot be counted fully addressed.
	info.Findings = append(info.Findings, remediationFinding("shared-residual", "CVE-2026-0000", false))
	snapshot.Servers[0].Risk.Vulnerability = info
	p := BuildRemediationPlan(snapshot, RemediationQuery{Server: "example-host"})
	if p.Selected.PotentialCVEs != 19 || p.Selected.RemainingCVEs != 81 {
		t.Fatal(p.Selected)
	}
}
func TestRemediationCanonicalQualificationRuntimeAndAvailability(t *testing.T) {
	for _, kind := range []string{"legacy", "stale", "qualified", "header", "external", "unknown_kernel", "blocked_scope", "unverified_package"} {
		t.Run(kind, func(t *testing.T) {
			f := remediationFinding("example-package", "CVE-1", true)
			switch kind {
			case "legacy":
				f.Availability = nil
			case "stale":
				f.AvailabilityFreshness = "stale"
			case "qualified":
				f.Qualification = vulnerabilities.QualificationQualified
			case "header":
				f.RuntimeView.Component = "development_headers"
			case "external":
				f.RuntimeView.Execution = "external"
			case "unknown_kernel":
				f.RuntimeView.Component = "kernel_image"
			case "blocked_scope":
				f.Availability.Scopes[0].State = "blocked"
			case "unverified_package":
				f.EvidenceState = "not_reverified"
			}
			if candidateRemediationFinding(f) {
				t.Fatal("unqualified candidate")
			}
		})
	}
}
