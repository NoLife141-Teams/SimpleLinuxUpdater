package observability

import (
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"debian-updater/internal/vulnerabilities"
)

type RemediationQuery struct {
	Server, Search, State, Package string
	Page, PageSize                 int
}

func ParseRemediationQuery(values url.Values) (RemediationQuery, error) {
	base, err := ParseVulnerabilityQuery(values)
	q := RemediationQuery{Server: base.Server, Search: base.Search, State: values.Get("state"), Package: values.Get("package"), Page: base.Page, PageSize: base.PageSize}
	if err != nil || len(q.Package) > 300 || !choice(q.State, "", "update_candidate", "no_verified_fix", "verify", "published_inaccessible", "applicability_review", "vendor_qualified", "external_owner") {
		return q, strconv.ErrSyntax
	}
	// CVE finding filters must not hide residual exposure from a correction plan.
	for _, key := range []string{"severity", "fix", "min_age", "sort", "distro"} {
		if values.Get(key) != "" {
			return q, strconv.ErrSyntax
		}
	}
	if q.Server == "" && (q.Search != "" || q.State != "" || q.Package != "") {
		return q, strconv.ErrSyntax
	}
	return q, nil
}

type RemediationHost struct {
	Server             string                `json:"server"`
	Distro             string                `json:"distro"`
	Coverage           VulnerabilityCoverage `json:"coverage"`
	AcceptedAt         string                `json:"accepted_at,omitempty"`
	CVEs               int                   `json:"cves"`
	Findings           int                   `json:"findings"`
	Packages           int                   `json:"packages"`
	CandidatePackages  int                   `json:"candidate_packages"`
	PotentialCVEs      int                   `json:"potential_cves"`
	RemainingCVEs      int                   `json:"remaining_cves"`
	UnverifiedCVEs     int                   `json:"unverified_cves"`
	NeedsRefresh       bool                  `json:"needs_refresh"`
	IncompleteCoverage bool                  `json:"incomplete_coverage"`
	Steps              []string              `json:"steps"`
}

type RemediationPackage struct {
	Package           string   `json:"package"`
	Action            string   `json:"action"`
	Severity          string   `json:"severity"`
	InstalledVersions []string `json:"installed_versions"`
	CandidateVersions []string `json:"candidate_versions"`
	InstalledVariants int      `json:"installed_variants"`
	CandidateVariants int      `json:"candidate_variants"`
	VersionConflict   bool     `json:"version_conflict"`
	NeedsRefresh      bool     `json:"needs_refresh"`
	CVEs              int      `json:"cves"`
	FixedCVEs         int      `json:"fixed_cves"`
	RemainingCVEs     int      `json:"remaining_cves"`
	UnverifiedCVEs    int      `json:"unverified_cves"`
	CVESample         []string `json:"cve_sample"`
}

type RemediationEvidence struct {
	VulnerabilityExposure
	Treatment string `json:"treatment"`
}

type RemediationPlan struct {
	GeneratedAt string                `json:"generated_at"`
	Servers     []string              `json:"servers"`
	Hosts       []RemediationHost     `json:"hosts"`
	Selected    *RemediationHost      `json:"selected,omitempty"`
	Rows        []RemediationPackage  `json:"rows"`
	Exposures   []RemediationEvidence `json:"exposures,omitempty"`
	Page        int                   `json:"page"`
	PageSize    int                   `json:"page_size"`
	Pages       int                   `json:"pages"`
	Total       int                   `json:"total"`
}

func sortedRemediationKeys(values map[string]bool) []string {
	out := make([]string, 0, len(values))
	for value := range values {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

// BuildRemediationPlan describes recorded candidate evidence only. Whole-host
// outcomes are calculated before package/search/action filtering or pagination.
func BuildRemediationPlan(snapshot DashboardSummaryResponse, q RemediationQuery) RemediationPlan {
	out := RemediationPlan{GeneratedAt: snapshot.GeneratedAt, Servers: []string{}, Hosts: []RemediationHost{}, Rows: []RemediationPackage{}, PageSize: q.PageSize}
	if out.PageSize < 1 || out.PageSize > 100 {
		out.PageSize = 50
	}
	for _, host := range snapshot.Servers {
		out.Servers = append(out.Servers, host.Name)
		info := host.Risk.Vulnerability
		coverage := VulnerabilityCoverage{Server: host.Name, Distro: info.Distro, State: info.State, Freshness: info.Freshness, Reason: info.CoverageReason, Evaluated: info.Evaluated, Inventoried: info.Inventoried, Unknown: info.Unknown, Unsupported: info.Unsupported, Errors: info.Errors[:min(3, len(info.Errors))], ErrorCount: len(info.Errors)}
		h := RemediationHost{Server: host.Name, Distro: info.Distro, Coverage: coverage, AcceptedAt: info.AcceptedAt, NeedsRefresh: info.Freshness != "fresh" || info.CoverageReason != "", IncompleteCoverage: info.Coverage != "complete" || info.State != "complete" || info.Unknown > 0 || info.Unsupported > 0 || len(info.Errors) > 0 || info.CoverageReason != "", Steps: []string{}}
		baseNeedsRefresh := h.NeedsRefresh
		byPackage := map[string][]DashboardVulnerabilityFinding{}
		for _, f := range info.Findings {
			if f.ID != "" && f.ResolvedAt == "" {
				byPackage[f.Package] = append(byPackage[f.Package], f)
				h.Findings++
			}
		}
		allCVEs, notAddressed, unverifiedCVEs := map[string]bool{}, map[string]bool{}, map[string]bool{}
		packageRows := []RemediationPackage{}
		for name, findings := range byPackage {
			row := RemediationPackage{Package: name, Action: "no_verified_fix", Severity: "unknown", NeedsRefresh: baseNeedsRefresh, CVESample: []string{}}
			installed, candidate, ids := map[string]bool{}, map[string]bool{}, map[string]bool{}
			missingInstalled, missingCandidate := false, false
			for _, f := range findings {
				if f.InstalledVersion == "" {
					missingInstalled = true
				} else {
					installed[f.InstalledVersion] = true
				}
				if f.CandidateVersion == "" {
					missingCandidate = true
				} else {
					candidate[f.CandidateVersion] = true
				}
				if f.SeveritySource != "" && vulnerabilities.SeverityRank(f.Severity) > vulnerabilities.SeverityRank(row.Severity) {
					row.Severity = f.Severity
				}
				if f.Freshness != "fresh" {
					row.NeedsRefresh = true
				}
				ids[f.ID] = true
				allCVEs[f.ID] = true
			}
			row.InstalledVariants, row.CandidateVariants = len(installed), len(candidate)
			row.InstalledVersions = sortedRemediationKeys(installed)
			row.CandidateVersions = sortedRemediationKeys(candidate)
			row.VersionConflict = len(installed) > 1 || len(candidate) > 1 || (missingInstalled && len(installed) > 0) || (missingCandidate && len(candidate) > 0)
			compatible := name != "" && !missingInstalled && !missingCandidate && len(installed) == 1 && len(candidate) == 1 && row.InstalledVersions[0] != row.CandidateVersions[0] && info.CoverageReason == ""
			assessments := map[string]string{}
			for _, f := range findings {
				assessment := "unverified"
				switch f.Disposition {
				case "fixed_by_candidate":
					if compatible && candidateRemediationFinding(f) {
						assessment = "candidate_fix"
					}
				case "still_affected", "no_fix_available":
					assessment = "remaining"
				}
				old := assessments[f.ID]
				if old != "" && old != assessment {
					assessment = "unverified"
				}
				assessments[f.ID] = assessment
			}
			if q.Server == host.Name && q.Package != "" && q.Package == name {
				for _, f := range findings {
					out.Exposures = append(out.Exposures, RemediationEvidence{VulnerabilityExposure: VulnerabilityExposure{DashboardVulnerabilityFinding: f, Server: host.Name, Distro: info.Distro}, Treatment: assessments[f.ID]})
				}
			}
			for id, assessment := range assessments {
				switch assessment {
				case "candidate_fix":
					row.FixedCVEs++
				case "remaining":
					row.RemainingCVEs++
					notAddressed[id] = true
				default:
					row.UnverifiedCVEs++
					notAddressed[id] = true
					unverifiedCVEs[id] = true
				}
			}
			row.CVEs = len(ids)
			row.CVESample = sortedRemediationKeys(ids)
			row.CVESample = row.CVESample[:min(3, len(row.CVESample))]
			row.InstalledVersions = row.InstalledVersions[:min(3, len(row.InstalledVersions))]
			row.CandidateVersions = row.CandidateVersions[:min(3, len(row.CandidateVersions))]
			if row.FixedCVEs > 0 {
				row.Action = "update_candidate"
				h.CandidatePackages++
			} else if row.UnverifiedCVEs > 0 || row.VersionConflict || name == "" || missingInstalled || info.CoverageReason != "" {
				row.Action = "verify"
			}

			allQualified, external, uncertain, published := true, false, false, false
			for _, f := range findings {
				allQualified = allQualified && (f.Qualification == vulnerabilities.QualificationQualified || f.Qualification == vulnerabilities.QualificationWithdrawn)
				external = external || f.RuntimeView.Execution == "external"
				uncertain = uncertain || f.RuntimeView.Component == "development_headers" || (f.RuntimeView.Component == "kernel_image" && f.RuntimeView.Execution != "active")
				published = published || (f.Availability != nil && f.Availability.Published.State == "published" && f.PublishedFixFreshness == "fresh")
			}
			if row.FixedCVEs == 0 {
				if allQualified {
					row.Action = "vendor_qualified"
				} else if external {
					row.Action = "external_owner"
				} else if uncertain {
					row.Action = "applicability_review"
				} else if published {
					row.Action = "published_inaccessible"
				}
			}
			h.NeedsRefresh = h.NeedsRefresh || row.NeedsRefresh
			packageRows = append(packageRows, row)
		}
		h.CVEs, h.Packages, h.UnverifiedCVEs = len(allCVEs), len(byPackage), len(unverifiedCVEs)
		h.RemainingCVEs = len(notAddressed)
		h.PotentialCVEs = h.CVEs - h.RemainingCVEs
		if h.NeedsRefresh || h.IncompleteCoverage {
			h.Steps = append(h.Steps, "Collect current package, candidate and complete scan evidence before approving changes.")
		}
		if h.CandidatePackages > 0 {
			h.Steps = append(h.Steps, "Review recorded candidate versions and the CVEs that would remain on other packages.", "Prepare remediation to refresh discovery and review the actual APT scope, extra packages, removals and health checks before explicit approval.")
		} else {
			h.Steps = append(h.Steps, "Review unresolved advisories and unverified candidates; no recorded package update is confirmed to address these findings.")
		}
		h.Steps = append(h.Steps, "After maintenance, run a complete installed-package scan to confirm which findings are resolved.")
		out.Hosts = append(out.Hosts, h)
		if q.Server == host.Name {
			selected := h
			out.Selected = &selected
			for _, row := range packageRows {
				if q.Package != "" {
					if row.Package == q.Package {
						out.Rows = append(out.Rows, row)
					}
					continue
				}
				if q.State != "" && row.Action != q.State {
					continue
				}
				search := strings.ToLower(q.Search)
				matches := search == "" || strings.Contains(strings.ToLower(row.Package), search)
				if !matches {
					for _, f := range byPackage[row.Package] {
						if strings.Contains(strings.ToLower(f.ID+" "+f.SourcePackage), search) {
							matches = true
							break
						}
					}
				}
				if matches {
					out.Rows = append(out.Rows, row)
				}
			}
		}
	}
	sort.Strings(out.Servers)
	sort.Slice(out.Hosts, func(i, j int) bool { return out.Hosts[i].Server < out.Hosts[j].Server })
	sort.Slice(out.Rows, func(i, j int) bool {
		a, b := out.Rows[i], out.Rows[j]
		if a.Severity != b.Severity {
			return vulnerabilities.SeverityRank(a.Severity) > vulnerabilities.SeverityRank(b.Severity)
		}
		if a.FixedCVEs != b.FixedCVEs {
			return a.FixedCVEs > b.FixedCVEs
		}
		return a.Package < b.Package
	})
	out.Total = len(out.Rows)
	if q.Package != "" {
		sort.Slice(out.Exposures, func(i, j int) bool { return out.Exposures[i].ID < out.Exposures[j].ID })
		out.Total = len(out.Exposures)
	} else if q.Server == "" {
		out.Total = len(out.Hosts)
	}
	out.Pages = max(1, (out.Total+out.PageSize-1)/out.PageSize)
	out.Page = min(max(1, q.Page), out.Pages)
	start := (out.Page - 1) * out.PageSize
	end := min(start+out.PageSize, out.Total)
	if q.Package != "" {
		out.Exposures = out.Exposures[start:end]
	} else if q.Server == "" {
		out.Hosts = out.Hosts[start:end]
	} else {
		out.Rows = out.Rows[start:end]
		out.Hosts = []RemediationHost{}
	}
	return out
}

// Candidate treatment requires current independent source/candidate facts, never a legacy fixed flag.
func candidateRemediationFinding(f DashboardVulnerabilityFinding) bool {
	if f.EvidenceState != "current" || f.Qualification != vulnerabilities.QualificationConfirmed || f.AvailabilityFreshness != "fresh" || f.Availability == nil || f.Availability.CandidateState != "addresses" {
		return false
	}
	if f.RuntimeView.Execution == "external" || f.RuntimeView.Component == "development_headers" || (f.RuntimeView.Component == "kernel_image" && f.RuntimeView.Execution != "active") {
		return false
	}
	for _, scope := range f.Availability.Scopes {
		if scope.State == "simulated" {
			return true
		}
	}
	return false
}

// ProjectRemediationEvidence shares the canonical qualification, runtime and package-proof projection.
func ProjectRemediationEvidence(o vulnerabilities.Observation, now time.Time) DashboardVulnerabilityInfo {
	return projectVulnerabilityRisk(nil, o, now, nil, 24*time.Hour, true).Vulnerability
}

// CandidateRemediationFinding exposes the shared conservative treatment rule for immutable scope estimates.
func CandidateRemediationFinding(f DashboardVulnerabilityFinding) bool {
	return candidateRemediationFinding(f)
}
