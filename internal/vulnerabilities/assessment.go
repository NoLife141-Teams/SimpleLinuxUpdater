package vulnerabilities

// Assessment records distribution triage independently of CVSS and candidate
// versions. Applicability is a source-package match, not proof of exploitability
// of a running service. Missing legacy assessments always require review.
type Assessment struct {
	Ecosystem      string `json:"ecosystem,omitempty"`
	SourcePackage  string `json:"source_package,omitempty"`
	SourceVersion  string `json:"source_version,omitempty"`
	VendorStatus   string `json:"vendor_status"`
	VendorReason   string `json:"vendor_reason,omitempty"`
	VendorPriority string `json:"vendor_priority,omitempty"`
	Applicability  string `json:"applicability"`
	Confidence     string `json:"confidence"`
	Source         string `json:"source,omitempty"`
	ModifiedAt     string `json:"modified_at,omitempty"`
	ObservedAt     string `json:"observed_at,omitempty"`
}

const (
	QualificationConfirmed = "vendor_confirmed"
	QualificationReview    = "needs_review"
	QualificationQualified = "vendor_qualified"
	QualificationWithdrawn = "withdrawn"
)

// Qualification does not interpret CVSS as vendor triage. A vendor decision
// without its exact source identity and provenance is not usable evidence.
func (a Assessment) Qualification() string {
	if a.Ecosystem == "" || a.SourcePackage == "" || a.SourceVersion == "" || a.Source == "" || a.ObservedAt == "" {
		return QualificationReview
	}
	switch a.VendorStatus {
	case "unimportant", "not_affected":
		if a.Confidence == "vendor_qualified" && a.Applicability == "source_match" {
			return QualificationQualified
		}
	case "withdrawn":
		return QualificationWithdrawn
	case "affected", "needed":
		if a.Confidence == "vendor_confirmed" && a.Applicability == "source_match" {
			return QualificationConfirmed
		}
	}
	return QualificationReview
}
