package vulnerabilities

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func evidenceScan(id, at string, packages []Package, findings []Finding) Scan {
	end, _ := time.Parse(time.RFC3339, at)
	s := Scan{ID: id, ServerName: "host", Scope: "installed_packages", State: "complete", StartedAt: end.Add(-time.Minute).Format(time.RFC3339), FinishedAt: at, Inventoried: len(packages), Packages: packages, Findings: findings}
	names := make([]string, 0, len(packages))
	for i := range s.Packages {
		p := &s.Packages[i]
		names = append(names, p.Name)
		if p.State == "" {
			p.State = "evaluated"
		}
		if p.State != "evaluated" {
			s.State = "partial"
			s.Unknown++
			continue
		}
		s.Evaluated++
		if p.SourcePackage == "" {
			p.SourcePackage = p.Name
		}
		if p.SourceVersion == "" {
			p.SourceVersion = p.Version
		}
		ids := []string{}
		for j := range s.Findings {
			f := &s.Findings[j]
			if f.Package == p.Name {
				f.InstalledVersion, f.SourcePackage = p.Version, p.SourcePackage
				ids = append(ids, f.ID)
			}
		}
		p.Proof = &EvaluationProof{ScanID: id, ServerName: s.ServerName, Package: p.Name, InstalledVersion: p.Version, SourcePackage: p.SourcePackage, SourceVersion: p.SourceVersion, Ecosystem: "Debian:13", Source: "https://example.invalid/querybatch", ObservedAt: end.Add(-30 * time.Second).Format(time.RFC3339), VerifiedAt: at, Complete: true, Stable: true, ProvenanceVerified: true, FindingIDs: ids}
	}
	s.InventoryProof = &InventoryProof{ScanID: id, ServerName: s.ServerName, ObservedAt: s.StartedAt, VerifiedAt: at, Complete: true, Stable: true, Packages: names}
	return s
}

func finding(t *testing.T, o Observation, pkg, id string) Finding {
	t.Helper()
	for _, f := range o.Findings {
		if f.Package == pkg && f.ID == id {
			return f
		}
	}
	t.Fatalf("missing %s/%s in %+v", pkg, id, o.Findings)
	return Finding{}
}

func initialEvidence() Observation {
	return Accept(Observation{}, evidenceScan("initial", "2026-10-01T12:00:00Z", []Package{{Name: "a:amd64", Version: "1"}, {Name: "b:amd64", Version: "1"}}, []Finding{{ID: "CVE-A", Package: "a:amd64"}, {ID: "CVE-B", Package: "b:amd64"}}))
}

func TestPackageProofResolutionIndependentOfUnrelatedCoverage(t *testing.T) {
	old := initialEvidence()
	scan := evidenceScan("next", "2026-10-02T12:00:00Z", []Package{{Name: "a:amd64", Version: "2"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
	next := Accept(old, scan)
	a, b := finding(t, next, "a:amd64", "CVE-A"), finding(t, next, "b:amd64", "CVE-B")
	if a.ResolvedAt != scan.FinishedAt || a.Resolution == nil || a.Resolution.Reason != "installed_version_changed" || a.Resolution.ScanID != "next" || a.Resolution.SourceVersion != "2" {
		t.Fatal(a)
	}
	if b.ResolvedAt != "" || b.LastSeen != old.Findings[1].LastSeen {
		t.Fatal(b)
	}
	if next.LastSuccess != old.LastSuccess || next.Accepted.State != "partial" {
		t.Fatal(next)
	}
	if old.PackageProofs["a:amd64"].InstalledVersion != "1" {
		t.Fatal("mutated previous observation")
	}
	if replay := Accept(next, scan); !reflect.DeepEqual(replay, next) {
		t.Fatal("replay changed evidence")
	}
	if !next.SupersedesPackageEvidence("a:amd64", "2026-10-02T11:00:00Z") || next.SupersedesPackageEvidence("a:amd64", "2026-10-02T13:00:00Z") {
		t.Fatal("approval ordering")
	}
	third := evidenceScan("later", "2026-10-03T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
	if !Accept(next, third).SupersedesPackageEvidence("a:amd64", "2026-10-02T11:00:00Z") {
		t.Fatal("later unknown scan discarded a durable proof")
	}
}

func TestEnrichmentFailureDoesNotInvalidatePackageProof(t *testing.T) {
	old := initialEvidence()
	scan := evidenceScan("new", "2026-10-02T12:00:00Z", []Package{{Name: "a:amd64", Version: "1"}, {Name: "b:amd64", Version: "1"}}, []Finding{{ID: "CVE-NEW", Package: "a:amd64", Severity: "unknown"}})
	scan.EnrichmentErrors = []string{"Advisory details unavailable"}
	next := Accept(old, scan)
	if f := finding(t, next, "a:amd64", "CVE-A"); f.Resolution == nil || f.Resolution.Reason != "no_longer_reported" {
		t.Fatal(f)
	}
	if f := finding(t, next, "a:amd64", "CVE-NEW"); f.ResolvedAt != "" || f.Severity != "unknown" {
		t.Fatal(f)
	}
	if next.LastSuccess != scan.FinishedAt {
		t.Fatal("enrichment changed evaluation completeness")
	}
}

func TestIncompleteOrMismatchedPackageProofCannotResolve(t *testing.T) {
	for name, mutate := range map[string]func(*Scan){
		"missing proof":               func(s *Scan) { s.Packages[0].Proof = nil },
		"query incomplete":            func(s *Scan) { s.Packages[0].Proof.Complete = false },
		"unstable":                    func(s *Scan) { s.Packages[0].Proof.Stable = false },
		"unverified provenance":       func(s *Scan) { s.Packages[0].Proof.ProvenanceVerified = false },
		"other scan":                  func(s *Scan) { s.Packages[0].Proof.ScanID = "other" },
		"other Server":                func(s *Scan) { s.Packages[0].Proof.ServerName = "other" },
		"version changed":             func(s *Scan) { s.Packages[0].Version = "3" },
		"source changed":              func(s *Scan) { s.Packages[0].SourceVersion = "3" },
		"missing ecosystem":           func(s *Scan) { s.Packages[0].Proof.Ecosystem = "" },
		"missing source":              func(s *Scan) { s.Packages[0].Proof.Source = "" },
		"verification outside scan":   func(s *Scan) { s.Packages[0].Proof.VerifiedAt = "2026-10-04T12:00:00Z" },
		"query outside scan":          func(s *Scan) { s.Packages[0].Proof.ObservedAt = "2026-01-01T00:00:00Z" },
		"unknown package":             func(s *Scan) { s.Packages[0].State = "unknown" },
		"duplicate selector":          func(s *Scan) { s.Packages = append(s.Packages, s.Packages[0]) },
		"missing finding from result": func(s *Scan) { s.Packages[0].Proof.FindingIDs = []string{"CVE-A"} },
		"cancelled":                   func(s *Scan) { s.State = "cancelled" },
		"interrupted":                 func(s *Scan) { s.State = "interrupted" },
		"unavailable":                 func(s *Scan) { s.State = "unavailable" },
		"wrong scope":                 func(s *Scan) { s.Scope = "pending_updates" },
		"legacy identity":             func(s *Scan) { s.ID = ""; s.ServerName = "" },
	} {
		t.Run(name, func(t *testing.T) {
			s := evidenceScan("new", "2026-10-02T12:00:00Z", []Package{{Name: "a:amd64", Version: "2"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			mutate(&s)
			next := Accept(initialEvidence(), s)
			if f := finding(t, next, "a:amd64", "CVE-A"); f.ResolvedAt != "" {
				t.Fatal(f)
			}
		})
	}
}

func TestRemovalRequiresStableCompleteInventory(t *testing.T) {
	makeScan := func() Scan {
		return evidenceScan("removed", "2026-10-02T12:00:00Z", []Package{{Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
	}
	good := Accept(initialEvidence(), makeScan())
	if f := finding(t, good, "a:amd64", "CVE-A"); f.Resolution == nil || f.Resolution.Reason != "package_removed" {
		t.Fatal(f)
	}
	if f := finding(t, good, "b:amd64", "CVE-B"); f.ResolvedAt != "" {
		t.Fatal(f)
	}
	for name, mutate := range map[string]func(*Scan){
		"no inventory":  func(s *Scan) { s.InventoryProof = nil },
		"incomplete":    func(s *Scan) { s.InventoryProof.Complete = false },
		"unstable":      func(s *Scan) { s.InventoryProof.Stable = false },
		"truncated":     func(s *Scan) { s.Inventoried++ },
		"wrong scan":    func(s *Scan) { s.InventoryProof.ScanID = "other" },
		"wrong Server":  func(s *Scan) { s.InventoryProof.ServerName = "other" },
		"wrong list":    func(s *Scan) { s.InventoryProof.Packages = []string{"a:amd64"} },
		"bad timestamp": func(s *Scan) { s.InventoryProof.VerifiedAt = "bad" },
	} {
		t.Run(name, func(t *testing.T) {
			s := makeScan()
			mutate(&s)
			if f := finding(t, Accept(initialEvidence(), s), "a:amd64", "CVE-A"); f.ResolvedAt != "" {
				t.Fatal(f)
			}
		})
	}
}

func TestInventoryAbsenceSupersedesOldApprovalAfterUnknownReinstall(t *testing.T) {
	for _, hadFinding := range []bool{false, true} {
		t.Run(map[bool]string{false: "approval only", true: "accepted finding"}[hadFinding], func(t *testing.T) {
			var old Observation
			if hadFinding {
				old = initialEvidence()
			}
			removed := evidenceScan("removed", "2026-10-02T12:00:00Z", []Package{{Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			afterRemoval := Accept(old, removed)
			approvalAt := "2026-10-01T13:00:00Z"
			if !afterRemoval.SupersedesPackageEvidence("a:amd64", approvalAt) {
				t.Fatal("complete absence did not supersede older approval")
			}
			reinstalled := evidenceScan("reinstalled", "2026-10-03T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			next := Accept(afterRemoval, reinstalled)
			if !next.SupersedesPackageEvidence("a:amd64", approvalAt) {
				t.Fatal("unknown reinstall resurrected approval evidence predating complete absence")
			}
			if next.SupersedesPackageEvidence("a:amd64", "2026-10-03T13:00:00Z") {
				t.Fatal("historical absence suppressed newer approval evidence")
			}
			if !hadFinding && len(afterRemoval.PackageAbsenceProofs) != 0 {
				t.Fatal("later inventory mutated the previous observation")
			}
			raw, err := json.Marshal(next)
			if err != nil {
				t.Fatal(err)
			}
			var restored Observation
			if err := json.Unmarshal(raw, &restored); err != nil || !restored.SupersedesPackageEvidence("a:amd64", approvalAt) {
				t.Fatal("absence cutoff did not survive JSON", err)
			}
			positive := evidenceScan("positive", "2026-10-04T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, []Finding{{Package: "a:amd64", ID: "CVE-A"}})
			current := Accept(restored, positive)
			unknown := evidenceScan("later unknown", "2026-10-05T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			if f := finding(t, Accept(current, unknown), "a:amd64", "CVE-A"); f.ResolvedAt != "" {
				t.Fatal("historical absence closed a newly observed finding", f)
			}
		})
	}
}

func TestUnverifiedInventoryCannotCreateAnAbsenceCutoff(t *testing.T) {
	for _, invalid := range []string{"incomplete", "unstable", "other scan", "other Server"} {
		t.Run(invalid, func(t *testing.T) {
			s := evidenceScan("removed", "2026-10-02T12:00:00Z", []Package{{Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			switch invalid {
			case "incomplete":
				s.InventoryProof.Complete = false
			case "unstable":
				s.InventoryProof.Stable = false
			case "other scan":
				s.InventoryProof.ScanID = "other"
			case "other Server":
				s.InventoryProof.ServerName = "other"
			}
			o := Accept(initialEvidence(), s)
			reinstalled := evidenceScan("reinstalled", "2026-10-03T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			if next := Accept(o, reinstalled); next.SupersedesPackageEvidence("a:amd64", "2026-10-01T13:00:00Z") || len(next.PackageAbsenceProofs) != 0 {
				t.Fatal("unverified inventory became a retained absence cutoff", next)
			}
		})
	}
}

func TestRetainedWholePackageAbsenceCoversApprovalSelectorAliases(t *testing.T) {
	for _, names := range [][2]string{{"a", "a:amd64"}, {"a:amd64", "a"}} {
		t.Run(names[0]+" to "+names[1], func(t *testing.T) {
			removed := evidenceScan("removed", "2026-10-02T12:00:00Z", []Package{{Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			o := Accept(Observation{}, removed)
			reinstalled := evidenceScan("reinstalled", "2026-10-03T12:00:00Z", []Package{{Name: names[1], Version: "2", State: "unknown"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
			o = Accept(o, reinstalled)
			if !o.SupersedesPackageEvidence(names[0], "2026-10-01T13:00:00Z") {
				t.Fatal("qualified spelling lost the earlier whole-package absence")
			}
			if o.SupersedesPackageEvidence(names[0], "2026-10-03T13:00:00Z") {
				t.Fatal("historical alias absence suppressed newer evidence")
			}
		})
	}
	// Absence of one explicit architecture does not establish absence of the
	// package's unqualified selector or of an installed foreign architecture.
	o := Accept(Observation{}, evidenceScan("foreign", "2026-10-02T12:00:00Z", []Package{{Name: "a:i386", Version: "1", State: "unknown"}}, nil))
	o = Accept(o, evidenceScan("native installed", "2026-10-03T12:00:00Z", []Package{{Name: "a:amd64", Version: "2", State: "unknown"}, {Name: "a:i386", Version: "1", State: "unknown"}}, nil))
	if !o.SupersedesPackageEvidence("a:amd64", "2026-10-01T13:00:00Z") || o.SupersedesPackageEvidence("a", "2026-10-01T13:00:00Z") || o.SupersedesPackageEvidence("a:i386", "2026-10-01T13:00:00Z") {
		t.Fatal("architecture-specific absence became a whole-package cutoff", o)
	}
}

func TestInventoryAbsenceDoesNotConfuseUnqualifiedAndMultiarchSelectors(t *testing.T) {
	for _, names := range [][2]string{{"lib", "lib:amd64"}, {"lib:amd64", "lib"}} {
		t.Run(names[0]+" to "+names[1], func(t *testing.T) {
			old := Accept(Observation{}, Scan{State: "partial", FinishedAt: "2026-10-01T12:00:00Z", Findings: []Finding{{Package: names[0], ID: "CVE-A"}}})
			current := evidenceScan("current", "2026-10-02T12:00:00Z", []Package{{Name: names[1], Version: "1", State: "unknown"}}, nil)
			next := Accept(old, current)
			if f := finding(t, next, names[0], "CVE-A"); f.ResolvedAt != "" || f.Resolution != nil {
				t.Fatal("architecture qualification was mistaken for package removal", f)
			}
			if next.SupersedesPackageEvidence(names[0], "2026-10-01T13:00:00Z") || len(next.PackageAbsenceProofs) != 0 {
				t.Fatal("still-present alias was recorded as absent", next)
			}
			// Also cover approval-only names newly appearing with a different
			// spelling, without a prior installed finding to seed the cutoff.
			before := Accept(Observation{}, evidenceScan("before", "2026-10-01T12:00:00Z", []Package{{Name: names[0], Version: "1", State: "unknown"}}, nil))
			after := Accept(before, current)
			if after.SupersedesPackageEvidence(names[1], "2026-10-01T11:00:00Z") || len(after.PackageAbsenceProofs) != 0 {
				t.Fatal("earlier alias was ignored while retaining absence cutoff", after)
			}
		})
	}
}

func TestInventoryAliasPresenceDoesNotBorrowArchitectureEvaluation(t *testing.T) {
	scan := evidenceScan("current", "2026-10-02T12:00:00Z", []Package{{Name: "lib:amd64", Version: "2"}, {Name: "lib:i386", Version: "1", State: "unknown"}}, nil)
	o := Accept(Observation{}, scan)
	at := "2026-10-01T12:00:00Z"
	if !o.SupersedesPackageEvidence("lib:amd64", at) || o.SupersedesPackageEvidence("lib:i386", at) || o.SupersedesPackageEvidence("lib", at) {
		t.Fatal("complete native evaluation was borrowed by an unknown architecture/alias", o)
	}
	old := Accept(Observation{}, evidenceScan("old", "2026-10-01T12:00:00Z", []Package{{Name: "lib:amd64", Version: "1"}}, []Finding{{Package: "lib:amd64", ID: "CVE-A"}}))
	foreignOnly := evidenceScan("foreign", "2026-10-02T12:00:00Z", []Package{{Name: "lib:i386", Version: "1", State: "unknown"}}, nil)
	if f := finding(t, Accept(old, foreignOnly), "lib:amd64", "CVE-A"); f.Resolution == nil || f.Resolution.Reason != "package_removed" {
		t.Fatal("distinct explicit architectures were incorrectly treated as aliases", f)
	}
}

func TestProofResolutionLegacyMultiarchReappearanceAndTimeOrdering(t *testing.T) {
	old := Accept(Observation{}, evidenceScan("old", "2026-10-01T12:00:00Z", []Package{{Name: "lib:amd64", Version: "1"}, {Name: "lib:i386", Version: "1"}}, []Finding{{Package: "lib:amd64", ID: "CVE-A"}, {Package: "lib:i386", ID: "CVE-A"}}))
	s := evidenceScan("new", "2026-10-02T12:00:00Z", []Package{{Name: "lib:amd64", Version: "2"}, {Name: "lib:i386", Version: "1", State: "unknown"}}, nil)
	next := Accept(old, s)
	if finding(t, next, "lib:amd64", "CVE-A").ResolvedAt == "" || finding(t, next, "lib:i386", "CVE-A").ResolvedAt != "" {
		t.Fatal(next)
	}
	reappeared := evidenceScan("again", "2026-10-03T12:00:00Z", []Package{{Name: "lib:amd64", Version: "3"}, {Name: "lib:i386", Version: "1", State: "unknown"}}, []Finding{{Package: "lib:amd64", ID: "CVE-A"}})
	again := Accept(next, reappeared)
	if f := finding(t, again, "lib:amd64", "CVE-A"); f.Resolution != nil || f.ResolvedAt != "" || f.FirstSeen != old.Findings[0].FirstSeen {
		t.Fatal(f)
	}
	if got := Accept(again, s); !reflect.DeepEqual(got, again) {
		t.Fatal("old scan replaced newer evidence")
	}
	legacy := Scan{State: "complete", Scope: "installed_packages", FinishedAt: "2026-10-04T12:00:00Z", Inventoried: 2, Evaluated: 2}
	if finding(t, Accept(again, legacy), "lib:amd64", "CVE-A").ResolvedAt != "" {
		t.Fatal("legacy counters closed finding")
	}
	for _, state := range []string{"cancelled", "interrupted"} {
		cancelled := next
		cancelled.LastAttempt = Scan{ID: "again", State: state}
		if got := Accept(cancelled, reappeared); !reflect.DeepEqual(got, cancelled) {
			t.Fatal("late result replaced terminal cancellation")
		}
	}
}

func TestProofRepositoryIdentityRenameDeleteAndJSONContinuity(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "proof.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	r := Repository{DB: func() *sql.DB { return db }}
	old := initialEvidence()
	if _, err = r.Save(t.Context(), "host", *old.Accepted); err != nil {
		t.Fatal(err)
	}
	s := evidenceScan("next", "2026-10-02T12:00:00Z", []Package{{Name: "a:amd64", Version: "2"}, {Name: "b:amd64", Version: "1", State: "unknown"}}, nil)
	if _, err = r.Save(t.Context(), "other", s); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("other Server proof accepted", err)
	}
	if _, err = r.Save(t.Context(), "host", Scan{ID: s.ID, State: "running", StartedAt: s.StartedAt}); err != nil {
		t.Fatal(err)
	}
	next, err := r.Save(t.Context(), "host", s)
	if err != nil {
		t.Fatal(err)
	}
	wrongOwner := s
	wrongOwner.ServerName = "other"
	if _, err := r.SaveReconciled(t.Context(), "host", wrongOwner, s.ID); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("recovery accepted a different collector owner for the admitted ID", err)
	}
	late := evidenceScan("obsolete", "2026-10-04T12:00:00Z", []Package{{Name: "a:amd64", Version: "3"}}, nil)
	if _, err = r.Save(t.Context(), "host", late); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("non-admitted terminal generation overwrote completed operation", err)
	}
	raw, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	var restored Observation
	if err = json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(next, restored) {
		t.Fatal("proof JSON roundtrip")
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = RenameServerTx(tx, "host", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	loaded, err := r.Load(context.Background(), "renamed")
	if err != nil || !loaded.SupersedesPackageEvidence("a:amd64", "2026-10-01T13:00:00Z") {
		t.Fatal(loaded, err)
	}
	if finding(t, loaded, "a:amd64", "CVE-A").Resolution.ScanID != "next" {
		t.Fatal(loaded)
	}
	if _, err = r.Save(t.Context(), "renamed", Scan{ID: "running", State: "running", StartedAt: "2026-10-03T12:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err = r.Save(t.Context(), "renamed", s); !errors.Is(err, ErrObservationChanged) {
		t.Fatal("late generation accepted", err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = DeleteServerTx(tx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	all, err := r.All(t.Context())
	if err != nil || len(all) != 0 {
		t.Fatal(all, err)
	}
}
