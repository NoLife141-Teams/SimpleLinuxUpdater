package remediation

import (
	"context"
	"database/sql"
	"debian-updater/internal/vulnerabilities"
	"encoding/json"
	"errors"
	_ "modernc.org/sqlite"
	"testing"
	"time"
)

func scanFixture(id, version string, ids []string) vulnerabilities.Scan {
	at := "2026-10-30T10:00:00Z"
	s := vulnerabilities.Scan{ID: id, ServerName: "example-host", Scope: "installed_packages", State: "complete", StartedAt: at, FinishedAt: at, Inventoried: 1, Evaluated: 1}
	p := vulnerabilities.Package{Name: "example-package", Version: version, SourcePackage: "source-example", SourceVersion: version, State: "evaluated", Proof: &vulnerabilities.EvaluationProof{ScanID: id, ServerName: s.ServerName, Package: "example-package", InstalledVersion: version, SourcePackage: "source-example", SourceVersion: version, Ecosystem: "Debian:12", Source: "https://source.invalid", ObservedAt: at, VerifiedAt: at, Stable: true, Complete: true, ProvenanceVerified: true, FindingIDs: ids}}
	s.Packages = []vulnerabilities.Package{p}
	s.InventoryProof = &vulnerabilities.InventoryProof{ScanID: id, ServerName: s.ServerName, ObservedAt: at, VerifiedAt: at, Complete: true, Stable: true, Packages: []string{p.Name}}
	for _, id := range ids {
		s.Findings = append(s.Findings, vulnerabilities.Finding{ID: id, Package: p.Name, InstalledVersion: version, SourcePackage: p.SourcePackage})
	}
	return s
}
func TestCompareRequiresIndependentPackageProofAndKeepsRequalificationDistinct(t *testing.T) {
	before := vulnerabilities.Accept(vulnerabilities.Observation{}, scanFixture("before", "1", []string{"CVE-1", "CVE-2"}))
	for _, tc := range []struct {
		name   string
		change func(*vulnerabilities.Scan)
		state  string
		fully  int
	}{
		{"version changed", func(*vulnerabilities.Scan) {}, "corrected", 2},
		{"same version publisher reassessment", func(s *vulnerabilities.Scan) { *s = scanFixture("after", "1", nil) }, "requalified", 0},
		{"unknown package", func(s *vulnerabilities.Scan) {
			s.State = "partial"
			s.Packages[0].Proof = nil
			s.Packages[0].State = "unknown"
			s.Unknown = 1
		}, "not_reverified", 0},
		{"failed scan", func(s *vulnerabilities.Scan) { s.State = "unavailable" }, "not_reverified", 0},
		{"invalid proof identity", func(s *vulnerabilities.Scan) { s.Packages[0].Proof.ScanID = "unrelated" }, "not_reverified", 0},
		{"package removed", func(s *vulnerabilities.Scan) {
			s.Packages = nil
			s.Inventoried = 0
			s.Evaluated = 0
			s.InventoryProof.Packages = nil
		}, "package_removed", 2},
		{"still present", func(s *vulnerabilities.Scan) { *s = scanFixture("after", "2", []string{"CVE-1", "CVE-2"}) }, "still_present", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			after := scanFixture("after", "2", nil)
			tc.change(&after)
			result := Compare(before, after)
			if result.Counts[tc.state] != 2 || result.FullyCorrectedCVEs != tc.fully {
				t.Fatalf("%+v", result)
			}
		})
	}
}
func TestComparePartialClosesOnlyDemonstratedPackageAndSharedCVERemains(t *testing.T) {
	initial := scanFixture("before", "1", []string{"CVE-shared"})
	other := initial.Findings[0]
	other.Package = "unknown-package"
	initial.Findings = append(initial.Findings, other)
	// The unknown package has no admitted proof; its positive finding is still retained.
	before := vulnerabilities.Accept(vulnerabilities.Observation{}, initial)
	after := scanFixture("after", "2", nil)
	after.State = "partial"
	after.Unknown = 1
	after.Packages = append(after.Packages, vulnerabilities.Package{Name: "unknown-package", Version: "1", State: "unknown"})
	after.Inventoried = 2
	after.InventoryProof = nil
	result := Compare(before, after)
	if result.Counts["corrected"] != 1 || result.Counts["not_reverified"] != 1 || result.FullyCorrectedCVEs != 0 {
		t.Fatalf("%+v", result)
	}
}
func TestDurablePlanRenameDeleteRecreateAndCAS(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err = EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	r := Repository{DB: func() *sql.DB { return db }}
	ctx := context.Background()
	key, err := r.ServerKey(ctx, "example-host")
	if err != nil {
		t.Fatal(err)
	}
	before := vulnerabilities.Accept(vulnerabilities.Observation{}, scanFixture("before", "1", []string{"CVE-1"}))
	p := Plan{ID: ID(), ServerKey: key, Server: "example-host", JobID: "job-example", State: "prepared", Configuration: "opaque", Before: before}
	if err = r.Insert(ctx, p); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Load(ctx, p.ID)
	if err != nil || loaded.Before.Accepted.ID != "before" || loaded.Configuration != "opaque" {
		t.Fatalf("%+v %v", loaded, err)
	}
	raw, _ := json.Marshal(loaded)
	var public map[string]any
	_ = json.Unmarshal(raw, &public)
	if public["Before"] != nil || public["Configuration"] != nil {
		t.Fatal("private snapshot leaked", string(raw))
	}
	tx, _ := db.Begin()
	if err = RenameServerTx(tx, "example-host", "renamed-host"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if name, err := r.ServerName(ctx, key); err != nil || name != "renamed-host" {
		t.Fatal(name, err)
	}
	p.State = "queued"
	if err = r.Save(ctx, p, "prepared"); err != nil {
		t.Fatal(err)
	}
	if err = r.Save(ctx, p, "prepared"); err == nil {
		t.Fatal("stale transition accepted")
	}
	tx, _ = db.Begin()
	if err = DeleteServerTx(tx, "renamed-host"); err != nil {
		t.Fatal(err)
	}
	_ = tx.Commit()
	replacement, err := r.ServerKey(ctx, "renamed-host")
	if err != nil || replacement == key {
		t.Fatal("reused stable identity", replacement, err)
	}
	if _, err = r.ServerName(ctx, key); err == nil {
		t.Fatal("deleted plan attached to replacement")
	}
	if err = r.SaveReview(ctx, key, "renamed-host", Review{ID: ID(), Package: "example-package"}); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("review accepted stale Server ownership: %v", err)
	}
	if reviews, err := r.Reviews(ctx, replacement); err != nil || len(reviews) != 0 {
		t.Fatalf("replacement inherited old review: %+v %v", reviews, err)
	}
}
func TestRiskReviewRequiresOwnedTreatmentAndFutureReevaluation(t *testing.T) {
	now := time.Now()
	v := Review{Package: "example-package", Owner: "operator", Reason: "No installable patch", Decision: "Mitigate", ReevaluateAt: now.Add(time.Hour).UTC().Format(time.RFC3339)}
	if !v.Valid(now) {
		t.Fatal(v)
	}
	v.ReevaluateAt = now.Add(-time.Hour).UTC().Format(time.RFC3339)
	if v.Valid(now) {
		t.Fatal("expired review accepted")
	}
}
