package observability

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"debian-updater/internal/servers"
	"debian-updater/internal/vulnerabilities"
)

// UpdateCVEInfo describes only the available package plan, never host exposure.
type UpdateCVEInfo struct {
	State              string `json:"state"`
	Correctable        int    `json:"correctable"`
	IncompletePackages int    `json:"incomplete_packages"`
	CheckedAt          string `json:"checked_at,omitempty"`
}

func projectUpdateRisk(status *servers.ServerStatus, now time.Time) DashboardRiskInfo {
	risk := DashboardRiskInfo{Level: "unknown", Summary: "Check for available updates", CVEs: []string{}, UpdateCVEs: UpdateCVEInfo{State: "not_checked"}}
	if status == nil {
		return risk
	}
	risk.PendingPackages = len(status.PendingUpdates)
	if risk.PendingPackages == 0 {
		risk.PendingPackages = len(status.Upgradable)
	}
	ids, correctable := map[string]bool{}, map[string]bool{}
	refreshFailed := status.APTMetadata.LastRefreshState == "failed" || status.APTMetadata.LastRefreshState == "partial"
	for _, update := range status.PendingUpdates {
		if update.Security {
			risk.SecurityUpdates++
		}
		if update.CVEScannedAt != "" && (risk.UpdateCVEs.CheckedAt == "" || update.CVEScannedAt < risk.UpdateCVEs.CheckedAt) {
			risk.UpdateCVEs.CheckedAt = update.CVEScannedAt
		}
		incomplete := refreshFailed || update.CVEState != "ready" || update.CVECoverage != "official" || !evidenceFresh(update.CVEScannedAt, now, 24*time.Hour) || (len(update.CVEs) > 0 && len(update.CVEFindings) == 0)
		for _, finding := range update.CVEFindings {
			id := strings.TrimSpace(finding.ID)
			if id == "" {
				continue
			}
			if finding.Assessment.Qualification() == vulnerabilities.QualificationReview || !evidenceFresh(finding.Assessment.ObservedAt, now, 24*time.Hour) {
				incomplete = true
			}
			fixes := !refreshFailed && updateCandidateFix(update, finding, now)
			if !ids[id] {
				correctable[id] = fixes
			} else {
				correctable[id] = correctable[id] && fixes
			}
			ids[id] = true
		}
		if incomplete {
			risk.UpdateCVEs.IncompletePackages++
		}
	}
	if risk.PendingPackages > len(status.PendingUpdates) {
		risk.UpdateCVEs.IncompletePackages += risk.PendingPackages - len(status.PendingUpdates)
	}
	for id := range ids {
		risk.CVEs = append(risk.CVEs, id)
		if correctable[id] {
			risk.UpdateCVEs.Correctable++
		}
	}
	sort.Strings(risk.CVEs)
	switch {
	case risk.PendingPackages == 0:
		// A zero here is not a statement about installed-package vulnerabilities.
		if status.APTMetadata.LastRefreshState == "succeeded" && status.Status != "error" && status.EmptyPackageDiscoveryRefreshAt == status.APTMetadata.LastRefreshAt && evidenceFresh(status.EmptyPackageDiscoveryRefreshAt, now, 24*time.Hour) {
			risk.Summary = "No packages to update"
			risk.Level = "normal"
			risk.UpdateCVEs.State = "ready"
			risk.UpdateCVEs.CheckedAt = status.APTMetadata.LastSuccessAt
		}
	case risk.UpdateCVEs.IncompletePackages > 0:
		risk.UpdateCVEs.State = "incomplete"
	default:
		risk.UpdateCVEs.State = "ready"
	}
	if risk.PendingPackages > 0 {
		risk.Level = "normal"
		packages, fixes := "updates", "fixes"
		if risk.PendingPackages == 1 {
			packages = "update"
		}
		if risk.UpdateCVEs.Correctable == 1 {
			fixes = "fix"
		}
		risk.Summary = fmt.Sprintf("%d %s · %d security · %d verified CVE %s", risk.PendingPackages, packages, risk.SecurityUpdates, risk.UpdateCVEs.Correctable, fixes)
		if risk.UpdateCVEs.State != "ready" {
			risk.Summary = fmt.Sprintf("%d %s · %d security · CVE lookup incomplete", risk.PendingPackages, packages, risk.SecurityUpdates)
		}
		if risk.SecurityUpdates > 0 {
			risk.Level = "elevated"
		}
	}
	return risk
}

func updateCandidateFix(u servers.PendingUpdate, f vulnerabilities.Finding, now time.Time) bool {
	if u.Deferred || u.CVEState != "ready" || u.CVECoverage != "official" || u.CurrentVersion == "" || u.CandidateVersion == "" || u.CandidateVersion == u.CurrentVersion || !evidenceFresh(u.CVEScannedAt, now, 24*time.Hour) {
		return false
	}
	a := f.Availability
	if f.Disposition != "fixed_by_candidate" || f.Assessment.Qualification() != vulnerabilities.QualificationConfirmed || !evidenceFresh(f.Assessment.ObservedAt, now, 24*time.Hour) || a == nil || a.CandidateState != "addresses" || a.CandidateBinaryVersion != u.CandidateVersion || f.InstalledVersion != u.CurrentVersion || f.CandidateVersion != u.CandidateVersion || !evidenceFresh(a.QueryObservedAt, now, 24*time.Hour) {
		return false
	}
	if f.Assessment.SourcePackage != u.SourcePackage || f.SourcePackage != u.SourcePackage || f.Assessment.SourceVersion != u.InstalledSourceVersion || a.CandidateSourcePackage != u.SourcePackage || a.CandidateSourceVersion != u.CandidateSourceVersion || a.CandidateEcosystem != f.Assessment.Ecosystem {
		return false
	}
	if a.APT.LastRefreshState != "succeeded" || !evidenceFresh(a.APT.LastSuccessAt, now, 24*time.Hour) {
		return false
	}
	for _, scope := range a.Scopes {
		if scope.State == "simulated" {
			return true
		}
	}
	return false
}
