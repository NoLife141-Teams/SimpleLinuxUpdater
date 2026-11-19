package main

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"debian-updater/internal/installationlock"
)

func TestBackupRecoveryRequiresExplicitModeAndExclusiveInstallationLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "servers.db")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
	var output bytes.Buffer
	for _, args := range [][]string{
		{"backup", "recover"}, {"backup", "recover", "--inspect", "--apply", "slu-restore-rollback-1"},
		{"backup", "recover", "--apply", ""},
	} {
		if handled, err := runRestoreRecoveryCommand(args, &output); !handled || err == nil || output.Len() != 0 {
			t.Fatalf("invalid recovery mode = %v, %v, %q", handled, err, output.String())
		}
	}
	lock, err := installationlock.Acquire(path, false)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	handled, err := runRestoreRecoveryCommand([]string{"backup", "recover", "--apply", "slu-restore-rollback-1"}, &output)
	if !handled || !errors.Is(err, installationlock.ErrBusy) || output.Len() != 0 {
		t.Fatalf("live installation apply = %v, %v, %q", handled, err, output.String())
	}
}
