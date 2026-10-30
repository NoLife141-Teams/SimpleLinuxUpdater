// Package vulnerabilities owns accepted installed-package security observations.
package vulnerabilities

import (
	"context"
	"sort"
	"time"
)

type Finding struct {
	Runtime            RuntimeApplicability `json:"runtime,omitzero"`
	Resolution         *Resolution          `json:"resolution,omitempty"`
	Assessment         Assessment           `json:"assessment"`
	ID                 string               `json:"id"`
	Disposition        string               `json:"disposition"`
	AdvisoryURL        string               `json:"advisory_url,omitempty"`
	Source             string               `json:"source,omitempty"`
	FixedByVersion     string               `json:"fixed_by_version,omitempty"`
	Severity           string               `json:"severity"`
	SeveritySource     string               `json:"severity_source,omitempty"`
	SeverityType       string               `json:"severity_type,omitempty"`
	SeverityObservedAt string               `json:"severity_observed_at,omitempty"`
	SeverityValue      string               `json:"severity_value,omitempty"`
	Package            string               `json:"package,omitempty"`
	SourcePackage      string               `json:"source_package,omitempty"`
	InstalledVersion   string               `json:"installed_version,omitempty"`
	CandidateVersion   string               `json:"candidate_version,omitempty"`
	FixAvailable       bool                 `json:"fix_available"`
	FirstSeen          string               `json:"first_seen,omitempty"`
	LastSeen           string               `json:"last_seen,omitempty"`
	ResolvedAt         string               `json:"resolved_at,omitempty"`
}

type Package struct {
	Proof         *EvaluationProof `json:"proof,omitempty"`
	Name          string           `json:"name"`
	Version       string           `json:"version"`
	SourcePackage string           `json:"source_package,omitempty"`
	SourceVersion string           `json:"source_version,omitempty"`
	State         string           `json:"state"` // evaluated, unknown, unsupported
	Reason        string           `json:"reason,omitempty"`
}

// ScanMetrics contains measured counts and elapsed milliseconds, never estimates.
type ScanMetrics struct {
	InventoryMS     int64 `json:"inventory_ms"`
	MetadataMS      int64 `json:"metadata_ms"`
	EvaluationMS    int64 `json:"evaluation_ms"`
	EnrichmentMS    int64 `json:"enrichment_ms"`
	PersistenceMS   int64 `json:"persistence_ms"`
	UniqueQueries   int   `json:"unique_queries"`
	QueryRequests   int   `json:"query_requests"`
	QueryPages      int   `json:"query_pages"`
	QueryReuse      int   `json:"query_reuse"`
	DetailRequests  int   `json:"detail_requests"`
	DetailCacheHits int   `json:"detail_cache_hits"`
	Checkpoints     int   `json:"checkpoints"`
}

type Diagnostic struct {
	Phase string `json:"phase"`
	Code  string `json:"code"`
	Limit int    `json:"limit,omitempty"`
}

type Enrichment struct {
	RequestLimit int          `json:"request_limit"`
	Concurrency  int          `json:"concurrency"`
	BudgetMS     int64        `json:"budget_ms"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
	State        string       `json:"state"`
	Requested    int          `json:"requested"`
	Obtained     int          `json:"obtained"`
	Missing      int          `json:"missing"`
}

// Progress contains observed facts only; no estimated percentage or ETA.
type Progress struct {
	Metrics     ScanMetrics `json:"metrics,omitzero"`
	Phase       string      `json:"phase"`
	Inventoried int         `json:"inventoried"`
	Metadata    int         `json:"metadata"`
	Evaluated   int         `json:"evaluated"`
	Unknown     int         `json:"unknown"`
	Unsupported int         `json:"unsupported"`
	Findings    int         `json:"findings"`
}

type progressContextKey struct{}

func WithProgress(ctx context.Context, publish func(Progress)) context.Context {
	return context.WithValue(ctx, progressContextKey{}, publish)
}
func PublishProgress(ctx context.Context, progress Progress) {
	if publish, ok := ctx.Value(progressContextKey{}).(func(Progress)); ok {
		publish(progress)
	}
}

type Scan struct {
	Checkpoint       bool            `json:"checkpoint,omitempty"`
	Metrics          ScanMetrics     `json:"metrics,omitzero"`
	Diagnostics      []Diagnostic    `json:"diagnostics,omitempty"`
	Enrichment       Enrichment      `json:"enrichment,omitzero"`
	Truncated        bool            `json:"truncated,omitempty"`
	RuntimeContext   RuntimeContext  `json:"runtime_context,omitzero"`
	ServerName       string          `json:"server_name,omitempty"`
	InventoryProof   *InventoryProof `json:"inventory_proof,omitempty"`
	EnrichmentErrors []string        `json:"enrichment_errors,omitempty"`
	ID               string          `json:"scan_id,omitempty"`
	Progress         Progress        `json:"progress"`

	State       string    `json:"state"` // running, complete, partial, unavailable, cancelled, interrupted
	Scope       string    `json:"scope"`
	Distro      string    `json:"distro,omitempty"`
	StartedAt   string    `json:"started_at"`
	FinishedAt  string    `json:"finished_at"`
	Inventoried int       `json:"inventoried"`
	Evaluated   int       `json:"evaluated"`
	Unknown     int       `json:"unknown"`
	Unsupported int       `json:"unsupported"`
	Packages    []Package `json:"packages"`
	Findings    []Finding `json:"findings"`
	Errors      []string  `json:"errors"`
}

// Observation preserves the last accepted inventory independently of the last
// attempt. Findings are a durable ledger; only complete stable package evidence
// (or a complete stable removal inventory) can resolve a missing finding.
type Observation struct {
	PackageAbsenceProofs map[string]InventoryAbsenceProof `json:"package_absence_proofs,omitempty"`
	PackageProofs        map[string]EvaluationProof       `json:"package_proofs,omitempty"`
	InventoryProof       *InventoryProof                  `json:"inventory_proof,omitempty"`
	AuditedScanID        string                           `json:"audited_scan_id,omitempty"`
	LastAttempt          Scan                             `json:"last_attempt"`
	Accepted             *Scan                            `json:"accepted,omitempty"`
	LastSuccess          string                           `json:"last_success,omitempty"`
	Findings             []Finding                        `json:"findings"`
}

func Accept(previous Observation, scan Scan) Observation {
	if attemptIsOlder(previous, scan) {
		return previous
	}
	out := previous
	out.LastAttempt = scan
	if scan.Checkpoint && scan.State == "partial" {
		// A validated accepted boundary does not terminalize its running owner.
		out.LastAttempt.State = "running"
		out.LastAttempt.FinishedAt = ""
	}
	if scan.State != "complete" && scan.State != "partial" {
		return out
	}
	out.Accepted = &scan
	proofs, removal := validatedPackageProofs(scan), scan.HasRemovalProof()
	installed := newPackagePresence(len(scan.Packages))
	for _, p := range scan.Packages {
		installed.add(p.Name)
	}
	retainProofs(&out, scan, proofs, removal)
	complete := scan.State == "complete" && scan.Scope == "installed_packages" && scan.Unknown == 0 && scan.Unsupported == 0 && scan.Evaluated == scan.Inventoried && len(scan.Errors) == 0
	if complete {
		out.LastSuccess = scan.FinishedAt
	}
	byKey := make(map[string]Finding, len(previous.Findings)+len(scan.Findings))
	for _, f := range previous.Findings {
		byKey[f.Package+"\x00"+f.ID] = f
	}
	seen := map[string]bool{}
	for _, f := range scan.Findings {
		key := f.Package + "\x00" + f.ID
		old := byKey[key]
		oldSeen, oldErr := time.Parse(time.RFC3339, old.LastSeen)
		nowSeen, nowErr := time.Parse(time.RFC3339, scan.FinishedAt)
		if oldErr == nil && nowErr == nil && nowSeen.Before(oldSeen) {
			seen[key] = true
			continue
		}
		f.FirstSeen = old.FirstSeen
		if f.FirstSeen == "" {
			f.FirstSeen = scan.FinishedAt
		}
		if (f.Severity == "" || f.Severity == "unknown") && old.SeveritySource != "" && SeverityRank(old.Severity) >= 0 {
			f.Severity = old.Severity
			f.SeveritySource = old.SeveritySource
			f.SeverityType = old.SeverityType
			f.SeverityValue = old.SeverityValue
			f.SeverityObservedAt = old.SeverityObservedAt
		} else if f.SeveritySource != "" && f.SeverityObservedAt == "" {
			f.SeverityObservedAt = scan.FinishedAt
		}
		f.LastSeen = scan.FinishedAt
		f.ResolvedAt = ""
		f.Resolution = nil
		if f.Severity == "" {
			f.Severity = "unknown"
		}
		byKey[key] = f
		seen[key] = true
	}
	out.Findings = make([]Finding, 0, len(byKey))
	for key, f := range byKey {
		if !seen[key] && f.ResolvedAt == "" {
			if proof := resolutionFor(scan, f, proofs, installed, removal); proof != nil {
				last, err := time.Parse(time.RFC3339, f.LastSeen)
				verified, _ := time.Parse(time.RFC3339, proof.VerifiedAt)
				if err != nil || !verified.Before(last) {
					f.ResolvedAt = scan.FinishedAt
					f.Resolution = proof
				}
			}
		}
		out.Findings = append(out.Findings, f)
	}
	sort.Slice(out.Findings, func(i, j int) bool {
		if out.Findings[i].Package != out.Findings[j].Package {
			return out.Findings[i].Package < out.Findings[j].Package
		}
		return out.Findings[i].ID < out.Findings[j].ID
	})
	return out
}

// SeverityRank is an ordering of source severity categories, not a CVSS score.
func SeverityRank(severity string) int {
	switch severity {
	case "critical":
		return 4
	case "high":
		return 3
	case "medium":
		return 2
	case "low":
		return 1
	case "none":
		return 0
	}
	return -1
}

func Freshness(o Observation, now time.Time, maxAge time.Duration) string {
	if o.Accepted == nil {
		return "unknown"
	}
	at, err := time.Parse(time.RFC3339, o.Accepted.FinishedAt)
	if err != nil || (o.LastAttempt.State == "unavailable" || o.LastAttempt.State == "cancelled" || o.LastAttempt.State == "interrupted") || now.Sub(at) > maxAge || at.After(now) {
		return "stale"
	}
	return "fresh"
}
