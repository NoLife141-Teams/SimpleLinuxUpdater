package vulnerabilities

import (
	"context"
	"time"
)

// EvaluationProof attests a complete source query for one exact installed
// binary/source identity, verified again at the end of this scan. It is not a
// claim that the vulnerable code is reachable at runtime.
type EvaluationProof struct {
	ScanID             string   `json:"scan_id"`
	ServerName         string   `json:"server_name"`
	Package            string   `json:"package"`
	InstalledVersion   string   `json:"installed_version"`
	SourcePackage      string   `json:"source_package"`
	SourceVersion      string   `json:"source_version"`
	Ecosystem          string   `json:"ecosystem"`
	Source             string   `json:"source"`
	ObservedAt         string   `json:"observed_at"`
	VerifiedAt         string   `json:"verified_at"`
	ProvenanceVerified bool     `json:"provenance_verified"`
	Complete           bool     `json:"complete"`
	Stable             bool     `json:"stable"`
	FindingIDs         []string `json:"finding_ids"`
}

// InventoryProof is separate from per-package evaluation: only a complete,
// stable whole inventory can demonstrate that a previously installed package
// has been removed. Unknown CVE coverage does not invalidate an inventory read.
type InventoryProof struct {
	ScanID     string   `json:"scan_id"`
	ServerName string   `json:"server_name"`
	ObservedAt string   `json:"observed_at"`
	VerifiedAt string   `json:"verified_at"`
	Complete   bool     `json:"complete"`
	Stable     bool     `json:"stable"`
	Packages   []string `json:"packages"`
}

type Resolution struct {
	Reason           string `json:"reason"`
	ScanID           string `json:"scan_id"`
	ServerName       string `json:"server_name"`
	VerifiedAt       string `json:"verified_at"`
	InstalledVersion string `json:"installed_version,omitempty"`
	SourcePackage    string `json:"source_package,omitempty"`
	SourceVersion    string `json:"source_version,omitempty"`
	Ecosystem        string `json:"ecosystem,omitempty"`
	Source           string `json:"source,omitempty"`
}

type scanIdentity struct{ server, id string }
type scanIdentityKey struct{}

// WithScanIdentity binds collection to the admitted Server action. Persistence
// verifies the returned identity instead of rebinding old proofs to a new job.
func WithScanIdentity(ctx context.Context, server, id string) context.Context {
	return context.WithValue(ctx, scanIdentityKey{}, scanIdentity{server: server, id: id})
}

func ScanIdentity(ctx context.Context) (server, id string) {
	identity, _ := ctx.Value(scanIdentityKey{}).(scanIdentity)
	return identity.server, identity.id
}

func withinScan(scan Scan, observed, verified string) bool {
	start, e1 := time.Parse(time.RFC3339, scan.StartedAt)
	end, e2 := time.Parse(time.RFC3339, scan.FinishedAt)
	at, e3 := time.Parse(time.RFC3339, observed)
	check, e4 := time.Parse(time.RFC3339, verified)
	return e1 == nil && e2 == nil && e3 == nil && e4 == nil && !at.Before(start) && !check.Before(at) && !end.Before(check)
}

func scanMayResolve(scan Scan) bool {
	return scan.Scope == "installed_packages" && (scan.State == "complete" || scan.State == "partial") && scan.ID != "" && scan.ServerName != ""
}

func (scan Scan) PackageProof(pkg Package) *EvaluationProof {
	p := validatedPackageProofs(scan)[pkg.Name]
	if p != nil && p.InstalledVersion == pkg.Version && p.SourcePackage == pkg.SourcePackage && p.SourceVersion == pkg.SourceVersion {
		return p
	}
	return nil
}

func validatedPackageProofs(scan Scan) map[string]*EvaluationProof {
	out := map[string]*EvaluationProof{}
	counts, findings := map[string]int{}, map[string][]Finding{}
	for _, p := range scan.Packages {
		counts[p.Name]++
	}
	for _, f := range scan.Findings {
		findings[f.Package] = append(findings[f.Package], f)
	}
	for _, p := range scan.Packages {
		if counts[p.Name] == 1 && validPackageProof(scan, p, findings[p.Name]) {
			out[p.Name] = p.Proof
		}
	}
	return out
}

func validPackageProof(scan Scan, pkg Package, findings []Finding) bool {
	p := pkg.Proof
	if !scanMayResolve(scan) || pkg.State != "evaluated" || p == nil || !p.Complete || !p.Stable || !p.ProvenanceVerified ||
		p.ScanID != scan.ID || p.ServerName != scan.ServerName || p.Package != pkg.Name || p.InstalledVersion != pkg.Version ||
		p.SourcePackage != pkg.SourcePackage || p.SourceVersion != pkg.SourceVersion || p.Ecosystem == "" || p.Source == "" ||
		p.Package == "" || p.InstalledVersion == "" || p.SourcePackage == "" || p.SourceVersion == "" || !withinScan(scan, p.ObservedAt, p.VerifiedAt) {
		return false
	}
	expected, actual := map[string]bool{}, map[string]bool{}
	for _, id := range p.FindingIDs {
		if id == "" {
			return false
		}
		expected[id] = true
	}
	for _, f := range findings {
		if !expected[f.ID] || f.InstalledVersion != pkg.Version || f.SourcePackage != pkg.SourcePackage {
			return false
		}
		actual[f.ID] = true
	}
	return len(expected) == len(actual)
}

func (scan Scan) HasRemovalProof() bool {
	p := scan.InventoryProof
	if !scanMayResolve(scan) || p == nil || !p.Complete || !p.Stable || p.ScanID != scan.ID || p.ServerName != scan.ServerName ||
		len(scan.Packages) != scan.Inventoried || len(p.Packages) != scan.Inventoried || !withinScan(scan, p.ObservedAt, p.VerifiedAt) {
		return false
	}
	seen := map[string]bool{}
	for _, pkg := range scan.Packages {
		if pkg.Name == "" || pkg.Version == "" || seen[pkg.Name] {
			return false
		}
		seen[pkg.Name] = true
	}
	for _, name := range p.Packages {
		if !seen[name] {
			return false
		}
		delete(seen, name)
	}
	if len(seen) != 0 {
		return false
	}
	return true
}

// SupersedesPackageEvidence prevents an old approval snapshot from resurrecting
// findings after a newer complete package evaluation, even on a partial host.
func (o Observation) SupersedesPackageEvidence(name, observedAt string) bool {
	at, err := time.Parse(time.RFC3339, observedAt)
	if err != nil {
		return false
	}
	if p, ok := o.PackageProofs[name]; ok {
		verified, err := time.Parse(time.RFC3339, p.VerifiedAt)
		if err == nil && !verified.Before(at) {
			return true
		}
	}
	if p := o.InventoryProof; p != nil {
		verified, err := time.Parse(time.RFC3339, p.VerifiedAt)
		if err == nil && !verified.Before(at) {
			for _, pkg := range p.Packages {
				if pkg == name {
					return false
				}
			}
			return true
		}
	}
	return false
}

func retainProofs(out *Observation, scan Scan, proofs map[string]*EvaluationProof, removal bool) {
	copyProofs := make(map[string]EvaluationProof, len(out.PackageProofs)+len(scan.Packages))
	for name, p := range out.PackageProofs {
		copyProofs[name] = p
	}
	for name, p := range proofs {
		old := copyProofs[name]
		at, _ := time.Parse(time.RFC3339, p.VerifiedAt)
		previous, err := time.Parse(time.RFC3339, old.VerifiedAt)
		if err == nil && at.Before(previous) {
			continue
		}
		copy := *p
		copy.FindingIDs = append([]string{}, p.FindingIDs...)
		copyProofs[name] = copy
	}
	out.PackageProofs = copyProofs
	if removal {
		copy := *scan.InventoryProof
		copy.Packages = append([]string{}, copy.Packages...)
		out.InventoryProof = &copy
	}
}

func resolutionFor(scan Scan, f Finding, proofs map[string]*EvaluationProof, installed map[string]bool, removal bool) *Resolution {
	if !scanMayResolve(scan) {
		return nil
	}
	if !installed[f.Package] {
		if !removal {
			return nil
		}
		return &Resolution{Reason: "package_removed", ScanID: scan.ID, ServerName: scan.ServerName, VerifiedAt: scan.InventoryProof.VerifiedAt}
	}
	p := proofs[f.Package]
	if p == nil {
		return nil
	}
	for _, id := range p.FindingIDs {
		if id == f.ID {
			return nil
		}
	}
	reason := "no_longer_reported"
	if f.InstalledVersion != "" && f.InstalledVersion != p.InstalledVersion {
		reason = "installed_version_changed"
	}
	return &Resolution{Reason: reason, ScanID: scan.ID, ServerName: scan.ServerName, VerifiedAt: p.VerifiedAt, InstalledVersion: p.InstalledVersion, SourcePackage: p.SourcePackage, SourceVersion: p.SourceVersion, Ecosystem: p.Ecosystem, Source: p.Source}
}

func attemptIsOlder(previous Observation, scan Scan) bool {
	if scan.ID != previous.LastAttempt.ID {
		started, err := time.Parse(time.RFC3339, scan.StartedAt)
		latest, oldErr := time.Parse(time.RFC3339, previous.LastAttempt.StartedAt)
		if err == nil && oldErr == nil && started.Before(latest) {
			return true
		}
	}
	if scan.ID != "" && scan.ID == previous.LastAttempt.ID && (previous.LastAttempt.State == "cancelled" || previous.LastAttempt.State == "interrupted") && (scan.State == "complete" || scan.State == "partial") {
		return true
	}
	if scan.ID != "" && previous.LastAttempt.ID != "" && scan.ID != previous.LastAttempt.ID && previous.LastAttempt.State == "running" && scan.State != "running" {
		return true
	}
	if previous.Accepted != nil && (scan.State == "complete" || scan.State == "partial") {
		at, err := time.Parse(time.RFC3339, scan.FinishedAt)
		before, oldErr := time.Parse(time.RFC3339, previous.Accepted.FinishedAt)
		if err == nil && oldErr == nil && at.Before(before) {
			return true
		}
	}
	return false
}
