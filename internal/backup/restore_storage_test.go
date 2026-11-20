package backup

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"debian-updater/internal/installationhealth"
)

func TestRestoreStorageReviewInvalidatesCapacityWhenPreparationRunsOutOfSpace(t *testing.T) {
	f := newDurabilityFixture(t, t.TempDir(), testRestoredRuntime{})
	archive := restoreStorageFixtureArchive(t, f)
	f.service.deps.ProbeRestoreStorage = func(string) installationhealth.VolumeCapacity {
		available := uint64(128 << 20)
		return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &available}
	}
	f.service.deps.EnsureSchema = func(*sql.DB) error {
		return &os.PathError{Op: "write", Path: f.db, Err: syscall.ENOSPC}
	}
	result, err := f.service.VerifyArchiveFileWithReview(t.Context(), archive, testPassphrase, nil)
	if err != nil || result.RestoreReady || len(result.Storage) != 1 {
		t.Fatalf("review after preparation ran out of space = %+v, %v", result, err)
	}
	if result.Storage[0].State != restoreStorageUnknown || result.Storage[0].AvailableBytes != nil {
		t.Fatalf("review reported stale available capacity after storage exhaustion: %+v", result.Storage[0])
	}
	if len(result.Blockers) != 1 || result.Blockers[0].Code != "insufficient_storage" ||
		strings.Contains(result.Blockers[0].Message, f.db) {
		t.Fatalf("unexpected storage blocker = %+v", result.Blockers)
	}
}

func TestOfflineArchiveVerificationReportsWorkspaceQuota(t *testing.T) {
	f := newDurabilityFixture(t, t.TempDir(), testRestoredRuntime{})
	archive := restoreStorageFixtureArchive(t, f)
	f.service.deps.ProbeRestoreStorage = func(string) installationhealth.VolumeCapacity {
		available := uint64(128 << 20)
		return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &available}
	}
	f.service.deps.EnsureSchema = func(*sql.DB) error {
		return &os.PathError{Op: "write", Path: f.db, Err: syscall.EDQUOT}
	}
	result, err := f.service.VerifyArchiveFileOffline(t.Context(), archive, testPassphrase)
	if err != nil || result.RestoreReady || len(result.Storage) != 1 {
		t.Fatalf("offline verification after exhausting a quota = %+v, %v", result, err)
	}
	if !reflect.DeepEqual(result.Storage[0].Roles, []string{restoreStoragePreparation}) ||
		result.Storage[0].State != restoreStorageUnknown || result.Storage[0].AvailableBytes != nil {
		t.Fatalf("offline storage report used the volume capacity in place of the exhausted quota: %+v", result.Storage[0])
	}
	if len(result.Blockers) != 1 || result.Blockers[0].Code != "insufficient_storage" {
		t.Fatalf("quota failure was not reported as a storage blocker: %+v", result.Blockers)
	}
}

func restoreStorageFixtureArchive(t *testing.T, f durabilityFixture) TemporaryFile {
	t.Helper()
	files := make(map[string][]byte, len(f.files))
	for name, path := range f.files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		files[name] = data
	}
	payload, err := BuildTarGz(files)
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := EncryptPayload(payload, testPassphrase)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "restore.slubkp")
	if err := os.WriteFile(path, encrypted, 0600); err != nil {
		t.Fatal(err)
	}
	return TemporaryFile{Path: path, Size: int64(len(encrypted))}
}

func TestRestoreStorageReviewGroupsRolesAndKeepsUnknownCapacityExplicit(t *testing.T) {
	root := t.TempDir()
	f := newDurabilityFixture(t, root, testRestoredRuntime{})
	archive := restoreStorageFixtureArchive(t, f)
	afterPreview := func() VerifyResult {
		t.Helper()
		result, err := f.service.VerifyArchiveFileWithReview(t.Context(), archive, testPassphrase, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Compatible || !result.ConfigValid || !result.DatabaseValid {
			t.Fatalf("archive validation did not complete: %+v", result)
		}
		return result
	}
	var sameDevice, unknownTrust bool
	free, insufficient := uint64(128<<20), uint64(1<<20)
	dbFree := &free
	f.service.deps.ProbeRestoreStorage = func(dir string) installationhealth.VolumeCapacity {
		switch {
		case sameDevice:
			return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &free}
		case dir == f.service.deps.TempDir():
			return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &free}
		case dir == root:
			return installationhealth.VolumeCapacity{Device: 2, Identified: true, AvailableBytes: dbFree}
		case dir == filepath.Dir(f.hosts):
			volume := installationhealth.VolumeCapacity{Device: 3, Identified: true}
			if !unknownTrust {
				volume.AvailableBytes = &free
			}
			return volume
		default:
			t.Fatalf("unexpected capacity lookup for %q", dir)
			return installationhealth.VolumeCapacity{}
		}
	}
	distinct := afterPreview()
	wantGroups := [][]string{{restoreStoragePreparation}, {restoreStorageDatabase, restoreStorageRollback}, {restoreStorageHostKeys}}
	if !distinct.RestoreReady || len(distinct.Storage) != len(wantGroups) {
		t.Fatalf("distinct storage review = %+v", distinct)
	}
	var sum uint64
	for i, volume := range distinct.Storage {
		if !reflect.DeepEqual(volume.Roles, wantGroups[i]) || volume.State != restoreStorageReady || *volume.AvailableBytes != free {
			t.Fatalf("distinct volume %d = %+v", i, volume)
		}
		sum += volume.RequiredBytes
	}
	sameDevice = true
	shared := afterPreview()
	if !shared.RestoreReady || len(shared.Storage) != 1 || len(shared.Storage[0].Roles) != 4 ||
		shared.Storage[0].RequiredBytes != sum-2*restoreStorageMinMargin {
		t.Fatalf("shared storage review = %+v", shared.Storage)
	}
	sameDevice = false
	unknownTrust = true
	unknown := afterPreview()
	if !unknown.RestoreReady || unknown.Storage[2].State != restoreStorageUnknown || unknown.Storage[2].AvailableBytes != nil {
		t.Fatalf("unknown capacity must remain unknown and advisory: %+v", unknown.Storage)
	}
	dbFree = &insufficient
	blocked := afterPreview()
	if blocked.RestoreReady || blocked.Storage[1].State != restoreStorageInsufficient ||
		len(blocked.Blockers) != 1 || blocked.Blockers[0].Code != "insufficient_storage" ||
		strings.Contains(blocked.Blockers[0].Message, root) {
		t.Fatalf("rollback storage blocker = %+v", blocked)
	}
	portable, err := f.service.VerifyArchiveFile(t.Context(), archive, testPassphrase)
	if err != nil || !portable.RestoreReady || len(portable.Storage) != 0 {
		t.Fatalf("portable verification must depend on archive validity: %+v, %v", portable, err)
	}
	known := false
	_, err = f.service.RestoreArchiveFileWithOptions(t.Context(), archive, testPassphrase, RestoreOptions{BeforeApply: func() {
		known = true
	}})
	var storage *RestoreStorageError
	if !errors.As(err, &storage) || known {
		t.Fatalf("restore must block before preparing replacement: %v", err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

func TestRestoreStorageRechecksAfterSealingRollbackSnapshots(t *testing.T) {
	root := t.TempDir()
	reloads := 0
	f := newDurabilityFixture(t, root, testRestoredRuntime{reload: func(context.Context) error {
		reloads++
		return nil
	}})
	archive := restoreStorageFixtureArchive(t, f)
	free := uint64(128 << 20)
	f.service.deps.ProbeRestoreStorage = func(string) installationhealth.VolumeCapacity {
		return installationhealth.VolumeCapacity{Device: 1, Identified: true, AvailableBytes: &free}
	}
	preview, err := f.service.VerifyArchiveFileWithReview(t.Context(), archive, testPassphrase, nil)
	if err != nil || !preview.RestoreReady {
		t.Fatalf("initial preview must be ready: %+v %v", preview, err)
	}
	f.service.syncRestoreFile = func(file *os.File) error {
		if filepath.Base(file.Name()) == "recovery.json" {
			free = 1 << 20
		}
		return file.Sync()
	}
	beforeReplacement := false
	_, err = f.service.RestoreArchiveFileWithOptions(t.Context(), archive, testPassphrase, RestoreOptions{
		BeforeReplacement: func() error { beforeReplacement = true; return nil },
	})
	var storage *RestoreStorageError
	if !errors.As(err, &storage) || len(storage.Volumes) != 1 ||
		storage.Volumes[0].State != restoreStorageInsufficient || *storage.Volumes[0].AvailableBytes != free ||
		beforeReplacement || reloads != 1 {
		t.Fatalf("late storage recheck: %v, storage=%+v, replacement=%v, reloads=%d", err, storage, beforeReplacement, reloads)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
	if leftovers, err := filepath.Glob(filepath.Join(root, restoreRollbackPrefix+"*")); err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary rollback files after blocked restore: %v, %v", leftovers, err)
	}
	free = 128 << 20
	f.service.syncRestoreFile = nil
	if _, err := f.service.RestoreArchiveFileWithOptions(t.Context(), archive, testPassphrase, RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, archivedRestoreServer, archivedRestoreTrust, false)
}

func TestRestoreStorageExhaustionDuringReplacementRollsBack(t *testing.T) {
	f := newDurabilityFixture(t, t.TempDir(), testRestoredRuntime{})
	var injected bool
	f.service.syncRestoreFile = func(file *os.File) error {
		if !injected && strings.HasPrefix(filepath.Base(file.Name()), ".restore-") && filepath.Dir(file.Name()) == filepath.Dir(f.hosts) {
			injected = true
			return syscall.ENOSPC
		}
		return file.Sync()
	}
	err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{})
	var incomplete *IncompleteRecoveryError
	if !injected || !IsStorageFull(err) || errors.As(err, &incomplete) {
		t.Fatalf("space exhaustion must take the normal rollback path: %v", err)
	}
	requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
}

type storageRejectionRuntime struct {
	testRestoredRuntime
	resume func() error
}

func (r storageRejectionRuntime) ResumeOriginalPersistence(context.Context) error {
	return r.resume()
}

func TestRestoreStorageExhaustionBeforeReplacementResumesOriginalPersistence(t *testing.T) {
	for _, failedFile := range []string{"snapshot-0", "recovery.json", ".slu-restore-recovery-"} {
		t.Run(failedFile, func(t *testing.T) {
			root := t.TempDir()
			var f durabilityFixture
			runtime := storageRejectionRuntime{
				testRestoredRuntime: testRestoredRuntime{reload: func(context.Context) error {
					return errors.New("restored state cannot be reloaded before replacement")
				}},
				resume: func() error {
					snapshots, err := filepath.Glob(filepath.Join(root, restoreRollbackPrefix+"*"))
					if err != nil || len(snapshots) != 0 {
						t.Fatalf("unpublished snapshots still consume space while resuming: %v, %v", snapshots, err)
					}
					return nil
				},
			}
			f = newDurabilityFixture(t, root, runtime)
			f.service.syncRestoreFile = func(file *os.File) error {
				if strings.HasPrefix(filepath.Base(file.Name()), failedFile) {
					return syscall.ENOSPC
				}
				return file.Sync()
			}
			err := f.service.applyArchiveFiles(t.Context(), f.files, RestoreOptions{})
			var incomplete *IncompleteRecoveryError
			if !IsStorageFull(err) || errors.As(err, &incomplete) {
				t.Fatalf("pre-replacement storage exhaustion should leave the original runtime available: %v", err)
			}
			requireDurableRestoreState(t, f.db, f.hosts, originalRestoreServer, originalRestoreTrust, false)
		})
	}
}
