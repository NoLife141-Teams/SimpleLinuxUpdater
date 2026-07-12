package main

import (
	"os"
	"os/exec"
	"regexp"
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
