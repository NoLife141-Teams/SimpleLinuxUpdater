package vulnerabilities

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPositiveApprovalHistoryPreservesInventoryAndRequiresIndependentResolution(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "positive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	r := Repository{DB: func() *sql.DB { return db }}
	ctx := context.Background()
	initial := evidenceScan("initial", "2026-11-01T12:00:00Z", []Package{{Name: "example:amd64", Version: "1", State: "unknown"}}, []Finding{{ID: "CVE-OTHER", Package: "example:amd64"}})
	_, err = r.Save(ctx, "host", initial)
	if err != nil {
		t.Fatal(err)
	}
	before, err := r.Load(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	f := Finding{ID: "CVE-CRITICAL", Package: "example:amd64", InstalledVersion: "1", SourcePackage: "example", PublishedAt: "2026-11-01T00:00:00Z", LastSeen: "2026-11-02T12:00:00Z", Severity: "critical", SeveritySource: "synthetic", Assessment: Assessment{Ecosystem: "Debian:13", SourcePackage: "example", SourceVersion: "1", VendorStatus: "affected", Confidence: "vendor_confirmed", Applicability: "source_match", Source: "synthetic", ObservedAt: "2026-11-02T12:00:00Z"}, Availability: &FixAvailability{Published: PublishedFix{State: "not_established"}}}
	record := func(finding Finding) {
		t.Helper()
		if err := r.RecordPositiveFindings(ctx, "host", []Finding{finding}); err != nil {
			t.Fatal(err)
		}
	}
	load := func() Observation {
		t.Helper()
		o, err := r.Load(ctx, "host")
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	record(f)
	first := load()
	watch := finding(t, first, f.Package, f.ID)
	if watch.PrioritySince != f.LastSeen || !reflect.DeepEqual(before.Accepted, first.Accepted) || !reflect.DeepEqual(before.LastAttempt, first.LastAttempt) || !reflect.DeepEqual(before.PackageProofs, first.PackageProofs) {
		t.Fatalf("positive history changed inventory/coverage or missed watch: %+v", first)
	}
	f.LastSeen = "2027-01-01T12:00:00Z"
	f.Availability = &FixAvailability{Published: PublishedFix{State: "published"}, CandidateState: "addresses"}
	f.FixAvailable = true
	record(f)
	retained := finding(t, load(), f.Package, f.ID)
	if retained.PrioritySince != watch.PrioritySince || retained.ResolvedAt != "" || finding(t, load(), f.Package, "CVE-OTHER").ResolvedAt != "" {
		t.Fatal("candidate evidence or missing approval match resolved installed findings")
	}
	// An empty approval and a stale replay cannot erase a watch.
	if err := r.RecordPositiveFindings(ctx, "host", nil); err != nil {
		t.Fatal(err)
	}
	f.LastSeen = "2026-11-02T12:00:00Z"
	record(f)
	if got := finding(t, load(), f.Package, f.ID); got.LastSeen != retained.LastSeen || got.PrioritySince != watch.PrioritySince {
		t.Fatal("old approval overwrote history")
	}
	// A later complete package evaluation can resolve it, and its proof rejects
	// approval evidence collected before that evaluation.
	scan := evidenceScan("resolved", "2027-01-02T12:00:00Z", []Package{{Name: f.Package, Version: "2", SourcePackage: "example"}}, nil)
	if _, err = r.Save(ctx, "host", Scan{ID: scan.ID, ServerName: "host", State: "running", StartedAt: scan.StartedAt}); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.Save(ctx, "host", scan)
	if err != nil {
		t.Fatal(err)
	}
	if finding(t, resolved, f.Package, f.ID).ResolvedAt == "" {
		t.Fatal("independent installed proof failed to resolve watch")
	}
	record(f)
	if got := finding(t, load(), f.Package, f.ID); got.ResolvedAt == "" {
		t.Fatal("superseded approval resurrected resolved finding")
	}
}
