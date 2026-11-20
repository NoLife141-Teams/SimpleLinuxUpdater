package backup

import (
	"errors"
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

func (s *Service) recoveryReplacementNeeds(database, hosts, stagedHosts string, databaseBytes uint64, diagnosis []string) []restoreStorageNeed {
	// A checkpoint can grow the live SQLite file before the atomic rename.
	sqlite := recoveryFileNeed(restoreEntryDirectory(database), restoreStorageDatabase, database+"-wal")
	sqlite.bytes = storageSum(sqlite.bytes, databaseBytes)
	needs := []restoreStorageNeed{sqlite}
	if stagedHosts != "" {
		needs = append(needs, recoveryFileNeed(restoreEntryDirectory(hosts), restoreStorageHostKeys, stagedHosts))
	}
	if len(diagnosis) != 0 {
		needs = append(needs, recoveryFileNeed(restoreEntryDirectory(database), offlineRecoveryDiagnosis, diagnosis...))
	}
	return needs
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
