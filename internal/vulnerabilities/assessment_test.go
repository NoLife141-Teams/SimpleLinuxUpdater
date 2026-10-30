package vulnerabilities

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestAssessmentPersistenceAndLegacyData(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "assessment.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	r := Repository{DB: func() *sql.DB { return db }}
	a := Assessment{Ecosystem: "Debian:13", SourcePackage: "openssh", SourceVersion: "1:1.0-1", VendorStatus: "unimportant", VendorReason: "public synthetic test", VendorPriority: "unimportant", Applicability: "source_match", Confidence: "vendor_qualified", Source: "https://example.invalid/advisory", ModifiedAt: "2026-10-01T00:00:00Z", ObservedAt: "2026-10-02T00:00:00Z"}
	scan := Scan{State: "complete", Scope: "installed_packages", Inventoried: 1, Evaluated: 1, FinishedAt: a.ObservedAt, Findings: []Finding{{ID: "CVE-2007-2243", Package: "example", Assessment: a}}}
	if _, err := r.Save(t.Context(), "example-host", scan); err != nil {
		t.Fatal(err)
	}
	o, err := r.Load(t.Context(), "example-host")
	if err != nil {
		t.Fatal(err)
	}
	if o.Findings[0].Assessment != a || o.Accepted.Findings[0].Assessment != a {
		t.Fatal("assessment lost on round-trip", o)
	}
	// The JSON is also the backup/restored observation representation.
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	var restored Observation
	if err := json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	if restored.Findings[0].Assessment != a {
		t.Fatal(restored)
	}
	var legacy Finding
	if err := json.Unmarshal([]byte(`{"id":"CVE-2026-1000","severity":"critical","source":"old-source"}`), &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.Assessment.Qualification() != QualificationReview {
		t.Fatal("legacy CVSS became vendor confirmation")
	}
	changed := scan
	changed.FinishedAt = "2026-10-03T00:00:00Z"
	changed.Findings = []Finding{{ID: "CVE-2007-2243", Package: "example"}}
	updated := Accept(o, changed)
	if updated.Findings[0].Assessment.Qualification() != QualificationReview || updated.Findings[0].FirstSeen != o.Findings[0].FirstSeen || updated.Findings[0].ResolvedAt != "" {
		t.Fatal("failed current assessment reused old triage or closed finding", updated)
	}
}
