package observability

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
)

func TestAvailableUpdateCVEsCountOnlyVerifiedCandidateCorrections(t *testing.T) {
	now := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339)
	fixture := func() servers.PendingUpdate {
		return servers.PendingUpdate{Package: "example", SourcePackage: "example", CurrentVersion: "1", CandidateVersion: "2", InstalledSourceVersion: "1", CandidateSourceVersion: "2", Security: true, CVEState: "ready", CVECoverage: "official", CVEScannedAt: at,
			CVEFindings: []vulnerabilities.Finding{{ID: "CVE-2026-1000", Package: "example", SourcePackage: "example", InstalledVersion: "1", CandidateVersion: "2", Disposition: "fixed_by_candidate", Assessment: vulnerabilities.Assessment{Ecosystem: "Debian:13", SourcePackage: "example", SourceVersion: "1", Source: "synthetic", ObservedAt: at, VendorStatus: "affected", Confidence: "vendor_confirmed", Applicability: "source_match"}, Availability: &vulnerabilities.FixAvailability{APT: vulnerabilities.APTContext{LastRefreshState: "succeeded", LastSuccessAt: at}, CandidateState: "addresses", CandidateBinaryVersion: "2", CandidateSourcePackage: "example", CandidateSourceVersion: "2", CandidateEcosystem: "Debian:13", QueryObservedAt: at, Scopes: []vulnerabilities.ScopeInstallability{{Scope: "all", State: "simulated"}}}}}}
	}
	for _, tc := range []struct {
		name   string
		change func(*servers.PendingUpdate)
		want   int
	}{
		{"verified", func(*servers.PendingUpdate) {}, 1},
		{"deferred", func(u *servers.PendingUpdate) { u.Deferred = true }, 0},
		{"stale", func(u *servers.PendingUpdate) { u.CVEScannedAt = now.Add(-48 * time.Hour).Format(time.RFC3339) }, 0},
		{"unknown provenance", func(u *servers.PendingUpdate) { u.CVECoverage = "official_installed_unverified" }, 0},
		{"vendor qualified", func(u *servers.PendingUpdate) {
			u.CVEFindings[0].Assessment.VendorStatus = "unimportant"
			u.CVEFindings[0].Assessment.Confidence = "vendor_qualified"
		}, 0},
		{"legacy fixed flag", func(u *servers.PendingUpdate) { u.CVEFindings[0].Availability = nil }, 0},
		{"candidate drift", func(u *servers.PendingUpdate) { u.CandidateVersion = "3" }, 0},
		{"source drift", func(u *servers.PendingUpdate) { u.CandidateSourceVersion = "3" }, 0},
		{"no simulated scope", func(u *servers.PendingUpdate) { u.CVEFindings[0].Availability.Scopes = nil }, 0},
		{"still reported", func(u *servers.PendingUpdate) { u.CVEFindings[0].Availability.CandidateState = "affected" }, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := fixture()
			tc.change(&u)
			r := projectUpdateRisk(&servers.ServerStatus{PendingUpdates: []servers.PendingUpdate{u}}, now)
			if r.UpdateCVEs.Correctable != tc.want || r.SecurityUpdates != 1 {
				t.Fatal(r)
			}
		})
	}
	u, v := fixture(), fixture()
	r := projectUpdateRisk(&servers.ServerStatus{PendingUpdates: []servers.PendingUpdate{u, v}}, now)
	if r.UpdateCVEs.Correctable != 1 || len(r.CVEs) != 1 {
		t.Fatal("duplicate source occurrences inflate counts", r)
	}
	v.CVEFindings[0].Availability.CandidateState = "affected"
	r = projectUpdateRisk(&servers.ServerStatus{PendingUpdates: []servers.PendingUpdate{u, v}}, now)
	if r.UpdateCVEs.Correctable != 0 {
		t.Fatal("partial correction promoted to complete correction", r)
	}
}

func TestAvailableUpdateCVEFailureDoesNotHideSecurityUpdates(t *testing.T) {
	r := projectUpdateRisk(&servers.ServerStatus{PendingUpdates: []servers.PendingUpdate{{Package: "example", Security: true, CVEState: "unavailable"}}}, time.Now())
	if r.SecurityUpdates != 1 || r.PendingPackages != 1 || r.UpdateCVEs.State != "incomplete" {
		t.Fatal(r)
	}
	r = projectUpdateRisk(&servers.ServerStatus{APTMetadata: vulnerabilities.APTContext{LastRefreshState: "succeeded"}}, time.Now())
	if r.Summary != "No packages to update" || len(r.CVEs) != 0 {
		t.Fatal(r)
	}
}
