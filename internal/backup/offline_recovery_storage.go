package backup

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"slices"
)

const offlineRecoveryDiagnosis = "diagnosis_snapshots"
const offlineRecoveryStorageBlocker = "insufficient_storage"

func recoveryFileNeed(directory, role string, paths ...string) restoreStorageNeed {
	need := restoreStorageNeed{directory: directory, role: role}
	for _, path := range paths {
		bytes, known := sizeOnDisk(path)
		need.bytes = storageSum(need.bytes, bytes)
		need.incomplete = need.incomplete || !known
	}
	return need
}

func recoveryReplacementNeeds(ctx context.Context, database, hosts, stagedHosts string, databaseBytes uint64, live *sql.DB, diagnosis []string) []restoreStorageNeed {
	sqlite := restoreStorageNeed{directory: restoreEntryDirectory(database), role: restoreStorageDatabase, bytes: databaseBytes}
	if live != nil {
		growth, known := recoveryCheckpointGrowth(ctx, database, live)
		sqlite.bytes = storageSum(sqlite.bytes, growth)
		sqlite.incomplete = !known
	}
	needs := []restoreStorageNeed{sqlite}
	if stagedHosts != "" {
		needs = append(needs, recoveryFileNeed(restoreEntryDirectory(hosts), restoreStorageHostKeys, stagedHosts))
	}
	if len(diagnosis) != 0 {
		needs = append(needs, recoveryFileNeed(restoreEntryDirectory(database), offlineRecoveryDiagnosis, diagnosis...))
	}
	return needs
}

func recoveryCheckpointGrowth(ctx context.Context, database string, live *sql.DB) (uint64, bool) {
	walBytes, known := sizeOnDisk(database + "-wal")
	if !known || walBytes == 0 {
		return 0, known
	}
	currentBytes, known := sizeOnDisk(database)
	if !known {
		return 0, false
	}
	// The live copy includes committed WAL frames. Repeated versions of a
	// page take space in the diagnosis snapshot but do not grow SQLite.
	var logicalBytes uint64
	if err := live.QueryRowContext(ctx, "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()").Scan(&logicalBytes); err != nil {
		return 0, false
	}
	return logicalBytes - min(logicalBytes, currentBytes), true
}

// A matching trust file only needs a directory sync. Only budget space for
// the copy when the current file differs or the comparison is unavailable.
func recoveryHostReplacementPath(hosts string, snapshot *fileRestoreSnapshot, digestKnown bool) string {
	if snapshot == nil || !snapshot.Exists {
		return ""
	}
	size, known := sizeOnDisk(hosts)
	snapshotSize, snapshotKnown := sizeOnDisk(snapshot.BackupPath)
	if !known || !snapshotKnown || size != snapshotSize || size > MaxExtractedBytes {
		return snapshot.BackupPath
	}
	expected := snapshot.SHA256
	if !digestKnown {
		var err error
		expected, err = recoveryTrustDigest(snapshot.BackupPath)
		if err != nil {
			return snapshot.BackupPath
		}
	}
	if digest, err := recoveryTrustDigest(hosts); err == nil && digest == expected {
		return ""
	}
	return snapshot.BackupPath
}

func recoveryTrustDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := copyFileBounded(digest, file, MaxExtractedBytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

// A measured shortage carries its own observation. A write that failed after
// the observation makes the capacity of the affected volume uncertain.
func recoveryStorageFailure(volumes []RestoreStorageVolume, role string, cause error) []RestoreStorageVolume {
	var measured *RestoreStorageError
	if errors.As(cause, &measured) {
		return measured.Volumes
	}
	for i := range volumes {
		if role == "" || slices.Contains(volumes[i].Roles, role) {
			volumes[i].AvailableBytes = nil
			volumes[i].State = restoreStorageUnknown
		}
	}
	return volumes
}

func (set *OfflineRecoverySet) storageFailure(cause error, role string) error {
	set.Ready = false
	set.Blocker = offlineRecoveryStorageBlocker
	set.StorageRole = role
	set.Storage = recoveryStorageFailure(set.Storage, role, cause)
	return offlineRecoveryStorageError(cause)
}

func offlineRecoveryStorageError(cause error) error {
	var measured *RestoreStorageError
	if errors.As(cause, &measured) {
		return measured
	}
	return errOfflineRecoveryStorage
}

var errOfflineRecoveryStorage = errors.New("offline recovery ran out of storage space. Free space on the affected volume and retry")

type offlineRecoveryWriteFailure string

func (offlineRecoveryWriteFailure) Error() string { return errOfflineRecoveryStorage.Error() }
