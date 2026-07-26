package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

func TestFrontendInteractionContracts(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	cmd := exec.Command(node, "--test", "static/js/dashboard-interaction.test.cjs")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("frontend interaction contracts failed: %v\n%s", err, output)
	}
}

func TestPendingUpdateCVEsUseConfirmedExpandableGroups(t *testing.T) {
	contents, err := os.ReadFile("static/js/index.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, contract := range []string{
		"cve_findings",
		"fixed_by_candidate",
		"still_affected",
		"Fixed by the available update",
		"Still affected after the available update",
		"advisory_url",
		"Coverage unknown",
		"official_installed_unverified",
		"Installed provenance unverified",
		"expandedCVEFindings",
		"data-cve-disclosure-key",
		"details.cve-findings",
	} {
		if !strings.Contains(source, contract) {
			t.Errorf("pending-update CVE presentation contract %q is missing", contract)
		}
	}
	if strings.Contains(source, "cves.slice(0, 3)") {
		t.Error("pending-update CVE presentation still hides confirmed findings after the first three")
	}
}

func TestOperatorWorkflowsDoNotUseNativeBrowserDialogs(t *testing.T) {
	nativeDialog := regexp.MustCompile(`(?:^|[^A-Za-z0-9_.])(?:window\.)?(?:alert|confirm)\s*\(`)
	for _, path := range []string{"static/js/index.js", "static/js/index-bulk-actions.js", "static/js/manage.js", "static/js/admin.js"} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if nativeDialog.Match(contents) {
			t.Errorf("%s still uses a blocking native browser dialog", path)
		}
	}
	common, err := os.ReadFile("static/js/common.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range []string{"aria-live", "aria-modal", "previousFocus", `event.key === "Tab"`, `event.key === "Escape"`} {
		if !regexp.MustCompile(regexp.QuoteMeta(contract)).Match(common) {
			t.Errorf("accessible application interaction contract %q is missing", contract)
		}
	}
}
