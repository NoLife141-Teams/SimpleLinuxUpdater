package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPageStylesDoNotRedefineSharedThemeTokens(t *testing.T) {
	for _, path := range []string{
		"static/css/index.css",
		"static/css/manage.css",
		"static/css/admin.css",
		"static/css/observability.css",
	} {
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(string(contents), ":root") {
			t.Fatalf("%s redefines shared theme tokens; base.css is the canonical owner", path)
		}
	}
}

func TestStatusPageDoesNotRedefineSharedComponents(t *testing.T) {
	contents, err := os.ReadFile("static/css/index.css")
	if err != nil {
		t.Fatal(err)
	}
	sharedSelectors := []string{
		".app-header", ".brand-lockup", ".brand-logo", ".app-nav", ".dashboard-main",
		".dashboard-head", ".head-meta", ".metric-strip", ".metric-item", ".control-strip",
		".table-shell", ".table-wrap", "table", "th", ".modal",
	}
	for _, selector := range sharedSelectors {
		pattern := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(selector) + `\s*\{`)
		if pattern.Match(contents) {
			t.Errorf("index.css redefines shared component %s; scope only page-specific composition there", selector)
		}
	}
}
