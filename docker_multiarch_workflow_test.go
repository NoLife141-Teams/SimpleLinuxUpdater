package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

type dockerWorkflowStep struct {
	Name string            `yaml:"name"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
	With map[string]string `yaml:"with"`
}

func dockerWorkflowSteps(t *testing.T, path, job string) []dockerWorkflowStep {
	t.Helper()
	var workflow struct {
		Jobs map[string]struct {
			Steps []dockerWorkflowStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(readWorkflowForTest(t, path)), &workflow); err != nil {
		t.Fatal(err)
	}
	steps := workflow.Jobs[job].Steps
	if len(steps) == 0 {
		t.Fatalf("no steps in %s/%s", path, job)
	}
	return steps
}

func TestMultiarchWorkflowsEnableContainerdBeforeDockerUse(t *testing.T) {
	for _, tt := range []struct{ path, job string }{
		{".github/workflows/ci.yml", "docker-smoke"},
		{".github/workflows/release.yml", "publish-docker"},
	} {
		t.Run(tt.job, func(t *testing.T) {
			ready := false
			for _, step := range dockerWorkflowSteps(t, tt.path, tt.job) {
				if strings.HasPrefix(step.Uses, "docker/setup-docker-action@") {
					var config struct {
						Features map[string]bool `json:"features"`
					}
					if err := json.Unmarshal([]byte(step.With["daemon-config"]), &config); err != nil {
						t.Fatalf("invalid Docker daemon configuration: %v", err)
					}
					ready = config.Features["containerd-snapshotter"]
					if !ready {
						t.Fatal("multiarchitecture qualification needs the containerd image store")
					}
					continue
				}
				if strings.HasPrefix(step.Uses, "docker/") || strings.Contains(step.Run, "docker build") {
					if !ready {
						t.Fatalf("%s uses Docker before configuring the multiarchitecture image store", step.Name)
					}
				}
			}
			if !ready {
				t.Fatal("containerd image store setup missing")
			}
		})
	}
}

func TestCIDockerSmokeChecksBothArchitecturesOfOneImmutableImage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker CI scripts target Linux")
	}
	var build, smoke string
	for _, step := range dockerWorkflowSteps(t, ".github/workflows/ci.yml", "docker-smoke") {
		if step.Name == "Build container without publishing" {
			build = step.Run
		}
		if step.Name == "Verify startup, privileges and persistence" {
			smoke = step.Run
		}
	}
	for _, required := range []string{"docker buildx build", "--platform linux/amd64,linux/arm64", "--load"} {
		if !strings.Contains(build, required) {
			t.Errorf("CI must load both architectures without publishing: missing %q", required)
		}
	}
	if strings.Contains(build, "--push") || smoke == "" {
		t.Fatal("CI must test a local image without a registry push")
	}
	const digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	for _, failArm64 := range []string{"false", "true"} {
		t.Run("arm64_failure="+failArm64, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "tools/ci"), 0755); err != nil {
				t.Fatal(err)
			}
			adapters := map[string]string{
				"docker":                   "#!/bin/sh\n[ \"$1 $2\" = 'image inspect' ] || exit 99\nprintf '%s\\n' '" + digest + "'\n",
				"tools/ci/docker-smoke.sh": "#!/bin/sh\nprintf '%s %s\\n' \"$1\" \"$2\" >> calls\n[ \"$FAIL_ARM64\" != true ] || [ \"$2\" != linux/arm64 ]\n",
			}
			for path, source := range adapters {
				if err := os.WriteFile(filepath.Join(root, path), []byte(source), 0755); err != nil {
					t.Fatal(err)
				}
			}
			cmd := exec.Command("bash", "-e", "-o", "pipefail", "-c", smoke)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "PATH="+root+string(os.PathListSeparator)+os.Getenv("PATH"), "FAIL_ARM64="+failArm64)
			output, err := cmd.CombinedOutput()
			if (err != nil) != (failArm64 == "true") {
				t.Errorf("arm64 failure must fail the CI step: err=%v output=%s", err, output)
			}
			calls, err := os.ReadFile(filepath.Join(root, "calls"))
			if err != nil {
				t.Fatal(err)
			}
			want := digest + " linux/amd64\n" + digest + " linux/arm64\n"
			if string(calls) != want {
				t.Errorf("smokes must use one immutable image for both platforms: got %q want %q", calls, want)
			}
		})
	}
}
