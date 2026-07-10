package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaintenanceCoordinationArchitectureBoundary(t *testing.T) {
	forbidden := []string{
		"currentMaintenanceState",
		"backupRestoreMu",
		"BackupBarrier",
		"CurrentMaintenanceActive",
		"TryBackupRestoreReadLock",
	}
	err := filepath.WalkDir(".", func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path == ".git" || path == "node_modules" || path == "test-results" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		text := string(data)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Errorf("production file %s restores forbidden maintenance seam %q", path, token)
			}
		}
		if strings.Contains(text, "maintenance_state") && filepath.ToSlash(path) != "internal/maintenance/persistence.go" {
			t.Errorf("production file %s owns raw maintenance-state persistence", path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan production Go files: %v", err)
	}
}
