package main

import (
	"os/exec"
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
