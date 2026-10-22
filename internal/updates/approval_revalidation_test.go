package updates

import (
	"testing"

	"debian-updater/internal/servers"
)

func TestApprovalSelectedPackagesRequireNewConsentForChangedExecutionFacts(t *testing.T) {
	base := servers.PendingUpdate{Package: "openssl", CurrentVersion: "1.0", CandidateVersion: "1.1", Source: "stable-security", Security: true}
	for _, tc := range []struct {
		name, scope string
		change      func(*servers.PendingUpdate)
		wantEqual   bool
	}{
		{name: "candidate version", scope: ApprovalScopeAll, change: func(p *servers.PendingUpdate) { p.CandidateVersion = "1.2" }},
		{name: "installed version", scope: ApprovalScopeAll, change: func(p *servers.PendingUpdate) { p.CurrentVersion = "1.0.1" }},
		{name: "package source", scope: ApprovalScopeSecurity, change: func(p *servers.PendingUpdate) { p.Source = "third-party" }},
		{name: "kept back install selector", scope: ApprovalScopeSecurityKeptBack, change: func(p *servers.PendingUpdate) { p.InstallPackage = "openssl/stable" }},
		{name: "kept back candidate", scope: ApprovalScopeSecurityKeptBack, change: func(p *servers.PendingUpdate) { p.CandidateVersion = "1.2" }},
		{name: "CVE assessment only", scope: ApprovalScopeAll, change: func(p *servers.PendingUpdate) { p.CVEState = "ready"; p.CVEs = []string{"CVE-2026-1234"} }, wantEqual: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := base
			if tc.scope == ApprovalScopeSecurityKeptBack {
				before.KeptBack = true
				before.RequiresFull = true
				before.InstallPackage = "openssl/testing"
			}
			after := before
			tc.change(&after)
			if got := approvalSelectedPackagesEqual(tc.scope, []servers.PendingUpdate{before}, []servers.PendingUpdate{after}); got != tc.wantEqual {
				t.Fatalf("approvalSelectedPackagesEqual() = %t, want %t", got, tc.wantEqual)
			}
		})
	}
}
