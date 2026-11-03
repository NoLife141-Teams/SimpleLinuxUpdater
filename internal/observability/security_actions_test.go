package observability

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/securityintel"
	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
)

func securityActionFixture() (DashboardSummaryResponse, time.Time) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	at := now.Add(-time.Hour).Format(time.RFC3339)
	f := DashboardVulnerabilityFinding{Finding: vulnerabilities.Finding{ID: "CVE-2026-1000", Package: "example", InstalledVersion: "1.0-1", CandidateVersion: "1.0-2", Severity: "critical", SeveritySource: "synthetic", PublishedAt: at, LastSeen: at, Availability: &vulnerabilities.FixAvailability{Published: vulnerabilities.PublishedFix{State: "published", Source: "synthetic", ObservedAt: at}, CandidateState: "addresses", RepositoryState: "observed", APT: vulnerabilities.APTContext{LastRefreshState: "succeeded", LastRefreshAt: at}, Scopes: []vulnerabilities.ScopeInstallability{{Scope: "all", State: "simulated"}}}}, Qualification: vulnerabilities.QualificationConfirmed, AssessmentFreshness: "fresh", Freshness: "fresh", AvailabilityFreshness: "fresh", PublishedFixFreshness: "fresh", SeverityFreshness: "fresh"}
	return DashboardSummaryResponse{Servers: []DashboardServerSummary{{Name: "synthetic-host", Risk: DashboardRiskInfo{Vulnerability: DashboardVulnerabilityInfo{Distro: "debian 12", Freshness: "fresh", Findings: []DashboardVulnerabilityFinding{f}}}}}}, now
}
func TestSecurityActionsRequiresInstallabilityAndPreservesUnknown(t *testing.T) {
	tests := []struct {
		name   string
		change func(*DashboardVulnerabilityFinding)
		fixes  int
	}{
		{"verified", func(*DashboardVulnerabilityFinding) {}, 1},
		{"legacy_boolean_is_not_proof", func(f *DashboardVulnerabilityFinding) { f.FixAvailable = true; f.Availability = nil }, 0},
		{"stale_inventory", func(f *DashboardVulnerabilityFinding) { f.Freshness = "stale" }, 0},
		{"stale_assessment", func(f *DashboardVulnerabilityFinding) { f.AssessmentFreshness = "stale" }, 0},
		{"unqualified_source", func(f *DashboardVulnerabilityFinding) { f.Qualification = "needs_review" }, 0},
		{"failed_apt_refresh", func(f *DashboardVulnerabilityFinding) { f.Availability.APT.LastRefreshState = "failed" }, 0},
		{"old_apt_refresh", func(f *DashboardVulnerabilityFinding) { f.Availability.APT.LastRefreshAt = "2020-01-01T00:00:00Z" }, 0},
		{"held_package", func(f *DashboardVulnerabilityFinding) {
			f.Availability.Restrictions = []string{"administrator_hold_observed"}
		}, 0},
		{"missing_repository", func(f *DashboardVulnerabilityFinding) { f.Availability.RepositoryState = "published_not_observed" }, 0},
		{"failed_simulation", func(f *DashboardVulnerabilityFinding) { f.Availability.Scopes[0].State = "blocked" }, 0},
		{"candidate_still_affected", func(f *DashboardVulnerabilityFinding) { f.Availability.CandidateState = "affected" }, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			tt.change(&summary.Servers[0].Risk.Vulnerability.Findings[0])
			out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now)
			if out.FixTotal != tt.fixes || out.AlertTotal != 0 {
				t.Fatalf("incorrect classification: %+v", out)
			}
			if tt.fixes == 0 && out.ReviewFindings != 1 {
				t.Fatal("missing proof silently dropped")
			}
		})
	}
}
func TestSecurityActionsPriorityUsesPublicationAndRetainsWatchedAlerts(t *testing.T) {
	tests := []struct {
		name             string
		published, watch string
		severity         string
		exploited        bool
		want             int
	}{
		{"recent_critical", "2026-11-01T12:00:00Z", "", "critical", false, 1},
		{"scan_date_is_not_publication", "", "", "critical", false, 0},
		{"old_unwatched", "2025-01-01T00:00:00Z", "", "critical", false, 0},
		{"old_watched_remains", "2025-01-01T00:00:00Z", "2025-01-02T00:00:00Z", "critical", false, 1},
		{"exploited_high_without_publication", "", "", "high", true, 1},
		{"future_publication", "2099-01-01T00:00:00Z", "", "critical", false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			f := &summary.Servers[0].Risk.Vulnerability.Findings[0]
			f.PublishedAt, f.PrioritySince, f.Severity = tt.published, tt.watch, tt.severity
			f.Availability.Published.State = "not_established"
			f.Availability.Published.Ranges = []vulnerabilities.FixRange{{Type: "ECOSYSTEM", Events: []vulnerabilities.VersionEvent{{Introduced: "0"}}}}
			f.Availability.CandidateState = "same"
			intel := securityintel.Snapshot{Exploited: map[string]securityintel.Exploitation{}, Feeds: map[string]securityintel.Feed{"kev": {State: "fresh"}}}
			if tt.exploited {
				intel.Exploited[f.ID] = securityintel.Exploitation{CVE: f.ID, Action: "Consult vendor guidance"}
			}
			out := BuildSecurityActions(summary, intel, SecurityActionQuery{}, now)
			if out.AlertTotal != tt.want || out.FixTotal != 0 {
				t.Fatalf("incorrect priority: %+v", out)
			}
			for _, alert := range out.Alerts {
				if alert.CVECount != len(alert.CVEs) || alert.CVECount != 1 {
					t.Fatalf("single-finding alert reported an incorrect CVE count: %+v", alert)
				}
			}
		})
	}
}

func TestSecurityActionsRetainsStaleWatchedCriticalAsPriorityToReview(t *testing.T) {
	for _, tt := range []struct {
		name, severityFreshness, findingFreshness, qualification, severity, resolved string
		wantPriority                                                                 int
	}{
		{"severity_expired", "stale", "fresh", vulnerabilities.QualificationConfirmed, "critical", "", 1},
		{"scan_expired", "stale", "stale", vulnerabilities.QualificationReview, "critical", "", 1},
		{"severity_unknown", "unknown", "fresh", vulnerabilities.QualificationConfirmed, "critical", "", 1},
		{"current_downgrade", "fresh", "fresh", vulnerabilities.QualificationConfirmed, "medium", "", 0},
		{"expired_downgrade", "stale", "stale", vulnerabilities.QualificationReview, "medium", "", 0},
		{"vendor_exemption", "stale", "fresh", vulnerabilities.QualificationQualified, "critical", "", 0},
		{"resolved", "stale", "stale", vulnerabilities.QualificationConfirmed, "critical", "2026-11-01T00:00:00Z", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			f := &summary.Servers[0].Risk.Vulnerability.Findings[0]
			f.PublishedAt = "2026-01-01T00:00:00Z"
			f.PrioritySince = "2026-01-02T00:00:00Z"
			f.SeverityFreshness, f.Freshness, f.Qualification, f.Severity, f.ResolvedAt = tt.severityFreshness, tt.findingFreshness, tt.qualification, tt.severity, tt.resolved
			f.Availability.Published.State = "not_established"
			f.Availability.Published.Ranges = []vulnerabilities.FixRange{{Type: "ECOSYSTEM", Events: []vulnerabilities.VersionEvent{{Introduced: "0"}}}}
			f.Availability.CandidateState = "same"
			out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now)
			if out.AlertTotal != 0 || out.FixTotal != 0 || out.ReviewPriority != tt.wantPriority {
				t.Fatalf("stale or superseded priority incorrectly classified: %+v", out)
			}
		})
	}
}

func TestSecurityActionsRetainsWatchWhenNewerApprovalEvidenceReplacesInstalledFinding(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		missing, downgrade, qualified bool
		alerts, reviewPriority        int
	}{
		{name: "refreshed_advisory", alerts: 1},
		{name: "missing_optional_advisory", missing: true, reviewPriority: 1},
		{name: "current_downgrade", downgrade: true},
		{name: "current_vendor_exemption", qualified: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			before, current := now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)
			old := summary.Servers[0].Risk.Vulnerability.Findings[0].Finding
			old.PublishedAt = now.Add(-40 * 24 * time.Hour).Format(time.RFC3339)
			old.PrioritySince = now.Add(-39 * 24 * time.Hour).Format(time.RFC3339)
			old.LastSeen, old.SeverityObservedAt = before, before
			old.Assessment = vulnerabilities.Assessment{Ecosystem: "Debian:12", SourcePackage: "example", SourceVersion: "1.0-1", VendorStatus: "affected", Confidence: "vendor_confirmed", Applicability: "source_match", Source: "synthetic", ObservedAt: before}
			old.Availability.Published.State = "not_established"
			old.Availability.Published.Ranges = []vulnerabilities.FixRange{{Type: "ECOSYSTEM", Events: []vulnerabilities.VersionEvent{{Introduced: "0"}}}}
			old.Availability.CandidateState = "same"
			observation := vulnerabilities.Observation{Accepted: &vulnerabilities.Scan{State: "partial", FinishedAt: before}, Findings: []vulnerabilities.Finding{old}}
			approval := old
			approval.PrioritySince, approval.PublishedAt = "", ""
			approval.Assessment.ObservedAt, approval.SeverityObservedAt = current, current
			if tc.missing {
				approval.Severity, approval.SeveritySource, approval.SeverityObservedAt = "unknown", "", ""
				approval.Assessment = vulnerabilities.Assessment{}
				approval.Availability = nil
			}
			if tc.downgrade {
				approval.Severity = "medium"
			}
			if tc.qualified {
				approval.Assessment.VendorStatus, approval.Assessment.Confidence = "unimportant", "vendor_qualified"
			}
			status := &servers.ServerStatus{PendingUpdates: []servers.PendingUpdate{{Package: old.Package, CurrentVersion: old.InstalledVersion, CandidateVersion: old.CandidateVersion, CVEScannedAt: current, CVEFindings: []servers.VulnerabilityFinding{approval}}}}
			summary.Servers[0].Risk = projectVulnerabilityRisk(status, observation, now, nil, 24*time.Hour, true)
			out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now)
			if out.AlertTotal != tc.alerts || out.ReviewPriority != tc.reviewPriority {
				t.Fatalf("newer approval evidence erased or incorrectly promoted a historical watch: %+v", out)
			}
		})
	}
}
func TestSecurityActionsBoundedQueuesAndSeparateTotals(t *testing.T) {
	summary, now := securityActionFixture()
	base := summary.Servers[0].Risk.Vulnerability.Findings[0]
	for i := 0; i < 60; i++ {
		f := base
		f.ID = "CVE-2026-" + strings.Repeat("1", i+1)
		summary.Servers[0].Risk.Vulnerability.Findings = append(summary.Servers[0].Risk.Vulnerability.Findings, f)
	}
	out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{PageSize: 1}, now)
	if out.FixTotal != 1 || len(out.Fixes) != 1 || out.Fixes[0].CVECount != 61 || len(out.Fixes[0].CVEs) != 10 {
		t.Fatalf("unbounded or missing grouped evidence: %+v", out.Fixes)
	}
	if out.LifecycleTotal != 1 || out.Fixes[0].CVEs == nil {
		t.Fatal("unexpected totals")
	}
}
func TestLifecycleDistinguishesUpstreamAndExtendedSupport(t *testing.T) {
	now := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
	intel := securityintel.Snapshot{Products: map[string]securityintel.Product{}, Feeds: map[string]securityintel.Feed{}}
	for _, product := range []string{"ubuntu", "debian", "python"} {
		intel.Feeds[product] = securityintel.Feed{State: "fresh"}
	}
	intel.Products["ubuntu"] = securityintel.Product{Releases: []securityintel.Release{{Name: "25.04", ReleaseDate: "2025-04-17", EOLFrom: "2026-01-17"}, {Name: "20.04", ReleaseDate: "2020-04-23", EOLFrom: "2025-05-31", EOESFrom: "2030-04-30"}}}
	intel.Products["python"] = securityintel.Product{Releases: []securityintel.Release{{Name: "3.9", ReleaseDate: "2020-10-05", EOLFrom: "2025-10-31"}, {Name: "3", ReleaseDate: "2008-12-03", EOLFrom: "2099-01-01"}}}
	for _, tt := range []struct{ product, version, kind, state, cycle string }{{"ubuntu", "25.04", "os", "end_of_life", "25.04"}, {"ubuntu", "20.04", "os", "extended_support_unverified", "20.04"}, {"python", "1:3.9.2-1+deb11u2", "application", "upstream_end_of_life", "3.9"}, {"python", "unknown", "application", "unknown", ""}, {"debian", "12", "os", "unknown", ""}} {
		t.Run(tt.product+tt.version, func(t *testing.T) {
			out := lifecycleFor("synthetic", tt.product, tt.product, tt.version, tt.kind, "fresh", intel, now)
			if out.State != tt.state || out.Cycle != tt.cycle {
				t.Fatalf("wrong lifecycle: %+v", out)
			}
			if tt.kind == "application" && out.SupportContext != "upstream_only_distribution_support_must_be_checked" {
				t.Fatal("upstream EOL misrepresented as distro support")
			}
		})
	}
	if appLifecycleProduct("php8.2-fpm:amd64") != "php" || appLifecycleProduct("postgresql-client-15") != "" || appLifecycleProduct("host-named-python") != "" {
		t.Fatal("unproven application identification")
	}
}
func TestSecurityActionsRejectsInvalidFilters(t *testing.T) {
	for _, raw := range []string{"view=invalid", "page=0", "page_size=101", "recent_days=0", "recent_days=999", "q=" + strings.Repeat("x", 201)} {
		values, _ := url.ParseQuery(raw)
		if _, err := ParseSecurityActionQuery(values); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}

func TestLifecyclePrefersNewerOSFactsOverOldScanIdentity(t *testing.T) {
	summary, now := securityActionFixture()
	host := &summary.Servers[0]
	host.Risk.Vulnerability.Distro = "ubuntu 25.04"
	host.Risk.Vulnerability.AcceptedAt = "2026-10-01T00:00:00Z"
	host.Health.OSPrettyName = "Ubuntu 24.04.3 LTS"
	host.Health.CollectedAt = "2026-11-02T11:00:00Z"
	intel := securityintel.Snapshot{Products: map[string]securityintel.Product{"ubuntu": {Releases: []securityintel.Release{{Name: "24.04", ReleaseDate: "2024-04-25", EOLFrom: "2029-05-31"}}}}, Feeds: map[string]securityintel.Feed{"ubuntu": {State: "fresh"}}}
	out := BuildSecurityActions(summary, intel, SecurityActionQuery{}, now)
	if out.Lifecycle[0].Version != "24.04" || out.Lifecycle[0].State != "supported" || out.Lifecycle[0].InventoryFreshness != "fresh" {
		t.Fatalf("obsolete scan overrides newer OS facts: %+v", out.Lifecycle)
	}
}

func TestLifecycleIncludesInstalledHistoricalPostgreSQLServerCycles(t *testing.T) {
	summary, now := securityActionFixture()
	summary.Servers[0].Risk.Vulnerability.Inventory = []vulnerabilities.Package{
		{Name: "postgresql-9.6:amd64", Version: "9.6.24-1.pgdg110+1"},
		{Name: "postgresql-17", Version: "17.1-1.pgdg120+1"},
		{Name: "postgresql-client-9.6", Version: "9.6.24-1.pgdg110+1"},
		{Name: "postgresql-9.6-pglogical", Version: "2.4.5-1"},
	}
	intel := securityintel.Snapshot{
		Products: map[string]securityintel.Product{"postgresql": {Releases: []securityintel.Release{
			{Name: "9.6", ReleaseDate: "2016-09-29", EOLFrom: "2021-11-11"},
			{Name: "17", ReleaseDate: "2024-09-26", EOLFrom: "2029-11-08"},
		}}},
		Feeds: map[string]securityintel.Feed{"postgresql": {State: "fresh"}},
	}
	out := BuildSecurityActions(summary, intel, SecurityActionQuery{}, now)
	apps := map[string]LifecycleStatus{}
	for _, row := range out.Lifecycle {
		if row.Kind == "application" {
			apps[row.Component] = row
		}
	}
	if len(apps) != 2 {
		t.Fatalf("installed historical server omitted or client/extension guessed as server: %+v", apps)
	}
	if legacy := apps["postgresql-9.6:amd64"]; legacy.Product != "postgresql" || legacy.Cycle != "9.6" || legacy.State != "upstream_end_of_life" || legacy.EOLAt != "2021-11-11" {
		t.Fatalf("historical installed PostgreSQL missed its known EOL calendar: %+v", legacy)
	}
	if modern := apps["postgresql-17"]; modern.Cycle != "17" || modern.State != "supported" {
		t.Fatalf("modern PostgreSQL cycle regressed: %+v", modern)
	}
}

func TestLifecycleRecognizesHistoricalAndMinimalPythonAndPHPRuntimes(t *testing.T) {
	for _, tc := range []struct{ name, version, product, cycle, state string }{
		{"python2.7:amd64", "2.7.18-8+deb11u1", "python", "2.7", "upstream_end_of_life"},
		{"python2.7-minimal", "2.7.18-8+deb11u1", "python", "2.7", "upstream_end_of_life"},
		{"python-minimal", "2.7.18-8", "python", "2.7", "upstream_end_of_life"},
		{"python3.11-minimal", "3.11.2-6+deb12u1", "python", "3.11", "supported"},
		{"php5-fpm", "5.6.40+dfsg-0+deb8u12", "php", "5.6", "upstream_end_of_life"},
		{"php5", "5.6.40+dfsg-0+deb8u12", "php", "5.6", "upstream_end_of_life"},
		{"php8.2-cli", "8.2.29-1~deb12u1", "php", "8.2", "ending_soon"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			summary.Servers[0].Risk.Vulnerability.Inventory = []vulnerabilities.Package{{Name: tc.name, Version: tc.version}}
			intel := securityintel.Snapshot{
				Products: map[string]securityintel.Product{
					"python": {Releases: []securityintel.Release{{Name: "2.7", ReleaseDate: "2010-07-03", EOLFrom: "2020-01-01"}, {Name: "3.11", ReleaseDate: "2022-10-24", EOLFrom: "2027-10-31"}}},
					"php":    {Releases: []securityintel.Release{{Name: "5.6", ReleaseDate: "2014-08-28", EOLFrom: "2018-12-31"}, {Name: "8.2", ReleaseDate: "2022-12-08", EOLFrom: "2026-12-31"}}},
				},
				Feeds: map[string]securityintel.Feed{"python": {State: "fresh"}, "php": {State: "fresh"}},
			}
			out := BuildSecurityActions(summary, intel, SecurityActionQuery{}, now)
			var apps []LifecycleStatus
			for _, row := range out.Lifecycle {
				if row.Kind == "application" {
					apps = append(apps, row)
				}
			}
			if len(apps) != 1 || apps[0].Product != tc.product || apps[0].Cycle != tc.cycle || apps[0].State != tc.state {
				t.Fatalf("installed runtime was omitted or misidentified: %+v", apps)
			}
		})
	}
}

func TestLifecycleDoesNotInterpretPHPExtensionVersionAsRuntimeVersion(t *testing.T) {
	summary, now := securityActionFixture()
	summary.Servers[0].Risk.Vulnerability.Inventory = []vulnerabilities.Package{
		{Name: "php8.2-common", SourcePackage: "php8.2", Version: "8.2.29-1~deb12u1"},
		{Name: "php8.2-apcu", SourcePackage: "php-apcu", Version: "5.1.22+4.0.11-2"},
		{Name: "php8.2-redis", SourcePackage: "php-redis", Version: "5.3.7+4.3.0-3"},
	}
	intel := securityintel.Snapshot{
		Products: map[string]securityintel.Product{"php": {Releases: []securityintel.Release{
			{Name: "5.1", ReleaseDate: "2005-11-24", EOLFrom: "2006-08-24"},
			{Name: "5.3", ReleaseDate: "2009-06-30", EOLFrom: "2014-08-14"},
			{Name: "8.2", ReleaseDate: "2022-12-08", EOLFrom: "2026-12-31"},
		}}},
		Feeds: map[string]securityintel.Feed{"php": {State: "fresh"}},
	}
	out := BuildSecurityActions(summary, intel, SecurityActionQuery{}, now)
	var apps []LifecycleStatus
	for _, row := range out.Lifecycle {
		if row.Kind == "application" {
			apps = append(apps, row)
		}
	}
	if len(apps) != 1 || apps[0].Component != "php8.2-common" || apps[0].Cycle != "8.2" || apps[0].State == "upstream_end_of_life" {
		t.Fatalf("extension version falsely diagnosed an unsupported PHP runtime: %+v", apps)
	}
}

func TestLifecycleDiscoversLegacyRecordedApplicationsWithoutChangingScanCoverage(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	at := "2026-10-01T10:00:00Z"
	for _, accepted := range []*vulnerabilities.Scan{nil, {State: "partial", Distro: "debian 11", FinishedAt: at, Inventoried: 500}, {State: "partial", Distro: "debian 11", FinishedAt: at, Inventoried: 500, Packages: []vulnerabilities.Package{}}} {
		observation := vulnerabilities.Observation{Accepted: accepted, Findings: []vulnerabilities.Finding{
			{ID: "CVE-2026-1000", Package: "python3.9-minimal:amd64", InstalledVersion: "3.9.2-1", LastSeen: at},
			{ID: "CVE-2026-1001", Package: "python3.9-minimal:amd64", InstalledVersion: "3.9.2-1", LastSeen: at},
			{ID: "CVE-2026-1002", Package: "postgresql-9.6", InstalledVersion: "9.6.24-1", LastSeen: at},
		}}
		risk := projectVulnerabilityRisk(nil, observation, now, nil, 24*time.Hour, false)
		info := risk.Vulnerability
		summary := DashboardSummaryResponse{Servers: []DashboardServerSummary{{Name: "recorded-host", Risk: risk}}}
		intel := securityintel.Snapshot{Products: map[string]securityintel.Product{
			"python":     {Releases: []securityintel.Release{{Name: "3.9", ReleaseDate: "2020-10-05", EOLFrom: "2025-10-31"}}},
			"postgresql": {Releases: []securityintel.Release{{Name: "9.6", ReleaseDate: "2016-09-29", EOLFrom: "2021-11-11"}}},
		}, Feeds: map[string]securityintel.Feed{"python": {State: "fresh"}, "postgresql": {State: "fresh"}}}
		out := BuildSecurityActions(summary, intel, SecurityActionQuery{PageSize: 1}, now)
		if out.LifecycleTotal != 3 || out.Pages != 3 || len(out.Lifecycle) != 1 {
			t.Fatalf("legacy apps omitted or pagination changed: %+v", out)
		}
		for page := 1; page <= 2; page++ {
			row := BuildSecurityActions(summary, intel, SecurityActionQuery{PageSize: 1, Page: page}, now).Lifecycle[0]
			if row.Kind != "application" || row.State != "upstream_end_of_life" || row.InventorySource != "recorded_findings" || !row.InventoryPartial || row.InventoryObservedAt != at || row.InventoryFreshness != info.Freshness || row.SupportContext != "upstream_only_distribution_support_must_be_checked" {
				t.Fatalf("historical observation promoted or provenance lost: %+v", row)
			}
		}
		wantCoverage := "unknown"
		if accepted != nil {
			wantCoverage = "partial"
		}
		if info.Occurrences != 3 || info.ReportedCVEs != 3 || len(info.Findings) != 3 || info.Coverage != wantCoverage || info.ScanEnabled || (accepted != nil && info.Inventoried != 500) {
			t.Fatalf("lifecycle compatibility changed CVE or scan coverage: %+v", info)
		}
	}
}

func TestLifecycleRecordedVersionsUseDatedObservationsConservatively(t *testing.T) {
	for _, tc := range []struct {
		name, firstAt, nextAt, nextVersion, version, versionState, observed string
	}{
		{"latest_not_highest_version", "2026-10-01T00:00:00Z", "2026-10-02T00:00:00Z", "3.9.1", "3.9.1", "", "2026-10-02T00:00:00Z"},
		{"same_instant_conflict", "2026-10-01T00:00:00Z", "2026-09-30T20:00:00-04:00", "3.10.1", "", "conflicting", "2026-10-01T00:00:00Z"},
		{"undated_conflict", "", "invalid", "3.10.1", "", "conflicting", ""},
		{"future_cannot_replace_dated", "2026-10-01T00:00:00Z", "2099-01-01T00:00:00Z", "3.10.1", "3.9.2", "", "2026-10-01T00:00:00Z"},
		{"dated_supersedes_undated", "", "2026-10-02T00:00:00Z", "3.10.1", "3.10.1", "", "2026-10-02T00:00:00Z"},
		{"undated_known_version", "", "", "3.9.2", "3.9.2", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			summary, now := securityActionFixture()
			info := &summary.Servers[0].Risk.Vulnerability
			info.Freshness = "unknown"
			info.Findings = []DashboardVulnerabilityFinding{
				{Finding: vulnerabilities.Finding{ID: "CVE-1", Package: "python3", InstalledVersion: "3.9.2", LastSeen: tc.firstAt}, Freshness: "stale"},
				{Finding: vulnerabilities.Finding{ID: "CVE-2", Package: "python3", InstalledVersion: tc.nextVersion, LastSeen: tc.nextAt}, Freshness: "stale"},
			}
			for range 2 { // Input ordering must not select a version on a tie.
				out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{Search: "python"}, now)
				if len(out.Lifecycle) != 1 {
					t.Fatalf("expected one recognized historical application: %+v", out.Lifecycle)
				}
				row := out.Lifecycle[0]
				if row.Version != tc.version || row.InventoryVersionState != tc.versionState || row.InventoryObservedAt != tc.observed || row.InventoryFreshness != "unknown" || row.State != "unknown" {
					t.Fatalf("guessed conflicting, future or missing version evidence: %+v", row)
				}
				info.Findings[0], info.Findings[1] = info.Findings[1], info.Findings[0]
			}
		})
	}
}

func TestLifecycleRecordedFindingsNeverInferMissingInstalledApplications(t *testing.T) {
	summary, now := securityActionFixture()
	summary.Servers[0].Name = "python-php-nodejs"
	info := &summary.Servers[0].Risk.Vulnerability
	info.Findings = []DashboardVulnerabilityFinding{
		{Finding: vulnerabilities.Finding{ID: "CVE-1", Package: "nodejs", CandidateVersion: "22.1", Assessment: vulnerabilities.Assessment{SourceVersion: "18.1"}}},
		{Finding: vulnerabilities.Finding{ID: "CVE-2", Package: "library", SourcePackage: "python3", InstalledVersion: "3.9.2"}},
		{Finding: vulnerabilities.Finding{ID: "CVE-3", Package: "php8.2-apcu", InstalledVersion: "5.1.2"}},
		{Finding: vulnerabilities.Finding{ID: "CVE-4", Package: "redis-server", InstalledVersion: "6.0", ResolvedAt: "2026-10-01T00:00:00Z"}},
		{Finding: vulnerabilities.Finding{Package: "nginx", InstalledVersion: "1.18"}},
	}
	if out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now); out.LifecycleTotal != 1 || out.Lifecycle[0].Kind != "os" {
		t.Fatalf("guessed an application from non-installed or resolved evidence: %+v", out.Lifecycle)
	}
}

func TestLifecycleRecordedInventoryKeepsActualObservationFreshness(t *testing.T) {
	for _, tc := range []struct{ inventory, finding, want string }{
		{"fresh", "fresh", "fresh"},
		{"fresh", "stale", "stale"},
		{"stale", "fresh", "stale"},
		{"unknown", "fresh", "unknown"},
		{"fresh", "unknown", "unknown"},
	} {
		summary, now := securityActionFixture()
		info := &summary.Servers[0].Risk.Vulnerability
		info.Freshness = tc.inventory
		info.Findings = []DashboardVulnerabilityFinding{{Finding: vulnerabilities.Finding{ID: "CVE-1", Package: "python3", InstalledVersion: "3.9.2", LastSeen: now.Add(-time.Hour).Format(time.RFC3339)}, Freshness: tc.finding}}
		out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{Search: "python"}, now)
		if len(out.Lifecycle) != 1 || out.Lifecycle[0].InventoryFreshness != tc.want {
			t.Fatalf("inventory %s / finding %s was promoted: %+v", tc.inventory, tc.finding, out.Lifecycle)
		}
	}
}

func TestLifecycleAcceptedInventoryTakesPrecedenceOverRecordedFindings(t *testing.T) {
	for _, inventory := range [][]vulnerabilities.Package{
		{},
		{{Name: "python3", Version: "3.11.2"}},
	} {
		summary, now := securityActionFixture()
		info := &summary.Servers[0].Risk.Vulnerability
		info.Inventory, info.AcceptedAt = inventory, "2026-10-01T00:00:00Z"
		info.InventoryAvailable = true
		info.Findings = []DashboardVulnerabilityFinding{
			{Finding: vulnerabilities.Finding{ID: "CVE-1", Package: "python3", InstalledVersion: "3.9.2", LastSeen: "2026-11-01T00:00:00Z"}},
			{Finding: vulnerabilities.Finding{ID: "CVE-2", Package: "redis-server", InstalledVersion: "6.0"}},
		}
		out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now)
		if out.LifecycleTotal != 1+len(inventory) {
			t.Fatalf("fallback overrode accepted inventory: %+v", out.Lifecycle)
		}
		for _, row := range out.Lifecycle {
			if row.Kind == "application" && (row.Version != "3.11.2" || row.InventorySource != "accepted_inventory" || row.InventoryPartial || row.InventoryObservedAt != info.AcceptedAt) {
				t.Fatalf("authoritative inventory version/provenance changed: %+v", row)
			}
		}
	}
}

func TestLifecycleDoesNotReplaceAProvenEmptyInventoryWithRecordedFindings(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	start, end := "2026-11-02T10:00:00Z", "2026-11-02T11:00:00Z"
	scan := vulnerabilities.Scan{ID: "empty-inventory", ServerName: "host", Scope: "installed_packages", State: "complete", StartedAt: start, FinishedAt: end,
		InventoryProof: &vulnerabilities.InventoryProof{ScanID: "empty-inventory", ServerName: "host", Complete: true, Stable: true, ObservedAt: start, VerifiedAt: end}}
	if !scan.HasRemovalProof() {
		t.Fatal("fixture lacks authoritative empty inventory")
	}
	observation := vulnerabilities.Observation{Accepted: &scan, Findings: []vulnerabilities.Finding{{ID: "CVE-1", Package: "python3", InstalledVersion: "3.9.2", LastSeen: "2026-10-01T00:00:00Z"}}}
	risk := projectVulnerabilityRisk(nil, observation, now, nil, 24*time.Hour, false)
	summary := DashboardSummaryResponse{Servers: []DashboardServerSummary{{Name: "host", Risk: risk}}}
	if out := BuildSecurityActions(summary, securityintel.Snapshot{}, SecurityActionQuery{}, now); out.LifecycleTotal != 1 || out.Lifecycle[0].Kind != "os" {
		t.Fatalf("historical finding overrode proven empty inventory: %+v", out.Lifecycle)
	}
}

func TestSecurityActionsPaginationUsesSelectedQueue(t *testing.T) {
	summary, now := securityActionFixture()
	// One fix and three lifecycle components must not create empty fix pages.
	summary.Servers[0].Risk.Vulnerability.Inventory = []vulnerabilities.Package{{Name: "python3", Version: "3.9.2"}, {Name: "nodejs", Version: "18.1"}}
	for _, tc := range []struct {
		view              string
		pages, page, rows int
	}{
		{"", 3, 3, 0}, {"fixes", 1, 1, 1}, {"alerts", 1, 1, 0}, {"lifecycle", 3, 3, 1},
	} {
		t.Run(tc.view, func(t *testing.T) {
			values := url.Values{"view": {tc.view}, "page": {"99"}, "page_size": {"1"}}
			q, err := ParseSecurityActionQuery(values)
			if err != nil {
				t.Fatal(err)
			}
			out := BuildSecurityActions(summary, securityintel.Snapshot{}, q, now)
			rows := len(out.Fixes)
			if tc.view == "alerts" {
				rows = len(out.Alerts)
			}
			if tc.view == "lifecycle" {
				rows = len(out.Lifecycle)
			}
			if out.Pages != tc.pages || out.Page != tc.page || rows != tc.rows || out.FixTotal != 1 || out.LifecycleTotal != 3 {
				t.Fatalf("queue pagination lost totals or yielded an empty page: %+v", out)
			}
		})
	}
}
