package vulnerabilities

import "testing"

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
