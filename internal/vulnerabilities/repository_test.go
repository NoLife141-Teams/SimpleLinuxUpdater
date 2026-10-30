package vulnerabilities

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestObservationContinuityResolutionAndReappearance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "security.db")
	open := func() *sql.DB {
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if err := EnsureSchema(db); err != nil {
			t.Fatal(err)
		}
		return db
	}
	db := open()
	defer func() { db.Close() }()
	repository := Repository{DB: func() *sql.DB { return db }}
	ctx := context.Background()
	scan := Scan{State: "complete", Scope: "installed_packages", Inventoried: 1, Evaluated: 1, FinishedAt: "2026-10-01T00:00:00Z", Findings: []Finding{{ID: "CVE-2026-1000", Package: "libssl3", Severity: "unknown", Disposition: "no_fix_available"}}}
	save := func(value Scan) Observation {
		o, err := repository.Save(ctx, "host", value)
		if err != nil {
			t.Fatal(err)
		}
		return o
	}
	scan.RuntimeContext = RuntimeContext{Virtualization: "container", KernelRelease: "synthetic-external", Source: "injected-session", ObservedAt: scan.FinishedAt}
	scan.Findings[0].Runtime = ClassifyRuntime("libssl3", "1", "openssl", "1", scan.RuntimeContext)
	first := save(scan)
	scan.FinishedAt = "2026-10-02T00:00:00Z"
	second := save(scan)
	if second.Findings[0].FirstSeen != first.Findings[0].FirstSeen || second.Findings[0].LastSeen != scan.FinishedAt {
		t.Fatal(second)
	}
	scan.State = "unavailable"
	scan.Findings = nil
	scan.FinishedAt = "2026-10-03T00:00:00Z"
	failed := save(scan)
	if failed.Accepted.FinishedAt != second.Accepted.FinishedAt || failed.Findings[0].ResolvedAt != "" || Freshness(failed, time.Now(), 24*time.Hour) != "stale" {
		t.Fatal(failed)
	}
	scan.State = "partial"
	scan.Evaluated = 0
	scan.Unknown = 1
	if partial := save(scan); partial.Findings[0].ResolvedAt != "" {
		t.Fatal(partial)
	}
	db.Close()
	db = open()
	restarted, err := repository.Load(ctx, "host")
	if err != nil {
		t.Fatal(err)
	}
	if restarted.Accepted.RuntimeContext.Virtualization != "container" || restarted.Findings[0].Runtime.Source != "injected-session" || restarted.Findings[0].FirstSeen != first.Findings[0].FirstSeen || restarted.Findings[0].ResolvedAt != "" {
		t.Fatal(restarted)
	}
	scan.State = "complete"
	scan.Unknown = 0
	scan.Evaluated = 1
	scan.FinishedAt = "2026-10-04T00:00:00Z"
	scan = evidenceScan("resolve", scan.FinishedAt, []Package{{Name: "libssl3", Version: "2"}}, nil)
	if resolved := save(scan); resolved.Findings[0].ResolvedAt != scan.FinishedAt {
		t.Fatal(resolved)
	}
	scan.Findings = first.Findings
	scan.FinishedAt = "2026-10-05T00:00:00Z"
	if again := save(scan); again.Findings[0].ResolvedAt != "" || again.Findings[0].FirstSeen != first.Findings[0].FirstSeen {
		t.Fatal(again)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := RenameServerTx(tx, "host", "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	renamed, err := repository.Load(ctx, "renamed")
	if err != nil || len(renamed.Findings) != 1 || renamed.Findings[0].Runtime.Source != "injected-session" {
		t.Fatalf("%+v %v", renamed, err)
	}
	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err := DeleteServerTx(tx, "renamed"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	all, err := repository.All(ctx)
	if err != nil || len(all) != 0 {
		t.Fatalf("%+v %v", all, err)
	}
}

func TestPartialOrInconsistentScanCannotResolve(t *testing.T) {
	previous := Observation{Findings: []Finding{{ID: "CVE-2026-1000", Package: "pkg", FirstSeen: "2026-01-01T00:00:00Z"}}}
	for _, scan := range []Scan{
		{State: "partial", Scope: "installed_packages"},
		{State: "complete", Scope: "pending_updates"},
		{State: "complete", Scope: "installed_packages", Inventoried: 1},
		{State: "complete", Scope: "installed_packages", Unknown: 1},
		{State: "complete", Scope: "installed_packages", Unsupported: 1},
		{State: "complete", Scope: "installed_packages", Errors: []string{"partial source failure"}},
	} {
		if got := Accept(previous, scan); got.Findings[0].ResolvedAt != "" {
			t.Fatal(got)
		}
	}
}

func TestKnownSeverityEvidenceSurvivesPartialDetailFailure(t *testing.T) {
	first := Accept(Observation{}, Scan{State: "complete", Scope: "installed_packages", FinishedAt: "2026-10-01T00:00:00Z", Findings: []Finding{{ID: "CVE-2026-1000", Package: "pkg", Severity: "high", SeveritySource: "OSV", SeverityValue: "vector"}}})
	next := Accept(first, Scan{State: "partial", Scope: "installed_packages", FinishedAt: "2026-10-02T00:00:00Z", Findings: []Finding{{ID: "CVE-2026-1000", Package: "pkg", Severity: "unknown"}}, Errors: []string{"severity unavailable"}})
	f := next.Findings[0]
	if f.Severity != "high" || f.SeverityValue != "vector" || f.SeveritySource != "OSV" || f.SeverityObservedAt != "2026-10-01T00:00:00Z" || f.LastSeen != "2026-10-02T00:00:00Z" {
		t.Fatal(f)
	}
}

func TestSaveGuardedCancelledCommitPreservesAcceptedFindings(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "cancel.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repository := Repository{DB: func() *sql.DB { return db }}
	previous, err := repository.Save(t.Context(), "host", Scan{ID: "old", State: "complete", Scope: "installed_packages", FinishedAt: "2026-10-01T00:00:00Z", Findings: []Finding{{ID: "CVE-2026-1000", Package: "pkg"}}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	_, err = repository.SaveGuarded(ctx, "host", Scan{ID: "new", State: "complete", Scope: "installed_packages", FinishedAt: "2026-10-02T00:00:00Z"}, func(commit func() error) error { cancel(); return ctx.Err() })
	if err == nil {
		t.Fatal("cancelled transaction accepted result")
	}
	after, err := repository.Load(t.Context(), "host")
	if err != nil || after.LastAttempt.ID != "old" || after.Accepted.FinishedAt != previous.Accepted.FinishedAt || after.Findings[0].ResolvedAt != "" {
		t.Fatal(after, err)
	}
}

func TestSaveReconciledRequiresTheOriginalAttempt(t *testing.T) {
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "reconcile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := EnsureSchema(db); err != nil {
		t.Fatal(err)
	}
	repository := Repository{DB: func() *sql.DB { return db }}
	for _, tc := range []struct {
		name, storedID, expectedID string
		missing                    bool
	}{
		{name: "deleted", expectedID: "old", missing: true},
		{name: "deleted legacy", missing: true},
		{name: "replaced", storedID: "new", expectedID: "old"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !tc.missing {
				if _, err := repository.Save(t.Context(), tc.name, Scan{ID: tc.storedID, State: "running"}); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := repository.SaveReconciled(t.Context(), tc.name, Scan{ID: tc.expectedID, State: "interrupted"}, tc.expectedID); !errors.Is(err, ErrObservationChanged) {
				t.Fatalf("stale reconciliation error = %v", err)
			}
			next, err := repository.Load(t.Context(), tc.name)
			if err != nil || next.LastAttempt.ID != tc.storedID || next.LastAttempt.State == "interrupted" {
				t.Fatalf("overwrote changed observation: %+v %v", next, err)
			}
		})
	}
}
