package backup

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	database := filepath.Join(dir, "servers.db")
	isolation := NewRestoreIsolation(func() string { return database })
	if err := isolation.Activate(); err != nil {
		t.Fatal(err)
	}
	injected := false
	isolation.files.sync = func(file *os.File) error {
		if file.Name() == dir {
			state, err := loadRestoreIsolationState(RestoreIsolationPath(database))
			if err == nil && !state.Active {
				injected = true
				return syscall.EIO
			}
		}
		if injected {
			return syscall.ENOSPC
		}
		return file.Sync()
	}
	now := func() time.Time { return time.Date(2026, 11, 19, 12, 0, 43, 0, time.UTC) }
	if err := isolation.Reactivate(now); !errors.Is(err, syscall.EIO) {
		t.Fatalf("release with a directory synchronization error = %v", err)
	}
	fresh := NewRestoreIsolation(isolation.databasePath)
	if active, err := fresh.State(); err != nil || !active {
		t.Fatalf("isolation after a failed resumption and restart = %v, %v", active, err)
	}
	if err := fresh.Reactivate(now); err != nil {
		t.Fatal(err)
	}
	fresh = NewRestoreIsolation(isolation.databasePath)
	if active, err := fresh.State(); err != nil || active {
		t.Fatalf("isolation after a successful resumption and restart = %v, %v", active, err)
	}
}

func TestIsolatedDatabaseRestorePreservesConflictingHostTrust(t *testing.T) {
	dir := t.TempDir()
	fixture := newDurabilityFixture(t, dir, testRestoredRuntime{})
	isolation := NewRestoreIsolation(func() string { return fixture.db })
	path := RestoreIsolationPath(fixture.db)
	if err := os.WriteFile(path, []byte(originalRestoreTrust), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.service.deps.KnownHostsWritePath = func() (string, error) { return path, nil }
	delete(fixture.files, "known_hosts")
	err := fixture.service.applyArchiveFiles(t.Context(), fixture.files, RestoreOptions{BeforeReplacement: isolation.Activate})
	if err == nil || !strings.Contains(err.Error(), "known_hosts restore target conflicts") {
		t.Fatalf("isolated database-only restore with conflicting trust path = %v", err)
	}
	requireDurableRestoreState(t, fixture.db, path, originalRestoreServer, originalRestoreTrust, false)
}

func TestRestoreIsolationStaysActiveIfResumeSpansScheduleBoundary(t *testing.T) {
	dir := t.TempDir()
	database := filepath.Join(dir, "servers.db")
	isolation := NewRestoreIsolation(func() string { return database })
	if err := isolation.Activate(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 19, 12, 0, 59, 0, time.UTC)
	clock := func() time.Time { return now }
	isolation.files.sync = func(file *os.File) error {
		if file.Name() == dir {
			state, err := loadRestoreIsolationState(RestoreIsolationPath(database))
			if err == nil && !state.Active {
				now = now.Add(2 * time.Second)
			}
		}
		return file.Sync()
	}
	if err := isolation.Reactivate(clock); err == nil {
		t.Fatal("resumption crossed its schedule boundary and still released isolation")
	}
	fresh := NewRestoreIsolation(isolation.databasePath)
	if _, blocked := fresh.ScheduledWork(now); !blocked {
		t.Fatal("interrupted resumption allowed the missed slot after restart")
	}
	if err := fresh.Reactivate(clock); err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 19, 12, 2, 0, 0, time.UTC)
	if resumeAt, blocked := fresh.ScheduledWork(now); !blocked || !resumeAt.Equal(want) {
		t.Fatalf("resumption after retry scheduled at %v, blocked = %v", resumeAt, blocked)
	}
}
