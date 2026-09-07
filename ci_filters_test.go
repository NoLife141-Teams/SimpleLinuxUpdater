package main

import (
	"regexp"
	"strings"
	"testing"
)

func TestCIPathFiltersCoverValidationInputs(t *testing.T) {
	source := readWorkflowForTest(t, ".github/workflows/ci.yml")
	filters := map[string][]string{}
	current := ""
	for _, line := range strings.Split(source, "\n") {
		if match := regexp.MustCompile(`^            (go|e2e|docker):$`).FindStringSubmatch(line); match != nil {
			current = match[1]
			continue
		}
		if strings.HasPrefix(line, "              - '") {
			filters[current] = append(filters[current], strings.TrimSuffix(strings.TrimPrefix(line, "              - '"), "'"))
		} else {
			current = ""
		}
	}
	matches := func(pattern, path string) bool {
		// These positive globs use only *, ** and **/, the subset used by the workflow.
		expr := regexp.QuoteMeta(pattern)
		expr = strings.ReplaceAll(expr, `\*\*/`, "@@OPTIONALDIR@@")
		expr = strings.ReplaceAll(expr, `\*\*`, "@@TREE@@")
		expr = strings.ReplaceAll(expr, `\*`, "[^/]*")
		expr = strings.ReplaceAll(expr, "@@OPTIONALDIR@@", "(?:.*/)?")
		expr = strings.ReplaceAll(expr, "@@TREE@@", ".*")
		return regexp.MustCompile("^" + expr + "$").MatchString(path)
	}
	for _, tt := range []struct {
		path                string
		goTest, e2e, docker bool
	}{
		{"tools/release/verify-tag-on-main.sh", true, false, false},
		{"tools/release/publication-policy.py", true, false, false},
		{"tools/ci/docker-smoke.sh", true, true, true},
		{"docker-entrypoint.sh", true, false, true},
		{"Dockerfile", true, false, true},
		{".dockerignore", false, false, true},
		{"webserver.go", true, true, true},
		{"internal/auth/service.go", true, true, true},
		{"templates/index.html", true, true, true},
		{"static/js/index.js", true, true, true},
		{"playwright.config.js", true, true, false},
		{".github/workflows/release.yml", true, true, true},
		{"docs/installation.md", true, false, false},
		{"package-lock.json", true, true, false},
		{"CHANGELOG.md", false, false, false},
	} {
		t.Run(tt.path, func(t *testing.T) {
			for filter, want := range map[string]bool{"go": tt.goTest, "e2e": tt.e2e, "docker": tt.docker} {
				got := false
				for _, pattern := range filters[filter] {
					got = got || matches(pattern, tt.path)
				}
				if got != want {
					t.Errorf("%s selects %s=%v want %v", filter, tt.path, got, want)
				}
			}
		})
	}
}
