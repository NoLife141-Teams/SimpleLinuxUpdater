package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGitForReleaseLineageTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}
	return strings.TrimSpace(string(output))
}

func TestReleaseLineageAcceptsMainAndRejectsUnmergedCommit(t *testing.T) {
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

	trackedFile := filepath.Join(work, "release.txt")
	if err := os.WriteFile(trackedFile, []byte("main\n"), 0o644); err != nil {
		t.Fatalf("write main fixture: %v", err)
	}
	runGitForReleaseLineageTest(t, work, "add", "release.txt")
	runGitForReleaseLineageTest(t, work, "commit", "-m", "main release")
	mainSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "HEAD")
	runGitForReleaseLineageTest(t, work, "push", "-u", "origin", "main")
	runGitForReleaseLineageTest(t, work, "tag", "-a", "v1.0.0", "-m", "main release", mainSHA)
	runGitForReleaseLineageTest(t, work, "push", "origin", "refs/tags/v1.0.0")
	mainTagSHA := runGitForReleaseLineageTest(t, work, "rev-parse", "refs/tags/v1.0.0")

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

	script, err := filepath.Abs("tools/release/verify-tag-on-main.sh")
	if err != nil {
		t.Fatalf("resolve release lineage script: %v", err)
	}
	for _, testCase := range []struct {
		name       string
		tag        string
		sha        string
		wantErr    bool
		wantOutput string
	}{
		{name: "main history", tag: "v1.0.0", sha: mainTagSHA, wantOutput: "is in origin/main history"},
		{name: "mismatched signal", tag: "v1.0.0", sha: unmergedTagSHA, wantErr: true, wantOutput: "does not resolve to"},
		{name: "unmerged branch", tag: "v1.0.1", sha: unmergedTagSHA, wantErr: true, wantOutput: "is not in origin/main history"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			cmd := exec.Command(script)
			cmd.Dir = work
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
