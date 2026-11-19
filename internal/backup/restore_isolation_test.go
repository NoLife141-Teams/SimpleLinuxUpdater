package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestRestoreIsolationActivatesAfterDrainingOriginalWork(t *testing.T) {
	dir := t.TempDir()
	isolation := NewRestoreIsolation(func() string { return filepath.Join(dir, "servers.db") })
	f := newDurabilityFixture(t, dir, testRestoredRuntime{
		prepare: func() {
			if isolation.BlocksExternalWork() {
				t.Error("isolation became active before the original runtime finished draining")
			}
		},
		reload: func(context.Context) error {
			fresh := NewRestoreIsolation(isolation.databasePath)
			if !fresh.BlocksExternalWork() {
				t.Error("isolation did not survive from cutover into the restored runtime")
			}
			return nil
		},
	})
	if err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{BeforeReplacement: isolation.Activate}); err != nil {
		t.Fatal(err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, archivedRestoreTrust, false)
}

func TestRestoreIsolationRetainsActiveStateOnFailedResumeSync(t *testing.T) {
	dir := t.TempDir()
	isolation := NewRestoreIsolation(func() string { return filepath.Join(dir, "servers.db") })
	if err := isolation.Activate(); err != nil {
		t.Fatal(err)
	}
	injected := false
	isolation.files.sync = func(file *os.File) error {
		if file.Name() == dir && !injected {
			injected = true
			return syscall.EIO
		}
		return file.Sync()
	}
	resumeAt := time.Now().UTC().Truncate(time.Minute).Add(time.Minute)
	if err := isolation.Reactivate(resumeAt); !errors.Is(err, syscall.EIO) {
		t.Fatalf("release with a directory synchronization error = %v", err)
	}
	fresh := NewRestoreIsolation(isolation.databasePath)
	if active, err := fresh.State(); err != nil || !active {
		t.Fatalf("isolation after a failed resumption and restart = %v, %v", active, err)
	}
	if err := fresh.Reactivate(resumeAt); err != nil {
		t.Fatal(err)
	}
	fresh = NewRestoreIsolation(isolation.databasePath)
	if active, err := fresh.State(); err != nil || active {
		t.Fatalf("isolation after a successful resumption and restart = %v, %v", active, err)
	}
}
