package vulnerabilities

import (
	"testing"
	"time"
)

func TestPriorityWatchCoversEverySupportedDisclosureWindow(t *testing.T) {
	observed := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		age     time.Duration
		tracked bool
	}{
		{"beyond_default", 31 * 24 * time.Hour, true},
		{"sixty_days", 60 * 24 * time.Hour, true},
		{"maximum_window", 90 * 24 * time.Hour, true},
		{"outside_window", 90*24*time.Hour + time.Second, false},
		{"future_publication", -time.Second, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := observed.Format(time.RFC3339)
			f := Finding{ID: "CVE-2026-1000", Package: "example", Severity: "critical", SeveritySource: "synthetic", PublishedAt: observed.Add(-tc.age).Format(time.RFC3339), Availability: &FixAvailability{Published: PublishedFix{State: "not_established"}}, Assessment: Assessment{Ecosystem: "Debian:12", SourcePackage: "example", SourceVersion: "1", VendorStatus: "affected", Applicability: "source_match", Confidence: "vendor_confirmed", Source: "synthetic", ObservedAt: at}}
			scan := Scan{State: "partial", FinishedAt: at, Findings: []Finding{f}}
			first := Accept(Observation{}, scan)
			if got := first.Findings[0].PrioritySince; (got != "") != tc.tracked || (tc.tracked && got != at) {
				t.Fatalf("watch at first observation = %q, tracked = %v", got, tc.tracked)
			}
			scan.FinishedAt = observed.Add(120 * 24 * time.Hour).Format(time.RFC3339)
			later := Accept(first, scan)
			if later.Findings[0].PrioritySince != first.Findings[0].PrioritySince {
				t.Fatal("later observation erased a watch after the supported disclosure window")
			}
		})
	}
}

func TestPriorityWatchRetainsOriginalPublicationAcrossLaterScans(t *testing.T) {
	f := Finding{ID: "CVE-2026-1000", Package: "example", Severity: "critical", SeveritySource: "synthetic", PublishedAt: "2026-11-01T00:00:00Z", Availability: &FixAvailability{Published: PublishedFix{State: "not_established"}}, Assessment: Assessment{Ecosystem: "Debian:12", SourcePackage: "example", SourceVersion: "1", VendorStatus: "affected", Applicability: "source_match", Confidence: "vendor_confirmed", Source: "synthetic", ObservedAt: "2026-11-02T00:00:00Z"}}
	scan := Scan{State: "partial", FinishedAt: "2026-11-02T00:00:00Z", Findings: []Finding{f}}
	first := Accept(Observation{}, scan)
	if first.Findings[0].PrioritySince != scan.FinishedAt {
		t.Fatal("recent confirmed alert was not tracked")
	}
	scan.FinishedAt = "2027-01-01T00:00:00Z"
	scan.Findings[0].PublishedAt = ""
	second := Accept(first, scan)
	if second.Findings[0].PrioritySince != first.Findings[0].PrioritySince || second.Findings[0].PublishedAt != f.PublishedAt {
		t.Fatal("alert aged out or scan date replaced publication")
	}
	scan.Findings[0].PublishedAt = "2099-01-01T00:00:00Z"
	scan.Findings[0].PrioritySince = ""
	if out := Accept(Observation{}, scan); out.Findings[0].PrioritySince != "" {
		t.Fatal("future publication created an alert")
	}
	scan.Findings[0].PublishedAt = "2026-12-30T00:00:00Z"
	scan.Findings[0].Assessment = Assessment{}
	if out := Accept(Observation{}, scan); out.Findings[0].PrioritySince != "" {
		t.Fatal("unqualified severity created a durable alert")
	}
}
