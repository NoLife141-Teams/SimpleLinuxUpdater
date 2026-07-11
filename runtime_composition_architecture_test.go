package main

import (
	"os"
	"strings"
	"testing"
)

func TestRestoredStateRehydrationArchitectureBoundary(t *testing.T) {
	appDepsSource, err := os.ReadFile("app_deps.go")
	if err != nil {
		t.Fatal(err)
	}
	backupSource, err := os.ReadFile("backup_restore.go")
	if err != nil {
		t.Fatal(err)
	}
	internalBackupSource, err := os.ReadFile("internal/backup/backup.go")
	if err != nil {
		t.Fatal(err)
	}

	for path, source := range map[string]string{
		"app_deps.go":       string(appDepsSource),
		"backup_restore.go": string(backupSource),
	} {
		for _, forbidden := range []string{"reloadAppRuntimeState", "reloadRuntimeState"} {
			if strings.Contains(source, forbidden) {
				t.Errorf("%s restores legacy reload implementation %q", path, forbidden)
			}
		}
	}
	for _, forbidden := range []string{"ResetRuntimeCaches", "ReloadRuntimeState"} {
		if strings.Contains(string(internalBackupSource), forbidden) {
			t.Errorf("backup adapter restores per-step runtime callback %q", forbidden)
		}
	}
	for _, required := range []string{"RestoredRuntime", "PreparePersistenceReplacement", "ReloadRestoredState"} {
		if !strings.Contains(string(internalBackupSource), required) {
			t.Errorf("backup adapter lost Runtime Composition interface %q", required)
		}
	}
}
