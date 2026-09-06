//go:build linux

package persistenceowner

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordingRepairer(t *testing.T) (*repairer, *[]string) {
	t.Helper()
	paths := make([]string, 0, 16)
	r := newRepairer(os.Getuid(), os.Getgid())
	r.fchown = func(fd, _, _ int) error {
		path, err := descriptorPath(fd)
		if err != nil {
			return err
		}
		paths = append(paths, strings.TrimSuffix(path, " (deleted)"))
		return nil
	}
	return r, &paths
}

func containsRecordedPath(paths []string, target string) bool {
	target = filepath.Clean(target)
	for _, path := range paths {
		if filepath.Clean(path) == target {
			return true
		}
	}
	return false
}

func writePersistenceTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRepairNormalizesPathsAndRepairsRollbackJournal(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	dbPath := filepath.Join(dataRoot, "tenant", "state", "servers.db")
	knownHosts := filepath.Join(dataRoot, "tenant", "state", "ssh", "known_hosts")

	for _, path := range []string{
		filepath.Join(dataRoot, "servers.json"),
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		dbPath + "-journal",
		filepath.Join(filepath.Dir(dbPath), "config.json"),
		knownHosts,
	} {
		writePersistenceTestFile(t, path)
	}

	r, recorded := recordingRepairer(t)
	err := repairWith(Config{
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		DataRoot:   dataRoot,
		WorkingDir: root,
		DBPath:     "  data/tenant/state/servers.db\t ",
		KnownHosts: "  data/tenant/state/ssh/known_hosts : /etc/ssh/ssh_known_hosts  ",
	}, r)
	if err != nil {
		t.Fatalf("repairWith() error = %v", err)
	}

	for _, want := range []string{
		dataRoot,
		filepath.Join(dataRoot, "tenant"),
		filepath.Join(dataRoot, "tenant", "state"),
		dbPath,
		dbPath + "-journal",
		filepath.Dir(knownHosts),
		knownHosts,
	} {
		if !containsRecordedPath(*recorded, want) {
			t.Fatalf("recorded ownership repairs = %v, want %q", *recorded, want)
		}
	}
	for _, got := range *recorded {
		if strings.HasPrefix(got, "/etc/ssh") {
			t.Fatalf("system known-hosts path must not be chowned: %v", *recorded)
		}
	}
}

func TestRepairRejectsSymlinkPersistenceFile(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "sentinel")
	writePersistenceTestFile(t, sentinel)
	dbPath := filepath.Join(dataRoot, "servers.db")
	if err := os.Symlink(sentinel, dbPath); err != nil {
		t.Fatal(err)
	}

	r, recorded := recordingRepairer(t)
	err := repairWith(Config{
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		DataRoot:   dataRoot,
		WorkingDir: root,
		DBPath:     dbPath,
	}, r)
	if err == nil {
		t.Fatal("repairWith() succeeded with symlink DB, want safe failure")
	}
	if !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("repairWith() error = %v, want symbolic-link refusal", err)
	}
	if containsRecordedPath(*recorded, sentinel) {
		t.Fatalf("symlink target was chowned: %v", *recorded)
	}
}

func TestRepairSkipsConfiguredKnownHostsOutsidePersistence(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	dbPath := filepath.Join(dataRoot, "servers.db")
	writePersistenceTestFile(t, dbPath)

	r, recorded := recordingRepairer(t)
	if err := repairWith(Config{
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		DataRoot:   dataRoot,
		WorkingDir: root,
		DBPath:     dbPath,
		KnownHosts: "/etc/ssh/ssh_known_hosts",
	}, r); err != nil {
		t.Fatalf("repairWith() error = %v", err)
	}
	for _, got := range *recorded {
		if strings.HasPrefix(got, "/etc/ssh") {
			t.Fatalf("external known-hosts path was chowned: %v", *recorded)
		}
	}
}

func TestDescriptorWalkDoesNotFollowSwappedAncestor(t *testing.T) {
	root := t.TempDir()
	managed := filepath.Join(root, "data")
	insideA := filepath.Join(managed, "a")
	insideB := filepath.Join(insideA, "b")
	target := filepath.Join(insideB, "target")
	outside := filepath.Join(root, "outside")
	outsideTarget := filepath.Join(outside, "b", "target")
	writePersistenceTestFile(t, target)
	writePersistenceTestFile(t, outsideTarget)

	r, recorded := recordingRepairer(t)
	swapped := false
	r.afterOpenDir = func(path string) error {
		if swapped || filepath.Clean(path) != filepath.Clean(insideA) {
			return nil
		}
		oldA := insideA + "-opened"
		if err := os.Rename(insideA, oldA); err != nil {
			return fmt.Errorf("rename opened ancestor: %w", err)
		}
		if err := os.Symlink(outside, insideA); err != nil {
			return fmt.Errorf("replace ancestor with symlink: %w", err)
		}
		swapped = true
		return nil
	}

	if err := r.repairFile(target, false, ownershipPlan{start: managed}); err != nil {
		t.Fatalf("repairFile() error = %v", err)
	}
	if !swapped {
		t.Fatal("test did not swap the opened ancestor")
	}
	if containsRecordedPath(*recorded, outsideTarget) {
		t.Fatalf("descriptor walk followed swapped symlink to outside target: %v", *recorded)
	}
	openedTarget := filepath.Join(insideA+"-opened", "b", "target")
	if !containsRecordedPath(*recorded, openedTarget) {
		t.Fatalf("ownership repair did not stay on opened descriptor tree: %v; want %q", *recorded, openedTarget)
	}
}
