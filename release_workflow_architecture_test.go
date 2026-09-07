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
	trigger := readWorkflowForTest(t, ".github/workflows/release-trigger.yml")

	for _, required := range []string{
		`workflow_run:`,
		`workflows: ["Release Tag Signal"]`,
		`RELEASE_TAG: ${{ github.event.workflow_run.head_branch }}`,
		`RELEASE_SHA: ${{ github.event.workflow_run.head_sha }}`,
		"fetch-depth: 0",
		"run: tools/release/verify-tag-on-main.sh",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("trusted release workflow does not enforce main-history provenance %q", required)
		}
	}
	if got := strings.Count(release, "run: tools/release/verify-tag-on-main.sh"); got != 3 {
		t.Errorf("each release job must independently verify provenance, got %d gates", got)
	}
	if strings.Contains(release, "ref: ${{ env.RELEASE_SHA }}") {
		t.Error("privileged workflow must not pass workflow_run input directly to actions/checkout")
	}
	if strings.Contains(release, "on:\n  push:") || strings.Contains(release, `tags: ["v*"]`) {
		t.Error("publication workflow must not be loaded from the untrusted tagged ref")
	}
	for _, required := range []string{
		"name: Release Tag Signal",
		"push:",
		`tags: ["v*"]`,
	} {
		if !strings.Contains(trigger, required) {
			t.Errorf("tag signal workflow is missing %q", required)
		}
	}
	if strings.Contains(trigger, "uses:") || strings.Contains(trigger, "contents: write") {
		t.Error("untrusted tag signal workflow must not check out code or receive write permissions")
	}
	if !strings.Contains(release, "needs: release-gate") {
		t.Error("GitHub release publication is not gated by release-gate")
	}
	if !strings.Contains(release, "needs: [release-gate, publish-release]") {
		t.Error("container publication is not gated by release-gate and GitHub release publication")
	}
	for _, required := range []string{
		"tag_name: ${{ env.RELEASE_TAG }}",
		"target_commitish: ${{ env.RELEASE_SHA }}",
		"type=raw,value=${{ env.RELEASE_TAG }}",
	} {
		if !strings.Contains(release, required) {
			t.Errorf("publication does not use the verified release identity %q", required)
		}
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

func TestReleasePublicationIsCoordinated(t *testing.T) {
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	for _, required := range []string{"group: release-publication", "cancel-in-progress: false", "draft: true", "make_latest: false", "verify-archives.sh", "publication-policy.py\" draft", "publication-policy.py\" finalize", "linux/amd64", "linux/arm64"} {
		if !strings.Contains(release, required) {
			t.Errorf("missing coordinated publication contract %q", required)
		}
	}
	if strings.Contains(release, "type=raw,value=latest") {
		t.Error("build must not overwrite latest before runtime verification and version policy")
	}
	for _, job := range []struct{ name, next string }{{"release-gate", "publish-release"}, {"publish-release", "publish-docker"}, {"publish-docker", ""}} {
		source := workflowJobForTest(t, release, job.name, job.next)
		verify := strings.Index(source, "run: tools/release/verify-tag-on-main.sh")
		preserve := strings.Index(source, `cp -R tools "$RUNNER_TEMP/trusted-tools"`)
		checkout := strings.Index(source, `git checkout --detach "$RELEASE_SHA"`)
		if verify < 0 || preserve < verify || checkout < preserve {
			t.Errorf("%s must verify provenance and preserve trusted helpers before executing tagged code", job.name)
		}
	}
	docker := workflowJobForTest(t, release, "publish-docker", "")
	if strings.Index(docker, "Verify published version on both architectures") > strings.Index(docker, "Finalize coordinated publication") {
		t.Error("finalization must follow both runtime smoke tests")
	}
	ci := readWorkflowForTest(t, ".github/workflows/ci.yml")
	smoke := workflowJobForTest(t, ci, "docker-smoke", "ci-required")
	if strings.Contains(smoke, "packages: write") || strings.Contains(smoke, "login-action") || !strings.Contains(smoke, "tools/ci/docker-smoke.sh") {
		t.Error("PR Docker validation must build and test without registry credentials")
	}
	for _, workflow := range []string{ci, release} {
		for _, required := range []string{`PLAYWRIGHT_BROWSERS_PATH=$RUNNER_TEMP/ms-playwright`, "path: ${{ env.PLAYWRIGHT_BROWSERS_PATH }}", "retention-days: 7", "playwright-report/", "test-results/"} {
			if !strings.Contains(workflow, required) {
				t.Errorf("missing browser cache or diagnostic contract %q", required)
			}
		}
	}
}

func TestReleaseQEMUHelperCannotFollowMutableImageTag(t *testing.T) {
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	_, setup, found := strings.Cut(release, "      - name: Set up QEMU for runtime verification\n")
	if !found {
		t.Fatal("missing release QEMU setup")
	}
	setup, _, _ = strings.Cut(setup, "\n      - name:")
	if !regexp.MustCompile(`(?m)^\s+image: docker\.io/tonistiigi/binfmt:[^\s@]+@sha256:[0-9a-f]{64}$`).MatchString(setup) {
		t.Error("privileged QEMU helper must be pinned by digest as well as the action SHA")
	}
	for _, required := range []string{"platforms: arm64", "cache-image: false"} {
		if !strings.Contains(setup, required) {
			t.Errorf("QEMU setup missing %q", required)
		}
	}
}
