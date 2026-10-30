package vulnerabilities

// PublishedFix preserves publisher events for an exact ecosystem/source. A fixed
// event is a source version, never the binary APT candidate or a closure proof.
type VersionEvent struct {
	Introduced   string `json:"introduced,omitempty"`
	Fixed        string `json:"fixed,omitempty"`
	LastAffected string `json:"last_affected,omitempty"`
	Limit        string `json:"limit,omitempty"`
}
type FixRange struct {
	Type   string         `json:"type"`
	Events []VersionEvent `json:"events"`
}
type PublishedFix struct {
	State         string     `json:"state"` // published, not_established, unknown, withdrawn
	Ecosystem     string     `json:"ecosystem,omitempty"`
	SourcePackage string     `json:"source_package,omitempty"`
	FixedVersions []string   `json:"fixed_versions,omitempty"`
	Ranges        []FixRange `json:"ranges,omitempty"`
	Source        string     `json:"source,omitempty"`
	ObservedAt    string     `json:"observed_at,omitempty"`
	ModifiedAt    string     `json:"modified_at,omitempty"`
}

// Index file dates describe local observations, not the success of the latest
// refresh. APT does not retain a reliable last-error record; unknown stays explicit.
type APTContext struct {
	State            string `json:"state"` // stale, unknown (file ages cannot prove completeness)
	ObservedAt       string `json:"observed_at,omitempty"`
	OldestIndexAt    string `json:"oldest_index_at,omitempty"`
	LastSuccessAt    string `json:"last_success_at,omitempty"`
	LastRefreshState string `json:"last_refresh_state"`
	LastRefreshAt    string `json:"last_refresh_at,omitempty"`
	LastRefreshError string `json:"last_refresh_error,omitempty"`
}
type ScopeInstallability struct {
	Scope           string   `json:"scope"`
	State           string   `json:"state"` // simulated, blocked, not_selected, unknown
	Reason          string   `json:"reason"`
	RemovedPackages []string `json:"removed_packages,omitempty"`
}
type FixAvailability struct {
	Restrictions           []string              `json:"restrictions,omitempty"`
	Published              PublishedFix          `json:"published"`
	CandidateState         string                `json:"candidate_state"` // addresses, affected, same, absent, unverified
	CandidateSourcePackage string                `json:"candidate_source_package,omitempty"`
	CandidateSourceVersion string                `json:"candidate_source_version,omitempty"`
	CandidateBinaryVersion string                `json:"candidate_binary_version,omitempty"`
	CandidateOrigin        string                `json:"candidate_origin,omitempty"`
	CandidateEcosystem     string                `json:"candidate_ecosystem,omitempty"`
	QueryObservedAt        string                `json:"query_observed_at,omitempty"`
	RepositoryState        string                `json:"repository_state"` // observed, published_not_observed, unknown
	Reason                 string                `json:"reason"`
	Action                 string                `json:"action"`
	APT                    APTContext            `json:"apt"`
	Scopes                 []ScopeInstallability `json:"scopes,omitempty"`
}

func LegacyAvailability() FixAvailability {
	return FixAvailability{Published: PublishedFix{State: "unknown"}, CandidateState: "unverified", RepositoryState: "unknown", Reason: "No verified candidate fix in the observed legacy data; publisher fix status is unknown", Action: "Run a fresh vulnerability scan and review the maintenance package plan", APT: APTContext{State: "unknown", LastRefreshState: "unknown"}}
}

// NormalizeLegacyAvailability is a lazy JSON migration. Preserve legacy fields
// for API consumers without upgrading their claims into new availability proof.
func NormalizeLegacyAvailability(o *Observation) {
	normalize := func(findings []Finding) {
		for i := range findings {
			if findings[i].Availability == nil {
				a := LegacyAvailability()
				findings[i].Availability = &a
			}
		}
	}
	normalize(o.Findings)
	normalize(o.LastAttempt.Findings)
	if o.Accepted != nil {
		normalize(o.Accepted.Findings)
	}
}
