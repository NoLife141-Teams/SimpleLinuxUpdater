//go:build linux

package persistenceowner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepairOwnsNewExternalAncestorsWithoutTouchingExistingParents(t *testing.T) {
	root := t.TempDir()
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		t.Fatal(err)
	}

	existingParent := filepath.Join(root, "var", "lib")
	if err := os.MkdirAll(existingParent, 0o755); err != nil {
		t.Fatal(err)
	}
	firstCreated := filepath.Join(existingParent, "simplelinuxupdater")
	secondCreated := filepath.Join(firstCreated, "tenant")
	dbPath := filepath.Join(secondCreated, "servers.db")

	r, recorded := recordingRepairer(t)
	if err := repairWith(Config{
		UID:        os.Getuid(),
		GID:        os.Getgid(),
		DataRoot:   dataRoot,
		WorkingDir: root,
		DBPath:     dbPath,
	}, r); err != nil {
		t.Fatalf("repairWith() error = %v", err)
	}

	for _, want := range []string{firstCreated, secondCreated} {
		if !containsRecordedPath(*recorded, want) {
			t.Fatalf("recorded ownership repairs = %v, want newly created external ancestor %q", *recorded, want)
		}
		info, err := os.Stat(want)
		if err != nil {
			t.Fatalf("stat newly created ancestor %q: %v", want, err)
		}
		if !info.IsDir() {
			t.Fatalf("newly created persistence ancestor %q is not a directory", want)
		}
	}

	if containsRecordedPath(*recorded, existingParent) {
		t.Fatalf("pre-existing external parent must not be chowned: %v", *recorded)
	}
	if containsRecordedPath(*recorded, filepath.Dir(existingParent)) {
		t.Fatalf("pre-existing external grandparent must not be chowned: %v", *recorded)
	}
}
