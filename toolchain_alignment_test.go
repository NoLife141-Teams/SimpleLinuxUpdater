package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

func readToolchainFile(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(content)
}

func canonicalGoVersion(t *testing.T) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^go\s+([0-9]+\.[0-9]+\.[0-9]+)\s*$`).FindStringSubmatch(readToolchainFile(t, "go.mod"))
	if len(match) != 2 {
		t.Fatal("go.mod must declare one exact patch-level Go version")
	}
	return match[1]
}

func TestDockerBuilderMatchesCanonicalGoVersion(t *testing.T) {
	canonical := canonicalGoVersion(t)
	dockerfile := readToolchainFile(t, "Dockerfile")
	match := regexp.MustCompile(`(?m)^FROM\s+golang:([0-9]+\.[0-9]+\.[0-9]+)-alpine\s+AS\s+builder\s*$`).FindStringSubmatch(dockerfile)
	if len(match) != 2 {
		t.Fatal("Dockerfile must use one exact golang patch-level alpine builder tag")
	}
	if match[1] != canonical {
		t.Fatalf("Docker builder Go version = %s, want go.mod version %s", match[1], canonical)
	}
}

func TestGoToolchainValidationIsRequiredInCIAndRelease(t *testing.T) {
	ci := readToolchainFile(t, ".github/workflows/ci.yml")
	release := readToolchainFile(t, ".github/workflows/release.yml")

	for _, required := range []string{
		"toolchain-alignment:",
		"run: tools/ci/verify-go-toolchain.sh",
		"TOOLCHAIN_ALIGNMENT_RESULT: ${{ needs.toolchain-alignment.result }}",
		`test "$TOOLCHAIN_ALIGNMENT_RESULT" = success`,
	} {
		if !strings.Contains(ci, required) {
			t.Errorf("CI does not require Go toolchain alignment contract %q", required)
		}
	}
	if !strings.Contains(release, "tools/ci/verify-go-toolchain.sh") {
		t.Error("release gate does not run the Go toolchain alignment contract")
	}

	for path, workflow := range map[string]string{
		".github/workflows/ci.yml":      ci,
		".github/workflows/release.yml": release,
	} {
		setupCount := strings.Count(workflow, "uses: actions/setup-go@")
		moduleCount := strings.Count(workflow, "go-version-file: 'go.mod'")
		if setupCount == 0 || moduleCount != setupCount {
			t.Errorf("%s setup-go steps using go.mod = %d, want %d", path, moduleCount, setupCount)
		}
	}
}

func TestDependabotCannotUpdateDockerGoBuilderIndependently(t *testing.T) {
	dependabot := readToolchainFile(t, ".github/dependabot.yml")
	dockerStart := strings.Index(dependabot, `package-ecosystem: "docker"`)
	npmStart := strings.Index(dependabot, `package-ecosystem: "npm"`)
	if dockerStart < 0 || npmStart <= dockerStart {
		t.Fatal("Dependabot Docker configuration block not found")
	}
	dockerConfig := dependabot[dockerStart:npmStart]
	for _, required := range []string{
		"ignore:",
		`dependency-name: "golang"`,
	} {
		if !strings.Contains(dockerConfig, required) {
			t.Errorf("Dependabot Docker config does not prevent independent Go builder updates: missing %q", required)
		}
	}
}

func TestCanonicalGoVersionIsDocumented(t *testing.T) {
	canonical := canonicalGoVersion(t)
	for path, required := range map[string]string{
		"README.md":            "Go-" + canonical + "-",
		"AGENTS.md":            "currently " + canonical,
		"docs/installation.md": "currently `" + canonical + "`",
	} {
		if !strings.Contains(readToolchainFile(t, path), required) {
			t.Errorf("%s does not document canonical Go version %s", path, canonical)
		}
	}
}
