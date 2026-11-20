package backup

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"modernc.org/sqlite"
	sqlitelib "modernc.org/sqlite/lib"
)

const (
	restoreStoragePreparation = "preparation"
	restoreStorageDatabase    = "sqlite_replacement"
	restoreStorageHostKeys    = "ssh_trust_replacement"
	restoreStorageRollback    = "rollback_snapshots"

	restoreStorageReady        = "ready"
	restoreStorageInsufficient = "insufficient"
	restoreStorageUnknown      = "unknown"
	restoreStorageMinMargin    = 8 * 1024 * 1024
)

type RestoreStorageVolume struct {
	Roles          []string `json:"roles"`
	RequiredBytes  uint64   `json:"required_bytes"`
	AvailableBytes *uint64  `json:"available_bytes"`
	State          string   `json:"state"`
}

type RestoreStorageError struct {
	Volumes []RestoreStorageVolume
}

func (e *RestoreStorageError) Error() string {
	for _, volume := range e.Volumes {
		if volume.State == restoreStorageInsufficient && volume.AvailableBytes != nil {
			return fmt.Sprintf("Insufficient space for %s. Approximately %d bytes are required and %d bytes are available.",
				restoreStorageRoleNames(volume.Roles), volume.RequiredBytes, *volume.AvailableBytes)
		}
	}
	return "The restore ran out of storage space. Free space on the affected volume before retrying."
}

// IsStorageFull identifies space exhaustion even if free space changed after a
// successful estimate. The restore still follows its normal rollback path.
func IsStorageFull(err error) bool {
	var sqliteErr *sqlite.Error
	return errors.Is(err, syscall.ENOSPC) ||
		(errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlitelib.SQLITE_FULL)
}

func restoreStorageRoleNames(roles []string) string {
	names := make([]string, 0, len(roles))
	for _, role := range roles {
		switch role {
		case restoreStoragePreparation:
			names = append(names, "archive and SQLite preparation")
		case restoreStorageDatabase:
			names = append(names, "SQLite replacement")
		case restoreStorageHostKeys:
			names = append(names, "SSH host-key replacement")
		case restoreStorageRollback:
			names = append(names, "rollback snapshots")
		}
	}
	return strings.Join(names, ", ")
}

func storageBlocker(volumes []RestoreStorageVolume) error {
	for _, volume := range volumes {
		if volume.State == restoreStorageInsufficient {
			return &RestoreStorageError{Volumes: volumes}
		}
	}
	return nil
}

type restoreStorageNeed struct {
	directory  string
	role       string
	bytes      uint64
	incomplete bool
}

type restoreStorageTargets struct {
	database string
	hosts    string
	rollback string
}

func (s *Service) restoreKnownHostsTarget(database string) string {
	if s.deps.KnownHostsWritePath != nil {
		if path, err := s.deps.KnownHostsWritePath(); err == nil && strings.TrimSpace(path) != "" {
			return path
		}
	}
	return filepath.Join(filepath.Dir(database), "known_hosts")
}

func (s *Service) previewRestoreTargets(replacesKnownHosts bool) restoreStorageTargets {
	targets := restoreStorageTargets{}
	if s.deps.DBPath != nil {
		targets.database = s.deps.DBPath()
	}
	if targets.database != "" {
		targets.rollback = restoreEntryDirectory(targets.database)
	}
	if replacesKnownHosts {
		targets.hosts = s.restoreKnownHostsTarget(targets.database)
	}
	return targets
}

func sizeOnDisk(path string) (uint64, bool) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, true
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 {
		return 0, false
	}
	// A sparse source is copied byte for byte by the restore pipeline.
	return uint64(info.Size()), true
}

func storageSum(a, b uint64) uint64 {
	if b > math.MaxUint64-a {
		return math.MaxUint64
	}
	return a + b
}

func existingStorageDirectory(directory string) string {
	if directory == "" {
		return ""
	}
	for dir := directory; ; dir = restoreEntryDirectory(dir) {
		_, err := os.Stat(dir)
		if !errors.Is(err, os.ErrNotExist) || restoreEntryDirectory(dir) == dir {
			return dir
		}
	}
}

func (s *Service) estimateRestoreStorage(needs ...restoreStorageNeed) []RestoreStorageVolume {
	type volumeEstimate struct {
		RestoreStorageVolume
		incomplete bool
	}
	groups := make([]volumeEstimate, 0, len(needs))
	devices := make(map[uint64]int)
	for _, need := range needs {
		capacity := s.deps.ProbeRestoreStorage(existingStorageDirectory(need.directory))
		index, exists := devices[capacity.Device]
		if !capacity.Identified || !exists {
			index = len(groups)
			groups = append(groups, volumeEstimate{})
			if capacity.Identified {
				devices[capacity.Device] = index
			}
		}
		group := &groups[index]
		group.Roles = append(group.Roles, need.role)
		group.RequiredBytes = storageSum(group.RequiredBytes, need.bytes)
		group.incomplete = group.incomplete || need.incomplete
		if available := capacity.AvailableBytes; available != nil && (group.AvailableBytes == nil || *available < *group.AvailableBytes) {
			group.AvailableBytes = available
		}
	}
	volumes := make([]RestoreStorageVolume, 0, len(groups))
	for _, group := range groups {
		margin := max(uint64(restoreStorageMinMargin), group.RequiredBytes/10)
		group.RequiredBytes = storageSum(group.RequiredBytes, margin)
		switch {
		case group.AvailableBytes != nil && group.RequiredBytes > *group.AvailableBytes:
			group.State = restoreStorageInsufficient
		case group.AvailableBytes == nil || group.incomplete:
			group.State = restoreStorageUnknown
		default:
			group.State = restoreStorageReady
		}
		volumes = append(volumes, group.RestoreStorageVolume)
	}
	return volumes
}

func (targets restoreStorageTargets) replacementNeeds(databaseBytes, hostsBytes uint64, withSnapshots bool) []restoreStorageNeed {
	var oldDatabaseBytes, oldHostsBytes uint64
	sqliteKnown := targets.database != ""
	if targets.database != "" {
		for _, path := range append([]string{targets.database}, SQLiteSidecarPaths(targets.database)...) {
			size, known := sizeOnDisk(path)
			oldDatabaseBytes = storageSum(oldDatabaseBytes, size)
			sqliteKnown = sqliteKnown && known
		}
	}
	databaseDir := ""
	if targets.database != "" {
		databaseDir = restoreEntryDirectory(targets.database)
	}
	needs := []restoreStorageNeed{
		{directory: databaseDir, role: restoreStorageDatabase, bytes: storageSum(databaseBytes, oldDatabaseBytes), incomplete: !sqliteKnown},
	}
	hostsKnown := true
	if targets.hosts != "" {
		oldHostsBytes, hostsKnown = sizeOnDisk(targets.hosts)
		needs = append(needs, restoreStorageNeed{
			directory: restoreEntryDirectory(targets.hosts), role: restoreStorageHostKeys,
			bytes: storageSum(hostsBytes, oldHostsBytes), incomplete: !hostsKnown,
		})
	}
	if withSnapshots {
		needs = append(needs, restoreStorageNeed{
			directory: targets.rollback, role: restoreStorageRollback, bytes: storageSum(oldDatabaseBytes, oldHostsBytes),
			incomplete: !sqliteKnown || !hostsKnown,
		})
	}
	return needs
}

func (s *Service) decryptStorage(path string) []RestoreStorageVolume {
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 0 || info.Size() > MaxUploadBytes {
		return nil // let the bounded envelope reader report invalid input
	}
	encoded := min(uint64(info.Size()), uint64(maxEncodedPayloadBytes))
	ciphertext := min(encoded*3/4, uint64(maxCiphertextBytes))
	return s.estimateRestoreStorage(restoreStorageNeed{
		directory: s.deps.TempDir(), role: restoreStoragePreparation, bytes: encoded + ciphertext,
	})
}

func (s *Service) archiveRestoreStorage(sizes map[string]uint64) ([]RestoreStorageVolume, bool) {
	var extracted uint64
	for _, size := range sizes {
		extracted += size // the bounded tar walker checked the aggregate size
	}
	databaseBytes := sizes["servers.db"]
	hostsBytes, hasHosts := sizes["known_hosts"]
	// While the extracted database is retained, preparation can hold a second
	// database and a SQLite rollback journal or WAL of comparable size.
	preparation := restoreStorageNeed{
		directory: s.deps.TempDir(), role: restoreStoragePreparation, bytes: extracted + 2*databaseBytes,
	}
	needs := append([]restoreStorageNeed{preparation},
		s.previewRestoreTargets(hasHosts).replacementNeeds(databaseBytes, hostsBytes, true)...)
	volumes := s.estimateRestoreStorage(needs...)
	// Verification can usually complete even if replacement would run out of
	// space. If there is not even room to extract, return the estimate alone.
	temp := volumes[0].AvailableBytes
	return volumes, temp == nil || *temp >= extracted
}

func (r VerifyResult) withStorageFailure(err error) VerifyResult {
	var storage *RestoreStorageError
	message := "Preparing the archive ran out of storage space. Free space on the preparation volume before retrying."
	if errors.As(err, &storage) {
		r.Storage = storage.Volumes
		message = storage.Error()
	} else {
		for i := range r.Storage {
			volume := &r.Storage[i]
			if slices.Contains(volume.Roles, restoreStoragePreparation) {
				volume.State = restoreStorageUnknown
				volume.AvailableBytes = nil
			}
		}
	}
	r.Blockers = append(r.Blockers, ReadinessIssue{Code: "insufficient_storage", Message: message})
	r.RestoreReady = false
	return r
}
