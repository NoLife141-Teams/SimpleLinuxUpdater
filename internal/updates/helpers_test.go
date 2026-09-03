package updates

import (
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestIsSudoPolicyError(t *testing.T) {
	tests := []struct {
		name    string
		message string
		want    bool
	}{
		{name: "password required", message: "sudo: a password is required", want: true},
		{name: "sudo unavailable to user", message: "operator is not allowed to run sudo on host", want: true},
		{name: "command denied", message: "Sorry, user operator is not allowed to execute '/usr/bin/apt-get update' as root on host.", want: true},
		{name: "not in sudoers", message: "operator is not in the sudoers file", want: true},
		{name: "ordinary apt failure", message: "E: The repository is not signed", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsSudoPolicyError(tt.message); got != tt.want {
				t.Fatalf("IsSudoPolicyError(%q) = %t, want %t", tt.message, got, tt.want)
			}
		})
	}
}

func TestParseUpgradableEntriesAndPackageSelection(t *testing.T) {
	stdout := strings.Join([]string{
		"NOTE: noise",
		"Inst openssl [3.0.1] (3.0.2 Ubuntu:22.04/jammy-security [amd64])",
		"Inst curl [7.1] (7.2 Ubuntu:22.04/jammy-updates [amd64])",
	}, "\n")
	pending, upgradable, err := ParseUpgradableEntries(stdout)
	if err != nil {
		t.Fatalf("ParseUpgradableEntries() error = %v", err)
	}
	if len(upgradable) != 2 {
		t.Fatalf("upgradable count = %d, want 2", len(upgradable))
	}
	if pending[0].Package != "openssl" || !pending[0].Security || pending[0].CurrentVersion != "3.0.1" || pending[0].CandidateVersion != "3.0.2" {
		t.Fatalf("first pending update = %+v, want parsed security openssl", pending[0])
	}
	if got := SecurityPackagesFromPendingUpdates(pending); !reflect.DeepEqual(got, []string{"openssl"}) {
		t.Fatalf("SecurityPackagesFromPendingUpdates() = %#v, want openssl", got)
	}
}

func TestFailedSystemdUnitHelpers(t *testing.T) {
	output := strings.Join([]string{
		"ssh.service loaded failed failed OpenBSD Secure Shell server",
		"",
		"postfix@-.service loaded failed failed Postfix Mail Transport Agent",
		"ssh.service loaded failed failed duplicate",
	}, "\n")
	units := ParseFailedSystemdUnits(output)
	wantUnits := []string{"ssh.service", "postfix@-.service"}
	if !reflect.DeepEqual(units, wantUnits) {
		t.Fatalf("ParseFailedSystemdUnits() = %#v, want %#v", units, wantUnits)
	}

	allUnits := []string{"a.service", "b.service", "c.service"}
	if got := SummarizeUnitNames(allUnits, 0); got != "a.service, b.service, c.service" {
		t.Fatalf("SummarizeUnitNames(max=0) = %q", got)
	}
	if got := SummarizeUnitNames(allUnits, 3); got != "a.service, b.service, c.service" {
		t.Fatalf("SummarizeUnitNames(max=3) = %q", got)
	}
	if got := SummarizeUnitNames(allUnits, 2); got != "a.service, b.service (+1 more)" {
		t.Fatalf("SummarizeUnitNames(max=2) = %q", got)
	}
	if got := SummarizeUnitNames(nil, 2); got != "" {
		t.Fatalf("SummarizeUnitNames(nil) = %q, want empty", got)
	}
}

func TestParseUpgradableEntriesAptSummaryBlock(t *testing.T) {
	stdout := strings.Join([]string{
		"Reading package lists... Done",
		"Building dependency tree... Done",
		"Reading state information... Done",
		"Calculating upgrade... Done",
		"The following packages will be upgraded:",
		"  apache2-utils base-files bash bind9-dnsutils bind9-host bind9-libs certbot",
		"  distro-info-data dpkg dpkg-dev e2fsprogs ifupdown inetutils-telnet jq",
		"  libpython3.13-stdlib sed sqv ssh sudo systemd",
		"73 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	pending, upgradable, err := ParseUpgradableEntries(stdout)
	if err != nil {
		t.Fatalf("ParseUpgradableEntries() error = %v", err)
	}
	want := []string{
		"apache2-utils", "base-files", "bash", "bind9-dnsutils", "bind9-host", "bind9-libs", "certbot",
		"distro-info-data", "dpkg", "dpkg-dev", "e2fsprogs", "ifupdown", "inetutils-telnet", "jq",
		"libpython3.13-stdlib", "sed", "sqv", "ssh", "sudo", "systemd",
	}
	if !reflect.DeepEqual(upgradable, want) {
		t.Fatalf("upgradable = %#v, want %#v", upgradable, want)
	}
	if len(pending) != len(want) || pending[0].Package != "apache2-utils" || pending[len(pending)-1].Package != "systemd" {
		t.Fatalf("pending updates = %+v, want package entries from summary block", pending)
	}
}

func TestParseAptListMetadataEntriesKeepsSourceAndSecurity(t *testing.T) {
	stdout := strings.Join([]string{
		"Listing...",
		"bash/stable 5.2.15-2+b8 amd64 [upgradable from: 5.2.15-2+b7]",
		"openssl/stable-security 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]",
		"ignored/stable 1.0 amd64 [upgradable from: 0.9]",
	}, "\n")
	pending, upgradable := ParseAptListMetadataEntries(stdout, []string{"openssl", "bash"})
	if len(pending) != 2 || len(upgradable) != 2 {
		t.Fatalf("len(pending)=%d len(upgradable)=%d, want 2/2", len(pending), len(upgradable))
	}
	if pending[0].Package != "openssl" || pending[0].Source != "stable-security" || pending[0].CandidateVersion != "3.0.17-1~deb12u2" || pending[0].CurrentVersion != "3.0.16-1~deb12u1" || !pending[0].Security {
		t.Fatalf("first pending update = %+v, want security openssl with apt source metadata", pending[0])
	}
	if pending[1].Package != "bash" || pending[1].Source != "stable" || pending[1].Security {
		t.Fatalf("second pending update = %+v, want non-security bash with source metadata", pending[1])
	}
	if !strings.Contains(upgradable[0], "openssl/stable-security") || !strings.Contains(upgradable[1], "bash/stable") {
		t.Fatalf("upgradable = %#v, want raw apt-list metadata lines in package-filter order", upgradable)
	}
}

func TestParseAptListMetadataEntrySecurityFromCommaDelimitedSource(t *testing.T) {
	line := "openssl/jammy-updates,jammy-security 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]"
	update, ok := ParseAptListMetadataEntry(line)
	if !ok {
		t.Fatalf("ParseAptListMetadataEntry() ok = false, want true")
	}
	if update.Source != "jammy-updates,jammy-security" || !update.Security {
		t.Fatalf("parsed update = %+v, want comma-delimited source and security=true", update)
	}
}

func TestParseAptListMetadataEntriesMatchesArchQualifiedSummaryPackages(t *testing.T) {
	stdout := strings.Join([]string{
		"Listing...",
		"openssl/stable 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]",
		"openssl/stable-security 3.0.18-1~deb12u2 i386 [upgradable from: 3.0.16-1~deb12u1]",
		"bash/stable 5.2.15-2+b8 amd64 [upgradable from: 5.2.15-2+b7]",
	}, "\n")
	pending, upgradable := ParseAptListMetadataEntries(stdout, []string{"openssl:i386", "bash"})
	if len(pending) != 2 || len(upgradable) != 2 {
		t.Fatalf("len(pending)=%d len(upgradable)=%d, want 2/2", len(pending), len(upgradable))
	}
	if pending[0].Package != "openssl:i386" || pending[0].CandidateVersion != "3.0.18-1~deb12u2" || pending[0].Source != "stable-security" || !pending[0].Security {
		t.Fatalf("pending[0] = %+v, want exact i386 security metadata", pending[0])
	}
	if pending[1].Package != "bash" || pending[1].Source != "stable" {
		t.Fatalf("pending[1] = %+v, want bash package metadata", pending[1])
	}
	if strings.Contains(upgradable[0], "amd64") || !strings.Contains(upgradable[0], "i386") {
		t.Fatalf("upgradable[0] = %q, want exact i386 metadata line", upgradable[0])
	}
}

func TestParseAptListMetadataEntriesDoesNotOverwriteBaseWithForeignArch(t *testing.T) {
	stdout := strings.Join([]string{
		"Listing...",
		"openssl/stable 3.0.17-amd64 amd64 [upgradable from: 3.0.16-amd64]",
		"openssl/stable-security 3.0.18-i386 i386 [upgradable from: 3.0.16-i386]",
	}, "\n")
	pending, upgradable := ParseAptListMetadataEntries(stdout, []string{"openssl"})
	if len(pending) != 1 || len(upgradable) != 1 {
		t.Fatalf("len(pending)=%d len(upgradable)=%d, want 1/1", len(pending), len(upgradable))
	}
	if pending[0].Package != "openssl" || pending[0].CandidateVersion != "3.0.17-amd64" || pending[0].Security {
		t.Fatalf("pending[0] = %+v, want first base-package metadata without foreign-arch overwrite", pending[0])
	}
	if strings.Contains(upgradable[0], "i386") {
		t.Fatalf("upgradable[0] = %q, want base package metadata, not foreign-arch metadata", upgradable[0])
	}
}

func TestNeedsAptListMetadataOnlyForSummaryFallback(t *testing.T) {
	if !NeedsAptListMetadata([]servers.PendingUpdate{{Package: "openssl", Raw: "openssl", CVEs: []string{}}}) {
		t.Fatalf("NeedsAptListMetadata(summary fallback) = false, want true")
	}
	if NeedsAptListMetadata([]servers.PendingUpdate{{Package: "openssl", Source: "stable-security", Security: true}}) {
		t.Fatalf("NeedsAptListMetadata(enriched update) = true, want false")
	}
	if !NeedsAptListMetadata([]servers.PendingUpdate{{Package: "debian-security-support", Raw: "debian-security-support", Security: true, CVEs: []string{}}}) {
		t.Fatalf("NeedsAptListMetadata(summary package with security marker) = false, want true")
	}
}

func TestMergePendingUpdatesWithMetadataKeepsSummaryFallbackForMissingPackages(t *testing.T) {
	summaryPending := []servers.PendingUpdate{
		{Package: "openssl", Raw: "openssl", CVEs: []string{}},
		{Package: "bash", Raw: "bash", CVEs: []string{}},
	}
	metadataPending := []servers.PendingUpdate{
		{
			Package:          "openssl",
			CurrentVersion:   "3.0.16-1~deb12u1",
			CandidateVersion: "3.0.17-1~deb12u2",
			Source:           "stable-security",
			Security:         true,
			Raw:              "openssl/stable-security 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]",
			CVEs:             []string{},
		},
	}
	mergedPending, mergedUpgradable := MergePendingUpdatesWithMetadata(summaryPending, metadataPending)
	if len(mergedPending) != 2 || len(mergedUpgradable) != 2 {
		t.Fatalf("len(mergedPending)=%d len(mergedUpgradable)=%d, want 2/2", len(mergedPending), len(mergedUpgradable))
	}
	if mergedPending[0].Package != "openssl" || mergedPending[0].Source != "stable-security" || !mergedPending[0].Security {
		t.Fatalf("mergedPending[0] = %+v, want enriched openssl metadata", mergedPending[0])
	}
	if mergedPending[1].Package != "bash" || mergedPending[1].Source != "" || mergedPending[1].Security {
		t.Fatalf("mergedPending[1] = %+v, want summary fallback bash", mergedPending[1])
	}
	if !strings.Contains(mergedUpgradable[0], "openssl/stable-security") || mergedUpgradable[1] != "bash" {
		t.Fatalf("mergedUpgradable = %#v, want enriched openssl raw and fallback bash raw", mergedUpgradable)
	}
}

func TestMergePendingUpdatesWithMetadataKeepsArchSummaryWhenExactMetadataMissing(t *testing.T) {
	summaryPending := []servers.PendingUpdate{
		{Package: "openssl:i386", Raw: "openssl:i386", CVEs: []string{}},
		{Package: "bash", Raw: "bash", CVEs: []string{}},
	}
	metadataPending := []servers.PendingUpdate{
		{
			Package:          "openssl",
			CurrentVersion:   "3.0.16-1~deb12u1",
			CandidateVersion: "3.0.17-1~deb12u2",
			Source:           "stable-security",
			Security:         true,
			Raw:              "openssl/stable-security 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]",
			CVEs:             []string{},
		},
	}
	mergedPending, mergedUpgradable := MergePendingUpdatesWithMetadata(summaryPending, metadataPending)
	if len(mergedPending) != 2 || len(mergedUpgradable) != 2 {
		t.Fatalf("len(mergedPending)=%d len(mergedUpgradable)=%d, want 2/2", len(mergedPending), len(mergedUpgradable))
	}
	if mergedPending[0].Package != "openssl:i386" || mergedPending[0].Source != "" || mergedPending[0].Security {
		t.Fatalf("mergedPending[0] = %+v, want arch-qualified summary fallback without base metadata", mergedPending[0])
	}
	if mergedPending[1].Package != "bash" || mergedPending[1].Source != "" || mergedPending[1].Security {
		t.Fatalf("mergedPending[1] = %+v, want summary fallback bash", mergedPending[1])
	}
	if got := SecurityPackagesFromPendingUpdates(mergedPending); len(got) != 0 {
		t.Fatalf("SecurityPackagesFromPendingUpdates() = %#v, want no security packages without exact arch metadata", got)
	}
}

func TestMergePendingUpdatesWithMetadataPrefersExactArchMetadata(t *testing.T) {
	summaryPending := []servers.PendingUpdate{
		{Package: "openssl:i386", Raw: "openssl:i386", CVEs: []string{}},
		{Package: "openssl:amd64", Raw: "openssl:amd64", CVEs: []string{}},
	}
	metadataPending := []servers.PendingUpdate{
		{
			Package:          "openssl:amd64",
			CurrentVersion:   "3.0.16-amd64",
			CandidateVersion: "3.0.17-amd64",
			Source:           "stable",
			Raw:              "openssl/stable 3.0.17-amd64 amd64 [upgradable from: 3.0.16-amd64]",
			CVEs:             []string{},
		},
		{
			Package:          "openssl:i386",
			CurrentVersion:   "3.0.16-i386",
			CandidateVersion: "3.0.18-i386",
			Source:           "stable-security",
			Security:         true,
			Raw:              "openssl/stable-security 3.0.18-i386 i386 [upgradable from: 3.0.16-i386]",
			CVEs:             []string{},
		},
	}
	mergedPending, _ := MergePendingUpdatesWithMetadata(summaryPending, metadataPending)
	if len(mergedPending) != 2 {
		t.Fatalf("len(mergedPending)=%d, want 2", len(mergedPending))
	}
	if mergedPending[0].Package != "openssl:i386" || mergedPending[0].CandidateVersion != "3.0.18-i386" || !mergedPending[0].Security {
		t.Fatalf("mergedPending[0] = %+v, want exact i386 security metadata", mergedPending[0])
	}
	if mergedPending[1].Package != "openssl:amd64" || mergedPending[1].CandidateVersion != "3.0.17-amd64" || mergedPending[1].Security {
		t.Fatalf("mergedPending[1] = %+v, want exact amd64 non-security metadata", mergedPending[1])
	}
}

func TestAptListUpgradableCmdForcesCLocaleWithoutSudo(t *testing.T) {
	want := ReadOnlyAptCommand("apt-get -s upgrade")
	if AptListUpgradableCmd != want {
		t.Fatalf("AptListUpgradableCmd = %q, want %q", AptListUpgradableCmd, want)
	}
	if strings.Contains(AptListUpgradableCmd, "sudo") {
		t.Fatalf("AptListUpgradableCmd = %q, should not require sudo or sudo SETENV", AptListUpgradableCmd)
	}
}

func TestBuildSelectedUpgradeCmdUsesValidatedTypedHelperOperation(t *testing.T) {
	got := BuildSelectedUpgradeCmd([]string{"openssl", "libssl3:amd64"})
	for _, required := range []string{"/usr/bin/apt-get", "install --only-upgrade -- 'openssl' 'libssl3:amd64'", RootHelperPath + " 'install-only-upgrade' 'openssl' 'libssl3:amd64'"} {
		if !strings.Contains(got, required) {
			t.Fatalf("BuildSelectedUpgradeCmd() = %q, missing %q", got, required)
		}
	}
	if got := BuildSelectedUpgradeCmd([]string{"openssl", "libfoo'bar"}); got != "" {
		t.Fatalf("BuildSelectedUpgradeCmd() accepted invalid selector: %q", got)
	}
}

func TestKeptBackSecurityPackagesFromPendingUpdates(t *testing.T) {
	updates := []servers.PendingUpdate{
		{Package: "openssl", Security: true},
		{Package: "linux-base", Security: true, KeptBack: false},
		{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
		{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
		{Package: "docker-ce", Security: false, KeptBack: true, RequiresFull: true},
		{Package: "linux-headers-amd64", Security: true, RequiresFull: true},
	}
	got := KeptBackSecurityPackagesFromPendingUpdates(updates)
	want := []string{"linux-headers-amd64", "linux-image-amd64"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("KeptBackSecurityPackagesFromPendingUpdates() = %#v, want %#v", got, want)
	}
	if got := SecurityPackagesFromPendingUpdates(updates); !reflect.DeepEqual(got, []string{"linux-base", "openssl"}) {
		t.Fatalf("SecurityPackagesFromPendingUpdates() = %#v, want only standard security packages", got)
	}
}

func TestKeptBackSecurityPackagesUseInstallSelectorForForeignArch(t *testing.T) {
	metadata := []servers.PendingUpdate{
		{
			Package:          "openssl",
			CurrentVersion:   "3.0.16-i386",
			CandidateVersion: "3.0.18-i386",
			Source:           "stable-security",
			Security:         true,
			Raw:              "openssl/stable-security 3.0.18-i386 i386 [upgradable from: 3.0.16-i386]",
			CVEs:             []string{},
		},
	}
	pending, _, plan := MergeAvailableUpdatesWithStandard(nil, nil, metadata, []string{metadata[0].Raw}, "", false)
	if len(pending) != 1 {
		t.Fatalf("len(pending)=%d, want 1", len(pending))
	}
	if pending[0].Package != "openssl" || pending[0].InstallPackage != "openssl:i386" || !pending[0].KeptBack || !pending[0].RequiresFull {
		t.Fatalf("pending[0] = %+v, want display package plus exact install selector", pending[0])
	}
	if got := KeptBackSecurityPackagesFromPendingUpdates(pending); !reflect.DeepEqual(got, []string{"openssl:i386"}) {
		t.Fatalf("KeptBackSecurityPackagesFromPendingUpdates() = %#v, want exact foreign arch selector", got)
	}
	if plan.KeptBackSecurityPackageCount != 1 {
		t.Fatalf("plan.KeptBackSecurityPackageCount = %d, want 1", plan.KeptBackSecurityPackageCount)
	}
}

func TestBuildSelectedInstallCmdAllowsNewDependencies(t *testing.T) {
	got := BuildSelectedInstallCmd([]string{"linux-image-amd64", "libssl3:amd64"})
	if !strings.Contains(got, "install -- 'linux-image-amd64' 'libssl3:amd64'") || !strings.Contains(got, RootHelperPath+" 'install' 'linux-image-amd64' 'libssl3:amd64'") {
		t.Fatalf("BuildSelectedInstallCmd() = %q, want direct root and typed helper forms", got)
	}
	if strings.Contains(got, "--only-upgrade") {
		t.Fatalf("BuildSelectedInstallCmd() = %q, should allow dependencies and new packages", got)
	}
}

func TestBuildSelectedInstallSimulationCmd(t *testing.T) {
	got := BuildSelectedInstallSimulationCmd([]string{"linux-image-amd64", "libssl3:amd64"})
	want := ReadOnlyAptCommand(`apt-get -o Debug::NoLocking=1 --print-uris --yes --download-only install -- 'linux-image-amd64' 'libssl3:amd64'`)
	if got != want {
		t.Fatalf("BuildSelectedInstallSimulationCmd() = %q, want %q", got, want)
	}
	if strings.Contains(got, "sudo") || strings.Contains(got, " -y ") {
		t.Fatalf("BuildSelectedInstallSimulationCmd() = %q, should be read-only simulation", got)
	}
	for _, required := range []string{"Debug::NoLocking=1", "--print-uris", "--download-only"} {
		if !strings.Contains(got, required) {
			t.Fatalf("BuildSelectedInstallSimulationCmd() = %q, want %s", got, required)
		}
	}
}

func TestFullUpgradePlanningCommandExposesSizesWithoutFetchingArchives(t *testing.T) {
	for _, required := range []string{"LC_ALL=C", "Debug::NoLocking=1", "--print-uris", "--yes", "--download-only", "full-upgrade"} {
		if !strings.Contains(AptFullUpgradeSimCmd, required) {
			t.Fatalf("AptFullUpgradeSimCmd = %q, want %s", AptFullUpgradeSimCmd, required)
		}
	}
	if strings.Contains(AptFullUpgradeSimCmd, "sudo") || strings.Contains(AptFullUpgradeSimCmd, " -y ") {
		t.Fatalf("AptFullUpgradeSimCmd = %q, should remain read-only", AptFullUpgradeSimCmd)
	}
}

func TestBuildUpgradePlanRecordsFullSimulationAvailability(t *testing.T) {
	pending := []servers.PendingUpdate{
		{Package: "openssl"},
		{Package: "linux-image-amd64", KeptBack: true, RequiresFull: true},
	}
	stdout := strings.Join([]string{
		"The following NEW packages will be installed:",
		"  linux-image-6.1.0-49-amd64",
		"The following packages will be upgraded:",
		"  openssl linux-image-amd64",
	}, "\n")
	available := BuildUpgradePlan(pending, stdout, true)
	if !available.FullUpgradePlanAvailable || available.FullUpgradePackageCount != 2 {
		t.Fatalf("available plan = %+v, want available full-upgrade plan", available)
	}
	wantRequiredKB := PlanDiskBaseReserveKB + 2*PlanDiskPerPackageKB + PlanDiskPerNewPackageKB
	if available.DiskSpaceRequiredKB != wantRequiredKB || available.DiskSpacePackageCount != 2 || available.DiskSpaceNewPackageCount != 1 {
		t.Fatalf("available plan disk estimate = %+v, want required=%d packages=2 new=1", available, wantRequiredKB)
	}
	unavailable := BuildUpgradePlan(pending, stdout, false)
	if unavailable.FullUpgradePlanAvailable {
		t.Fatalf("unavailable plan = %+v, should record failed full-upgrade simulation", unavailable)
	}
}

func TestBuildUpgradePlanUsesExactAptDiskFacts(t *testing.T) {
	stdout := strings.Join([]string{
		"The following packages will be upgraded:",
		"  openssl curl",
		"2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
		"Need to get 100 MB of archives.",
		"After this operation, 200 MB of additional disk space will be used.",
	}, "\n")
	plan := BuildUpgradePlan([]servers.PendingUpdate{{Package: "openssl"}, {Package: "curl"}}, stdout, true)

	if !plan.FullUpgradeDiskFactsAvailable || plan.DiskSpaceSource != PlanDiskSourceExact {
		t.Fatalf("plan = %+v, want exact APT disk facts", plan)
	}
	if plan.DiskSpacePackageCount != 2 || plan.DiskSpaceNewPackageCount != 0 {
		t.Fatalf("exact plan compatibility counts = %+v, want packages=2 new=0", plan)
	}
	if plan.DiskSpaceArchiveBytes != 100_000_000 || plan.DiskSpaceInstalledDeltaBytes != 200_000_000 || plan.DiskSpaceInstalledGrowthBytes != 200_000_000 {
		t.Fatalf("exact disk components = %+v", plan)
	}
	wantMargin := PlanDiskExactMarginMinBytes
	wantRequiredBytes := int64(300_000_000) + wantMargin + PlanDiskBaseReserveKB*1024
	wantRequiredKB := wantRequiredBytes / 1024
	if wantRequiredBytes%1024 != 0 {
		wantRequiredKB++
	}
	if plan.DiskSpaceSafetyMarginBytes != wantMargin || plan.DiskSpaceRequiredKB != wantRequiredKB {
		t.Fatalf("exact requirement = %+v, want margin=%d requiredKB=%d", plan, wantMargin, wantRequiredKB)
	}
}

func TestExactPlanDiskSpaceEstimateRejectsFreedSpaceAsPeakEvidence(t *testing.T) {
	if estimate, ok := exactPlanDiskSpaceEstimate(aptDiskFacts{ArchiveBytes: 25_000_000, InstalledDeltaBytes: -900_000_000}); ok {
		t.Fatalf("exactPlanDiskSpaceEstimate() = %+v, true; want conservative fallback", estimate)
	}
	plan := BuildUpgradePlan(
		[]servers.PendingUpdate{{Package: "replacement"}},
		"Need to get 25 MB of archives.\nAfter this operation, 900 MB disk space will be freed.\n",
		true,
	)
	if plan.DiskSpaceSource != PlanDiskSourceEstimate || plan.DiskSpaceRequiredKB != PlanDiskBaseReserveKB+PlanDiskPerPackageKB {
		t.Fatalf("freed-space plan = %+v, want package-based fallback", plan)
	}
}

func TestBuildUpgradePlanFallsBackWhenAptDiskFactsArePartialOrMalformed(t *testing.T) {
	for _, output := range []string{
		"The following packages will be upgraded:\n  openssl\nNeed to get 20 MB of archives.\n",
		"The following packages will be upgraded:\n  openssl\nNeed to get 1,14 MB of archives.\nAfter this operation, 10 MB of additional disk space will be used.\n",
	} {
		plan := BuildUpgradePlan([]servers.PendingUpdate{{Package: "openssl"}}, output, true)
		want := PlanDiskBaseReserveKB + PlanDiskPerPackageKB
		if plan.FullUpgradeDiskFactsAvailable || plan.DiskSpaceSource != PlanDiskSourceEstimate || plan.DiskSpaceRequiredKB != want {
			t.Fatalf("fallback plan = %+v, want source=estimate requiredKB=%d", plan, want)
		}
	}
}

func TestEstimatePlanDiskSpaceUsesLargerExactSimulationScope(t *testing.T) {
	plan := BuildUpgradePlan(nil, "Need to get 10 MB of archives.\nAfter this operation, 5 MB of additional disk space will be used.\n", true)
	ApplyKeptBackSecuritySimulation(&plan, "Need to get 40 MB of archives.\nAfter this operation, 200 MB of additional disk space will be used.\n")

	if plan.DiskSpaceSource != PlanDiskSourceExact || plan.DiskSpaceArchiveBytes != 40_000_000 || plan.DiskSpaceInstalledGrowthBytes != 200_000_000 {
		t.Fatalf("selected exact scope = %+v, want larger kept-back simulation", plan)
	}
}

func TestEstimatePlanDiskSpaceKeepsConservativeComponentsAcrossExactScopes(t *testing.T) {
	plan := BuildUpgradePlan(nil, "Need to get 500 MB of archives.\nAfter this operation, 0 B of additional disk space will be used.\n", true)
	ApplyKeptBackSecuritySimulation(&plan, "Need to get 0 B of archives.\nAfter this operation, 500 MB of additional disk space will be used.\n")

	if plan.DiskSpaceSource != PlanDiskSourceExact || plan.DiskSpaceArchiveBytes != 500_000_000 || plan.DiskSpaceInstalledGrowthBytes != 500_000_000 {
		t.Fatalf("combined exact components = %+v, want both per-filesystem maxima", plan)
	}
}

func TestEstimatePlanDiskSpaceFallsBackWhenKeptBackFactsAreIncomplete(t *testing.T) {
	plan := BuildUpgradePlan([]servers.PendingUpdate{{Package: "openssl"}}, "Need to get 10 MB of archives.\nAfter this operation, 5 MB of additional disk space will be used.\n", true)
	ApplyKeptBackSecuritySimulation(&plan, "Need to get 40 MB of archives.\n")

	if plan.DiskSpaceSource != PlanDiskSourceEstimate || plan.DiskSpaceRequiredKB != PlanDiskBaseReserveKB+PlanDiskPerPackageKB {
		t.Fatalf("plan = %+v, want conservative package estimate", plan)
	}
}

func TestExactPlanDiskSpaceEstimateRejectsOverflow(t *testing.T) {
	if estimate, ok := exactPlanDiskSpaceEstimate(aptDiskFacts{ArchiveBytes: math.MaxInt64, InstalledDeltaBytes: 1}); ok {
		t.Fatalf("exactPlanDiskSpaceEstimate() = %+v, true; want overflow rejected", estimate)
	}
}

func TestEstimatePlanDiskSpaceUsesMostConservativeKnownScope(t *testing.T) {
	plan := servers.UpgradePlan{
		StandardPackageCount:        3,
		KeptBackPackageCount:        2,
		FullUpgradePackageCount:     4,
		FullUpgradeNewPackages:      []string{"kernel-a"},
		KeptBackSecurityNewPackages: []string{"kernel-a", "kernel-b"},
	}
	estimate := EstimatePlanDiskSpace(plan)
	if estimate.PackageCount != 5 || estimate.NewPackageCount != 2 {
		t.Fatalf("EstimatePlanDiskSpace() = %+v, want packages=5 new=2", estimate)
	}
	if estimate.Source != PlanDiskSourceEstimate {
		t.Fatalf("legacy plan source = %q, want %q", estimate.Source, PlanDiskSourceEstimate)
	}
	want := PlanDiskBaseReserveKB + 5*PlanDiskPerPackageKB + 2*PlanDiskPerNewPackageKB
	if estimate.RequiredKB != want {
		t.Fatalf("required KB = %d, want %d", estimate.RequiredKB, want)
	}
}

func TestApplyKeptBackSecuritySimulationParsesExactImpact(t *testing.T) {
	plan := BuildUpgradePlan([]servers.PendingUpdate{
		{Package: "openssl", Security: true},
		{Package: "linux-image-amd64", Security: true, KeptBack: true, RequiresFull: true},
	}, "", false)
	stdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following NEW packages will be installed:",
		"  linux-image-6.1.0-49-amd64 linux-image-6.1.0-49-cloud-amd64",
		"The following packages will be removed:",
		"  obsolete-kernel",
		"The following packages will be upgraded:",
		"  linux-image-amd64",
	}, "\n")
	ApplyKeptBackSecuritySimulation(&plan, stdout)
	if !plan.KeptBackSecurityPlanAvailable || plan.KeptBackSecurityPackageCount != 1 {
		t.Fatalf("kept-back security plan = %+v, want available one-package plan", plan)
	}
	if !reflect.DeepEqual(plan.KeptBackSecurityNewPackages, []string{"linux-image-6.1.0-49-amd64", "linux-image-6.1.0-49-cloud-amd64"}) {
		t.Fatalf("kept-back new packages = %#v", plan.KeptBackSecurityNewPackages)
	}
	if !reflect.DeepEqual(plan.KeptBackSecurityRemovedPackages, []string{"obsolete-kernel"}) {
		t.Fatalf("kept-back removed packages = %#v", plan.KeptBackSecurityRemovedPackages)
	}
}

func TestAptMutationCommandsDeclareNonInteractivePolicy(t *testing.T) {
	commands := []string{
		AptUpdateCmd,
		AptUpgradeCmd,
		AptFullUpgradeCmd,
		AptAutoremoveCmd,
		BuildSelectedUpgradeCmd([]string{"openssl"}),
		BuildSelectedInstallCmd([]string{"linux-image-amd64"}),
	}
	for _, command := range commands {
		for _, required := range []string{
			"DEBIAN_FRONTEND=noninteractive",
			"DEBIAN_PRIORITY=critical",
			"APT_LISTCHANGES_FRONTEND=none",
			"NEEDRESTART_MODE=a",
			"UCF_FORCE_CONFFOLD=1",
			"Dpkg::Options::=--force-confdef",
			"Dpkg::Options::=--force-confold",
		} {
			if !strings.Contains(command, required) {
				t.Fatalf("command %q does not declare %q", command, required)
			}
		}
	}
	for _, required := range []string{"/usr/bin/dpkg --force-confdef --force-confold --configure -a", "/usr/bin/apt-get " + AptDpkgConffileOptions + " -y -f install", RootHelperPath + " 'repair'"} {
		if !strings.Contains(AptRepairCmd, required) {
			t.Fatalf("AptRepairCmd = %q, missing %q", AptRepairCmd, required)
		}
	}
}

func TestNonInteractiveSudoersSpecsRestrictEnvironmentWrapper(t *testing.T) {
	spec := NonInteractiveAptSudoersSpec()
	for _, forbidden := range []string{"/usr/bin/apt,", "/usr/bin/apt-get,", "/usr/bin/apt-get *", "/usr/bin/env "} {
		if strings.Contains(spec, forbidden) {
			t.Fatalf("NonInteractiveAptSudoersSpec() = %q, contains generic grant %q", spec, forbidden)
		}
	}
	for _, operation := range []string{"update", "upgrade", "full-upgrade", "autoremove", "repair", "lock-probe", "lock-probe-extended", "install", "install-only-upgrade", "reboot"} {
		if !strings.Contains(spec, RootHelperPath+" "+operation) {
			t.Fatalf("NonInteractiveAptSudoersSpec() = %q, missing typed operation %q", spec, operation)
		}
	}
}

func TestRootHelperRejectsEscapeInputsBeforeDispatch(t *testing.T) {
	helperPath := filepath.Join(t.TempDir(), "simplelinuxupdater-root-helper")
	if err := os.WriteFile(helperPath, []byte(RootHelperScript()), 0o700); err != nil {
		t.Fatalf("write helper: %v", err)
	}

	tests := []struct {
		name string
		args []string
	}{
		{name: "arbitrary apt option", args: []string{"update", "-o", "Debug::pkgProblemResolver=true"}},
		{name: "apt pre invoke hook", args: []string{"install", "APT::Update::Pre-Invoke::=/bin/sh", "openssl"}},
		{name: "shell metacharacters", args: []string{"install", "openssl;id"}},
		{name: "command substitution", args: []string{"install", "$(id)"}},
		{name: "invalid package option", args: []string{"install", "--reinstall"}},
		{name: "invalid architecture", args: []string{"install", "openssl:amd64;id"}},
		{name: "apt removal suffix", args: []string{"install", "curl-"}},
		{name: "architecture removal suffix", args: []string{"install", "curl:amd64-"}},
		{name: "apt regex fallback", args: []string{"install", "a.+"}},
		{name: "arbitrary path", args: []string{"install", "/tmp/package.deb"}},
		{name: "unknown operation", args: []string{"shell", "id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(helperPath, tt.args...)
			output, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("helper accepted %q; output=%q", tt.args, output)
			}
			if !strings.Contains(string(output), "refused") {
				t.Fatalf("helper rejection for %q = %q, want refused diagnostic", tt.args, output)
			}
		})
	}
}

func TestPackageSelectorValidation(t *testing.T) {
	for _, selector := range []string{"openssl", "libssl3:amd64", "linux-image-6.12.0-1-amd64", "libstdc++6"} {
		if !IsValidPackageSelector(selector) {
			t.Errorf("IsValidPackageSelector(%q) = false, want true", selector)
		}
	}
	for _, selector := range []string{"", "-o", "openssl;id", "$(id)", "pkg:amd64:extra", "pkg:../amd64", "/tmp/pkg.deb", "pkg=1.2", "curl-", "curl:amd64-", "a.+"} {
		if IsValidPackageSelector(selector) {
			t.Errorf("IsValidPackageSelector(%q) = true, want false", selector)
		}
	}
}

func TestHostCommandEffectOwnsPackageManagerTimeoutSemantics(t *testing.T) {
	tests := []struct {
		effect              HostCommandEffect
		usesLocks           bool
		needsReconciliation bool
	}{
		{effect: HostCommandEffectReadOnly},
		{effect: HostCommandEffectMetadataMutation, usesLocks: true},
		{effect: HostCommandEffectPackageStateMutation, usesLocks: true, needsReconciliation: true},
		{effect: HostCommandEffectSystemStateMutation},
	}
	for _, tt := range tests {
		t.Run(string(tt.effect), func(t *testing.T) {
			if !tt.effect.Valid() {
				t.Fatal("declared host command effect must be valid")
			}
			if got := tt.effect.UsesPackageManagerLocks(); got != tt.usesLocks {
				t.Fatalf("UsesPackageManagerLocks() = %t, want %t", got, tt.usesLocks)
			}
			if got := tt.effect.RequiresReconciliationOnUnknownOutcome(); got != tt.needsReconciliation {
				t.Fatalf("RequiresReconciliationOnUnknownOutcome() = %t, want %t", got, tt.needsReconciliation)
			}
		})
	}
	if HostCommandEffect("").Valid() || HostCommandEffect("unknown").Valid() {
		t.Fatal("missing and unknown host command effects must fail validation")
	}
}

func TestAptRepairLockGuardFailsClosedOnProbeErrors(t *testing.T) {
	tests := []struct {
		name        string
		probe       string
		wantProceed bool
	}{
		{name: "no lock holder", probe: "sh -c 'exit 1'", wantProceed: true},
		{name: "active lock holder", probe: "sh -c 'printf 1234; exit 0'"},
		{name: "probe diagnostic", probe: "sh -c 'printf permission-denied >&2; exit 1'"},
		{name: "unexpected probe exit", probe: "sh -c 'exit 2'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := buildAptRepairLockGuard(tt.probe) + "; printf repair-started"
			output, err := exec.Command("sh", "-c", command).CombinedOutput()
			proceeded := strings.Contains(string(output), "repair-started")
			if proceeded != tt.wantProceed {
				t.Fatalf("repair proceeded = %v, want %v; output = %q; error = %v", proceeded, tt.wantProceed, output, err)
			}
			if tt.wantProceed && err != nil {
				t.Fatalf("lock guard error = %v, want success; output = %q", err, output)
			}
			if !tt.wantProceed && err == nil {
				t.Fatalf("lock guard succeeded, want blocking error; output = %q", output)
			}
		})
	}
}

func TestAptRepairAuditGuardRequiresEmptySuccessfulAudit(t *testing.T) {
	tests := []struct {
		name        string
		audit       string
		wantProceed bool
	}{
		{name: "clean audit", audit: "sh -c 'exit 0'", wantProceed: true},
		{name: "reported package problem", audit: "sh -c 'printf partially-installed-package; exit 0'"},
		{name: "audit command failure", audit: "sh -c 'printf audit-failed >&2; exit 2'"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			command := buildAptRepairAuditGuard(tt.audit) + " && printf repair-complete"
			output, err := exec.Command("sh", "-c", command).CombinedOutput()
			proceeded := strings.Contains(string(output), "repair-complete")
			if proceeded != tt.wantProceed {
				t.Fatalf("repair proceeded = %v, want %v; output = %q; error = %v", proceeded, tt.wantProceed, output, err)
			}
			if tt.wantProceed && err != nil {
				t.Fatalf("audit guard error = %v, want success; output = %q", err, output)
			}
			if !tt.wantProceed && err == nil {
				t.Fatalf("audit guard succeeded, want blocking error; output = %q", output)
			}
		})
	}
}

func TestRetryHelpersClassifyRetryableOutput(t *testing.T) {
	err := MarkRetryableFromOutput(errors.New("exit status 100"), "Could not get lock /var/lib/dpkg/lock-frontend")
	if !IsRetryableError(err) {
		t.Fatalf("MarkRetryableFromOutput() did not tag retryable lock output")
	}
	delay := ComputeRetryDelay(RetryPolicy{BaseDelay: time.Second, MaxDelay: 8 * time.Second, JitterPct: 0}, 3, 0)
	if delay != 4*time.Second {
		t.Fatalf("ComputeRetryDelay() = %s, want 4s", delay)
	}
}

func TestRetryHelpersHonorExplicitNonRetryableClassification(t *testing.T) {
	err := NonRetryableTaggedError{Err: errors.New("command timed out after 5m0s")}
	if IsRetryableError(err) {
		t.Fatalf("IsRetryableError(%v) = true, want explicit non-retryable classification", err)
	}
	marked := MarkRetryableFromOutput(err, "E: Could not get lock /var/lib/dpkg/lock-frontend")
	if IsRetryableError(marked) {
		t.Fatalf("MarkRetryableFromOutput(%v) became retryable, want replay-disabled classification to win", marked)
	}
}

func TestRetryHelpersNeverReplayContextTermination(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if IsRetryableError(err) {
			t.Fatalf("IsRetryableError(%v) = true, want false", err)
		}
	}
}

func TestPreparePendingUpdatesForCVEScansEveryNamedPackageAndSorts(t *testing.T) {
	const packageCount = 75
	updates := make([]servers.PendingUpdate, 0, packageCount)
	for i := 0; i < packageCount; i++ {
		updates = append(updates, servers.PendingUpdate{Package: strings.Repeat("a", i+1), Security: i%2 == 0})
	}
	prepared := PreparePendingUpdatesForCVE(updates)
	if prepared[0].CVEState != "pending" {
		t.Fatalf("first CVE state = %q, want pending", prepared[0].CVEState)
	}
	if prepared[len(prepared)-1].CVEState != "pending" {
		t.Fatalf("last CVE state = %q, want pending", prepared[len(prepared)-1].CVEState)
	}
}
