package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCIRequiredRejectsUnexpectedJobResults(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("CI helper scripts target the Linux GitHub Actions runner")
	}
	workflow := readWorkflowForTest(t, ".github/workflows/ci.yml")
	job := workflowJobForTest(t, workflow, "ci-required", "")
	_, body, ok := strings.Cut(job, "        run: |\n")
	if !ok {
		t.Fatal("missing required gate script")
	}
	var script strings.Builder
	for _, line := range strings.Split(body, "\n") {
		script.WriteString(strings.TrimPrefix(line, "          ") + "\n")
	}
	base := map[string]string{"CHANGES_RESULT": "success", "FRONTEND_QUALITY_RESULT": "success", "GO_REQUIRED": "true", "E2E_REQUIRED": "true", "DOCKER_REQUIRED": "true", "TEST_RESULT": "success", "QUALITY_RESULT": "success", "UI_E2E_RESULT": "success", "DOCKER_RESULT": "success"}
	run := func(t *testing.T, env map[string]string, wantSuccess bool) {
		t.Helper()
		cmd := exec.Command("bash", "-e", "-c", script.String())
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		output, err := cmd.CombinedOutput()
		if (err == nil) != wantSuccess {
			t.Fatalf("gate success=%v want %v: %s", err == nil, wantSuccess, output)
		}
	}
	clone := func() map[string]string {
		result := map[string]string{}
		for k, v := range base {
			result[k] = v
		}
		return result
	}
	t.Run("all required successful", func(t *testing.T) { run(t, base, true) })
	for _, key := range []string{"CHANGES_RESULT", "FRONTEND_QUALITY_RESULT", "TEST_RESULT", "QUALITY_RESULT", "UI_E2E_RESULT", "DOCKER_RESULT"} {
		for _, result := range []string{"failure", "cancelled", "skipped", ""} {
			t.Run(key+"/"+result, func(t *testing.T) { env := clone(); env[key] = result; run(t, env, false) })
		}
	}
	for _, group := range []struct {
		required string
		results  []string
	}{{"GO_REQUIRED", []string{"TEST_RESULT", "QUALITY_RESULT"}}, {"E2E_REQUIRED", []string{"UI_E2E_RESULT"}}, {"DOCKER_REQUIRED", []string{"DOCKER_RESULT"}}} {
		t.Run(group.required+"/skipped", func(t *testing.T) {
			env := clone()
			env[group.required] = "false"
			for _, k := range group.results {
				env[k] = "skipped"
			}
			run(t, env, true)
			for _, k := range group.results {
				env[k] = "success"
				run(t, env, false)
				env[k] = "skipped"
			}
		})
	}
}

func TestReleasePublicationPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("release publication targets the Linux GitHub Actions runner")
	}
	cmd := exec.Command("python3", "-m", "unittest", "discover", "-s", "tools/release", "-p", "test_*.py", "-v")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("publication policy regression: %v\n%s", err, output)
	}
}

func TestCoverageScriptRejectsMalformedAndLowCoverage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("coverage helper targets the Linux GitHub Actions runner")
	}
	root := t.TempDir()
	mock := filepath.Join(root, "go")
	if err := os.WriteFile(mock, []byte("#!/bin/sh\nprintf 'total: (statements) %s\\n' \"$TEST_COVERAGE\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		value   string
		success bool
	}{{"73.0%", true}, {"100.0%", true}, {"72.9%", false}, {"", false}, {"...", false}, {"invalid", false}} {
		t.Run(tt.value, func(t *testing.T) {
			cmd := exec.Command("bash", "tools/ci/check-coverage.sh")
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "TEST_COVERAGE="+tt.value, "GO_COVERAGE_THRESHOLD=73.0")
			output, err := cmd.CombinedOutput()
			if (err == nil) != tt.success {
				t.Fatalf("success=%v want %v: %s", err == nil, tt.success, output)
			}
		})
	}
}
