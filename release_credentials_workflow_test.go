package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

func TestReleaseCredentialIsConfinedToGitHubReleaseSteps(t *testing.T) {
	var workflow struct {
		Env  map[string]string `yaml:"env"`
		Jobs map[string]struct {
			Env   map[string]string `yaml:"env"`
			Steps []struct {
				Name string            `yaml:"name"`
				Run  string            `yaml:"run"`
				With map[string]string `yaml:"with"`
				Env  map[string]string `yaml:"env"`
			} `yaml:"steps"`
		} `yaml:"jobs"`
	}
	source := readWorkflowForTest(t, ".github/workflows/release.yml")
	if err := yaml.Unmarshal([]byte(source), &workflow); err != nil {
		t.Fatal(err)
	}
	allowed := 0
	for _, job := range workflow.Jobs {
		for _, step := range job.Steps {
			switch step.Name {
			case "Create GitHub release":
				if step.With["token"] != "${{ secrets.RELEASE_TOKEN || github.token }}" {
					t.Fatal("draft creation must support the scoped release credential")
				}
				allowed++
			case "Finalize GitHub release":
				if step.Env["RELEASE_TOKEN"] != "${{ secrets.RELEASE_TOKEN }}" || step.Env["GH_TOKEN"] != "${{ github.token }}" {
					t.Fatal("finalization must keep release and registry credentials separate")
				}
				if !strings.HasSuffix(strings.TrimSpace(step.Run), "publication-policy.py\" finalize") {
					t.Fatal("release credential must be confined to finalization")
				}
				allowed++
			}
		}
	}
	encoded, err := json.Marshal(workflow)
	if err != nil {
		t.Fatal(err)
	}
	if allowed != 2 || strings.Count(string(encoded), "secrets.RELEASE_TOKEN") != 2 {
		t.Fatal("release credential must appear only in draft creation and finalization")
	}
	docker := workflowJobForTest(t, source, "publish-docker", "")
	if !workflowStepsOrdered(docker, "Qualify digest and promote image", "Finalize GitHub release") {
		t.Fatal("GitHub finalization must follow successful qualification and promotion")
	}
}
