package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readWorkflowForTest(t *testing.T, path string) string {
	t.Helper()
	source, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(source)
}

func workflowJobForTest(t *testing.T, source, name, nextName string) string {
	t.Helper()
	startMarker := "\n  " + name + ":"
	start := strings.Index(source, startMarker)
	if start < 0 {
		t.Fatalf("workflow job %q not found", name)
	}
	end := len(source)
	if nextName != "" {
		endMarker := "\n  " + nextName + ":"
		if relativeEnd := strings.Index(source[start+len(startMarker):], endMarker); relativeEnd >= 0 {
			end = start + len(startMarker) + relativeEnd
		}
	}
	return source[start:end]
}

func TestReleaseGateRejectsTagsOutsideMainHistory(t *testing.T) {
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	releaseGate := workflowJobForTest(t, release, "release-gate", "publish-release")

	for _, required := range []string{
		"fetch-depth: 0",
		"run: tools/release/verify-tag-on-main.sh",
	} {
		if !strings.Contains(releaseGate, required) {
			t.Errorf("release-gate does not enforce main-history provenance %q", required)
		}
	}
	if !strings.Contains(release, "needs: release-gate") {
		t.Error("GitHub release publication is not gated by release-gate")
	}
	if !strings.Contains(release, "needs: [release-gate, publish-release]") {
		t.Error("container publication is not gated by release-gate and GitHub release publication")
	}
}

func TestReleaseGateMatchesCriticalCIValidation(t *testing.T) {
	ci := readWorkflowForTest(t, ".github/workflows/ci.yml")
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	releaseGate := workflowJobForTest(t, release, "release-gate", "publish-release")

	criticalCommands := []string{
		"go vet ./...",
		"staticcheck ./...",
		"govulncheck ./...",
		"actionlint",
		"go test -race -count=1 ./...",
		"go test -count=1 -covermode=atomic -coverprofile=coverage.out ./...",
		"go build -o webserver .",
		"npm ci",
		"npm audit --audit-level=moderate",
		"npm run test:unit",
		"npm run test:e2e",
	}
	for _, command := range criticalCommands {
		if !strings.Contains(ci, command) {
			t.Errorf("normal CI lost critical validation %q", command)
		}
		if !strings.Contains(releaseGate, command) {
			t.Errorf("release-gate lost critical CI validation %q", command)
		}
	}
}

func TestReleaseWorkflowPinsActionsByCommit(t *testing.T) {
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	usesPattern := regexp.MustCompile(`(?m)^\s*uses:\s*([^\s#]+)(?:\s+#\s*(\S+))?\s*$`)
	shaPattern := regexp.MustCompile(`^[0-9a-f]{40}$`)
	matches := usesPattern.FindAllStringSubmatch(release, -1)
	if len(matches) == 0 {
		t.Fatal("release workflow contains no actions")
	}
	for _, match := range matches {
		action, ref, found := strings.Cut(match[1], "@")
		if !found || !shaPattern.MatchString(ref) {
			t.Errorf("release action %q is not pinned to a full commit SHA", match[1])
			continue
		}
		if len(match) < 3 || !strings.HasPrefix(match[2], "v") {
			t.Errorf("release action %s@%s lacks a readable version comment", action, ref)
		}
	}
}
