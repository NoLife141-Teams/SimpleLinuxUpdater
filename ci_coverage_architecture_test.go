package main

import (
	"strings"
	"testing"
)

func TestCICoverageRegressionGuard(t *testing.T) {
	ci := readWorkflowForTest(t, ".github/workflows/ci.yml")
	release := readWorkflowForTest(t, ".github/workflows/release.yml")
	testJob := workflowJobForTest(t, ci, "test", "quality")
	requiredJob := workflowJobForTest(t, ci, "ci-required", "")
	releaseGate := workflowJobForTest(t, release, "release-gate", "publish-release")

	for _, required := range []string{
		"GO_COVERAGE_THRESHOLD: '73.0'",
		"Measured Go coverage:",
		"required minimum:",
		"go tool cover -func=coverage.out",
		"exit 1",
	} {
		if !strings.Contains(testJob, required) {
			t.Errorf("CI test job is missing coverage guard contract %q", required)
		}
		if !strings.Contains(releaseGate, required) {
			t.Errorf("release gate is missing coverage guard contract %q", required)
		}
	}
	if !strings.Contains(requiredJob, "- test") || !strings.Contains(requiredJob, `test "$TEST_RESULT" = success`) {
		t.Fatal("ci-required must fail when the coverage matrix job fails")
	}
}
