package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func runGitForReleaseLineageTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	gitArgs := append([]string{"-c", "commit.gpgSign=false", "-c", "tag.gpgSign=false"}, args...)
	cmd := exec.Command("git", gitArgs...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestReleaseLineageAcceptsMainAndRejectsUnmergedCommit(t *testing.T) {
	const script = "tools/release/verify-tag-on-main.sh"
	bash := "bash"
	if runtime.GOOS == "windows" {
		bash = os.Getenv("SLU_RELEASE_LINEAGE_BASH")
		if bash == "" {
			t.Skip("set SLU_RELEASE_LINEAGE_BASH to the Git for Windows Bash path")
		}
		if runtime.GOARCH != "amd64" {
			t.Fatalf("Windows release lineage qualification requires amd64, got %s", runtime.GOARCH)
		}
	} else {
		// The release workflow invokes the checkout script directly.
		info, err := os.Stat(script)
		if err != nil {
			t.Fatalf("stat checkout script: %v", err)
		}
		if info.Mode().Perm()&0o100 == 0 {
			t.Fatalf("checkout script %s must be executable", script)
		}
	}

	testRoot := t.TempDir()
	origin := filepath.Join(testRoot, "origin.git")
	work := filepath.Join(testRoot, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatalf("create worktree: %v", err)
	}
	runGitForReleaseLineageTest(t, testRoot, "init", "--bare", "--initial-branch=main", origin)
	runGitForReleaseLineageTest(t, work, "init", "--initial-branch=main")
	runGitForReleaseLineageTest(t, work, "config", "user.name", "Release Test")
	runGitForReleaseLineageTest(t, work, "config", "user.email", "release-test@example.invalid")
	runGitForReleaseLineageTest(t, work, "remote", "add", "origin", origin)

	if err := os.MkdirAll(filepath.Join(work, filepath.Dir(script)), 0o755); err != nil {
		t.Fatalf("create release tools directory: %v", err)
	}
	for _, name := range []string{".gitattributes", script} {
		source, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read checkout %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(work, name), source, 0o644); err != nil {
			t.Fatalf("copy checkout %s: %v", name, err)
		}
	}
	trackedFile := filepath.Join(work, "release.txt")
	if err := os.WriteFile(trackedFile, []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write main fixture: %v", err)
	}
	runGitForReleaseLineageTest(t, work, "add", ".gitattributes", "release.txt")
	runGitForReleaseLineageTest(t, work, "add", "--chmod=+x", script)
	runGitForReleaseLineageTest(t, work, "commit", "-m", "main release")
	mainSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "HEAD")
	runGitForReleaseLineageTest(t, work, "tag", "-a", "v1.0.0", "-m", "main release", mainSHA)
	mainTagSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "refs/tags/v1.0.0")
	runGitForReleaseLineageTest(t, work, "commit", "--allow-empty", "-m", "advance main")
	mainTip := runGitForReleaseLineageTest(t, work, "rev-parse", "HEAD")
	runGitForReleaseLineageTest(t, work, "push", "origin", "main", "refs/tags/v1.0.0")

	runGitForReleaseLineageTest(t, work, "checkout", "-b", "unmerged")
	if err := os.WriteFile(trackedFile, []byte("unmerged\n"), 0o644); err != nil {
		t.Fatalf("write unmerged fixture: %v", err)
	}
	runGitForReleaseLineageTest(t, work, "add", "release.txt")
	runGitForReleaseLineageTest(t, work, "commit", "-m", "unmerged release")
	unmergedSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "HEAD")
	runGitForReleaseLineageTest(t, work, "tag", "-a", "v1.0.1", "-m", "unmerged release", unmergedSHA)
	runGitForReleaseLineageTest(t, work, "push", "origin", "refs/tags/v1.0.1")
	unmergedTagSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "refs/tags/v1.0.1")

	checkout := filepath.Join(testRoot, "checkout")
	runGitForReleaseLineageTest(t, testRoot, "clone", "--no-tags", "-c", "core.autocrlf=true", origin, checkout)
	ordinaryFile, err := os.ReadFile(filepath.Join(checkout, "release.txt"))
	if err != nil || string(ordinaryFile) != "main\r\n" {
		t.Fatalf("fixture checkout did not convert the ordinary text file to CRLF: %v", err)
	}
	checkedScript, err := os.ReadFile(filepath.Join(checkout, script))
	if err != nil || strings.ContainsRune(string(checkedScript), '\r') {
		t.Fatalf("fixture checkout did not preserve LF for the release script: %v", err)
	}
	t.Logf("Qualified LF script checkout with core.autocrlf=true on %s/%s", runtime.GOOS, runtime.GOARCH)
	for _, testCase := range []struct {
		name       string
		tag        string
		sha        string
		wantErr    bool
		wantOutput string
	}{
		{name: "main history", tag: "v1.0.0", sha: mainTagSHA, wantOutput: "is in origin/main history"},
		{name: "mismatched signal", tag: "v1.0.0", sha: mainTip, wantErr: true, wantOutput: "does not resolve to"},
		{name: "unmerged branch", tag: "v1.0.1", sha: unmergedTagSHA, wantErr: true, wantOutput: "is not in origin/main history"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			// Match the Windows release step's Git Bash shell and relative command.
			cmd := exec.Command(bash, "--noprofile", "--norc", "-e", "-o", "pipefail", "-c", script)
			cmd.Dir = checkout
			cmd.Env = append(os.Environ(), "RELEASE_TAG="+testCase.tag, "RELEASE_SHA="+testCase.sha)
			output, runErr := cmd.CombinedOutput()
			if (runErr != nil) != testCase.wantErr {
				t.Fatalf("verify lineage error = %v, wantErr %t\n%s", runErr, testCase.wantErr, output)
			}
			if !strings.Contains(string(output), testCase.wantOutput) {
				t.Fatalf("verify lineage output = %q, want %q", output, testCase.wantOutput)
			}
		})
	}
}
