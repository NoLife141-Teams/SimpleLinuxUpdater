package installationlock

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestInstallationLockProcessHelper(t *testing.T) {
	path := os.Getenv("SLU_TEST_INSTALLATION_LOCK")
	if path == "" {
		return
	}
	lease, err := Acquire(path, true)
	if err != nil {
		fmt.Fprintln(os.Stdout, "BUSY")
		os.Exit(2)
	}
	fmt.Fprintln(os.Stdout, "LOCKED")
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = lease.Close()
	os.Exit(0)
}

func startLockProcess(t *testing.T, path string) (*exec.Cmd, io.WriteCloser, *bufio.Reader) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestInstallationLockProcessHelper$")
	cmd.Env = append(os.Environ(), "SLU_TEST_INSTALLATION_LOCK="+path)
	in, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	return cmd, in, bufio.NewReader(out)
}

func TestInstallationLockExcludesProcessesAndSurvivesDatabaseReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.db")
	if err := os.WriteFile(path, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	first, in, out := startLockProcess(t, path)
	if line, err := out.ReadString('\n'); err != nil || line != "LOCKED\n" {
		t.Fatal(line, err)
	}
	if lease, err := Acquire(path, false); !errors.Is(err, ErrBusy) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatalf("concurrent acquisition %v", err)
	}
	second, secondIn, secondOut := startLockProcess(t, path)
	if line, _ := secondOut.ReadString('\n'); line != "BUSY\n" {
		t.Fatal(line)
	}
	_ = secondIn.Close()
	if err := second.Wait(); err == nil {
		t.Fatal("concurrent process acquired lock")
	}
	replacement := path + ".replacement"
	if err := os.WriteFile(replacement, []byte("restored"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if lease, err := Acquire(path, false); !errors.Is(err, ErrBusy) {
		if lease != nil {
			_ = lease.Close()
		}
		t.Fatal("restore replaced lock", err)
	}
	_ = in.Close()
	if err := first.Wait(); err != nil {
		t.Fatal(err)
	}
	lease, err := Acquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	_ = lease.Close()
	if _, err = os.Stat(path + ".lock"); err != nil {
		t.Fatal("lock file should remain stable", err)
	}
}

func TestInstallationLockRejectsLinksAndResolvesParentAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "servers.db")
	if err := os.WriteFile(path, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.db")
	if err := os.Link(path, alias); err != nil {
		t.Skip("hardlinks unavailable", err)
	}
	if lease, err := Acquire(alias, false); err == nil {
		_ = lease.Close()
		t.Fatal("accepted hard-linked DB")
	}
	_ = os.Remove(alias)
	if err := os.Symlink(path, alias); err == nil {
		if lease, err := Acquire(alias, false); err == nil {
			_ = lease.Close()
			t.Fatal("accepted symlink DB")
		}
		_ = os.Remove(alias)
	}
	lease, err := Acquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	parentAlias := filepath.Join(t.TempDir(), "data")
	if err := os.Symlink(dir, parentAlias); err == nil {
		if other, err := Acquire(filepath.Join(parentAlias, "servers.db"), false); !errors.Is(err, ErrBusy) {
			if other != nil {
				_ = other.Close()
			}
			t.Fatal("parent alias bypassed lock", err)
		}
	}
}
