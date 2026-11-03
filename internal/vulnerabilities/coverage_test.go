package vulnerabilities

import "testing"

func TestDistributionCoverageAndLegacyScans(t *testing.T) {
	for _, tc := range []struct{ distro, reason string }{
		{"debian 12", ""}, {"debian 13", ""}, {"ubuntu 24.04", ""},
		{"Ubuntu 25.04", CoverageSourceUnavailable},
		{"ubuntu 20.04", CoverageUnsupportedDistribution},
		{"fedora 42", CoverageUnsupportedDistribution}, {"", ""},
	} {
		t.Run(tc.distro, func(t *testing.T) {
			if got := ScanCoverageReason(Scan{Distro: tc.distro}); got != tc.reason {
				t.Fatalf("reason=%q want=%q", got, tc.reason)
			}
		})
	}
}
