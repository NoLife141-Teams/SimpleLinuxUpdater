package backup

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func interruptedRecoveryFixture(t *testing.T) (durabilityFixture, string, string) {
	t.Helper()
	root := t.TempDir()
	calls := 0
	f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
		calls++
		return errors.New("fixture reload failure")
	}})
	f.service.deps.ConfigPath = func() string { return f.files["config.json"] }
	f.service.syncRestoreFile = func(file *os.File) error {
		if calls == 1 && filepath.Dir(file.Name()) == root && strings.HasPrefix(filepath.Base(file.Name()), ".restore-") {
			return syscall.EIO
		}
		return file.Sync()
	}
	var incomplete *IncompleteRecoveryError
	if err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{}); !errors.As(err, &incomplete) {
		t.Fatalf("expected incomplete recovery: %v", err)
	}
	f.service.syncRestoreFile = nil
	id := filepath.Base(incomplete.SnapshotDir)
	return f, root, id
}

func offlineRecoveryMergeFixtureJobs(context.Context, *sql.DB, *sql.DB) error { return nil }

func TestOfflineRecoveryInspectsAndAppliesWithDurableDiagnosis(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	before, err := os.ReadFile(f.db)
	if err != nil {
		t.Fatal(err)
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), "", offlineRecoveryMergeFixtureJobs)
	if err != nil || !report.RecoveryRequired || len(report.Sets) != 1 || report.Sets[0].ID != id || !report.Sets[0].Ready {
		t.Fatalf("inspect = %+v, %v", report, err)
	}
	current, err := os.ReadFile(f.db)
	if err != nil || !bytes.Equal(before, current) {
		t.Fatal("inspection changed the live database")
	}
	if _, err := f.service.OfflineRestoreRecovery(t.Context(), "slu-restore-rollback-0", offlineRecoveryMergeFixtureJobs); err == nil {
		t.Fatal("recovery accepted an unselected set")
	}
	report, err = f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err != nil || report.Applied != id || report.RecoveryRequired || report.Diagnosis == "" {
		t.Fatalf("apply = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
	retained, err := os.ReadFile(filepath.Join(root, report.Diagnosis, "snapshot-0"))
	if err != nil || !bytes.Equal(before, retained) {
		t.Fatal("diagnosis did not preserve pre-recovery bytes")
	}
}

func TestOfflineRecoveryRejectsUnrelatedSnapshotAndKeepsLatch(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	manifest := filepath.Join(root, id, "recovery.json")
	raw, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	var entries []fileRestoreSnapshot
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatal(err)
	}
	entries[0].BackupPath = f.files["servers.db"]
	raw, err = json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || len(report.Sets) != 1 || report.Sets[0].Blocker != "invalid_manifest" || !report.RecoveryRequired {
		t.Fatalf("apply = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, originalRestoreTrust, true)
}

func TestOfflineRecoveryMarkerRemovalSyncFailureKeepsSQLiteLatch(t *testing.T) {
	f, root, id := interruptedRecoveryFixture(t)
	f.service.syncRestoreFile = func(file *os.File) error {
		if file.Name() == root {
			if _, err := os.Stat(RestoreRecoveryMarkerPath(f.db)); errors.Is(err, os.ErrNotExist) {
				return syscall.EIO
			}
		}
		return file.Sync()
	}
	report, err := f.service.OfflineRestoreRecovery(t.Context(), id, offlineRecoveryMergeFixtureJobs)
	if err == nil || !report.RecoveryRequired || report.Diagnosis == "" || report.Applied != "" {
		t.Fatalf("failed marker sync = %+v, %v", report, err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, true)
}
