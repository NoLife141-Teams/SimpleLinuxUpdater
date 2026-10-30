package main

import (
	"os"
	"os/exec"
	"regexp"
	"strconv"
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

func TestPendingUpdateCVEsUseReportedExpandableGroups(t *testing.T) {
	contents, err := os.ReadFile("static/js/index.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(contents)
	for _, contract := range []string{
		"cve_findings",
		"fixed_by_candidate",
		"still_affected",
		"Absent from the candidate source assessment",
		"Still reported in the candidate source assessment",
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

func TestStatusPageModalsRenderAboveDrawer(t *testing.T) {
	statusCSS, err := os.ReadFile("static/css/index.css")
	if err != nil {
		t.Fatal(err)
	}
	sharedCSS, err := os.ReadFile("static/css/base.css")
	if err != nil {
		t.Fatal(err)
	}
	// Compare each backdrop's declared layer rather than requiring the old,
	// unscoped modal selector. Browser tests cover the composed stacking order.
	rules := regexp.MustCompile(`(?s)([^{}]+)\{([^{}]*)\}`)
	zIndex := regexp.MustCompile(`(?:^|;)\s*z-index:\s*(-?\d+)\s*;`)
	layer := func(source []byte, selector string) int {
		t.Helper()
		value, found := 0, false
		for _, rule := range rules.FindAllStringSubmatch(string(source), -1) {
			for _, candidate := range strings.Split(rule[1], ",") {
				if strings.TrimSpace(candidate) != selector {
					continue
				}
				if match := zIndex.FindStringSubmatch(rule[2]); match != nil {
					parsed, parseErr := strconv.Atoi(match[1])
					if parseErr != nil {
						t.Fatal(parseErr)
					}
					value = parsed
					found = true
				}
			}
		}
		if !found {
			t.Fatalf("%s has no declared integer layer", selector)
		}
		return value
	}
	drawer := layer(statusCSS, ".side-drawer")
	if drawer != 90 {
		t.Fatal("status drawer must retain its documented layer")
	}
	modal := layer(statusCSS, ".modal-backdrop:not(.action-confirm-backdrop):not(.typed-confirm-backdrop)")
	if modal <= drawer {
		t.Fatal("status-page modal backdrops must render above the drawer")
	}
	for _, selector := range []string{".action-confirm-backdrop", ".typed-confirm-backdrop"} {
		if layer(sharedCSS, selector) <= modal {
			t.Errorf("%s must render above both the status modal and drawer", selector)
		}
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
