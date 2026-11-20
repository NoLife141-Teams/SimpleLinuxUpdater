package backup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWindowsRestoreTargetIdentity(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "fixture.sqlite")
	if err := os.WriteFile(database, []byte("database fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	service := &Service{}
	for _, target := range []string{database, database + "-wal", RestoreRecoveryMarkerPath(database)} {
		variant := filepath.Join(dir, strings.ToUpper(filepath.Base(target)))
		if err := service.validateRestoreHostTarget(database, variant, true, true); !errors.Is(err, ErrRestoreHostTarget) {
			t.Errorf("reserved restore destination was accepted: %v", err)
		}
		if same, err := sameRestoreEntry(target, variant); err != nil || !same {
			t.Errorf("recovery path identity did not survive capitalization: %v, %v", same, err)
		}
	}
	if err := service.validateRestoreHostTarget(database, filepath.Join(dir, "host-trust"), true, true); err != nil {
		t.Fatalf("separate destination was rejected: %v", err)
	}
	if data, err := os.ReadFile(database); err != nil || string(data) != "database fixture" {
		t.Fatalf("target validation changed the existing fixture: %v", err)
	}
}
