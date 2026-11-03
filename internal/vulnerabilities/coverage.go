package vulnerabilities

import "strings"

const (
	CoverageUnsupportedDistribution = "unsupported_distribution"
	CoverageSourceUnavailable       = "source_coverage_unavailable"
)

// DistributionEcosystem identifies known ecosystems, independently of whether
// their current advisory feed can safely assess installed packages.
func DistributionEcosystem(distroID, version string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(distroID)) {
	case "debian":
		if version == "12" || version == "13" {
			return "Debian:" + version, true
		}
	case "ubuntu":
		if version == "22.04" || version == "24.04" || version == "26.04" {
			return "Ubuntu:" + version + ":LTS", true
		}
		if version == "25.04" {
			return "Ubuntu:25.04", true
		}
	}
	return "", false
}

func DistributionCoverageReason(distroID, version string) string {
	if _, known := DistributionEcosystem(distroID, version); !known {
		return CoverageUnsupportedDistribution
	}
	// This release has unavailable current-source coverage in this application.
	// Empty responses cannot prove absence; historical snapshots are not used.
	if strings.EqualFold(strings.TrimSpace(distroID), "ubuntu") && version == "25.04" {
		return CoverageSourceUnavailable
	}
	return ""
}

// ScanCoverageReason also classifies records persisted before coverage_reason
// existed, so an already-scanned unsupported host is immediately identifiable.
func ScanCoverageReason(scan Scan) string {
	if scan.CoverageReason != "" {
		return scan.CoverageReason
	}
	parts := strings.Fields(scan.Distro)
	if len(parts) != 2 {
		return ""
	}
	return DistributionCoverageReason(parts[0], parts[1])
}

func CoverageMessage(reason, distro string) string {
	switch reason {
	case CoverageUnsupportedDistribution:
		return "Distribution not supported for CVE scanning: " + distro
	case CoverageSourceUnavailable:
		return "CVE data unavailable for " + distro + "; current-source coverage is unavailable; historical snapshots are not used"
	default:
		return ""
	}
}
