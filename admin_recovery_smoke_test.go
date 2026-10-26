package main

import (
	"os/exec"
	"runtime"
	"testing"
)

func TestRecoverySmokeSelectsOnlyLocalDockerDaemons(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Docker smoke targets Linux")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for smoke regression")
	}
	command := exec.Command(python, "-m", "unittest", "discover", "-s", "tools/ci", "-p", "test_admin_recovery_smoke.py", "-v")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("local Docker selection: %v\n%s", err, output)
	}
}
