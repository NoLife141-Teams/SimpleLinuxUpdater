package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"debian-updater/internal/privatefile"
)

const restoreSealVersion = 1

type restoreRecoveryMarker struct {
	ID              string `json:"id"`
	ManifestSHA256  string `json:"manifest_sha256,omitempty"`
	InstallationMAC string `json:"installation_mac,omitempty"`
}

type sealedRecoveryManifest struct {
	Version            int                   `json:"version"`
	SetID              string                `json:"set_id"`
	ReplacesKnownHosts bool                  `json:"replaces_known_hosts"`
	Snapshots          []fileRestoreSnapshot `json:"snapshots"`
}

func recoveryDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (marker restoreRecoveryMarker) digest() string {
	return recoveryDigest([]byte(marker.ID + "/" + marker.ManifestSHA256 + "/" + marker.InstallationMAC))
}

// ClearFinalizedRestoreMarker removes only the marker bound to the durable
// finalization proof in active SQLite. The caller owns the installation lock.
func ClearFinalizedRestoreMarker(dbPath, digest string) (bool, error) {
	path := RestoreRecoveryMarkerPath(dbPath)
	marker, err := readRestoreRecoveryMarker(path)
	if err != nil || marker.digest() != digest {
		return false, nil
	}
	files := restoreFiles{}
	if err := files.removeFile(path); err != nil {
		_, _ = files.publishRecoveryMarker(path, marker)
		return false, err
	}
	return true, nil
}

func (marker restoreRecoveryMarker) installationMAC(key []byte) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = io.WriteString(mac, "simplelinuxupdater/restore-recovery/v1/"+marker.ID+"/"+marker.ManifestSHA256)
	return mac.Sum(nil)
}

func (marker restoreRecoveryMarker) matchesInstallation(key []byte) bool {
	mac, err := hex.DecodeString(marker.InstallationMAC)
	return len(key) != 0 && err == nil && hmac.Equal(mac, marker.installationMAC(key))
}

func readRestoreRecoveryMarker(path string) (restoreRecoveryMarker, error) {
	data, err := readRecoveryFile(path, 512)
	if err != nil {
		return restoreRecoveryMarker{}, err
	}
	if id := strings.TrimSpace(string(data)); validRecoverySetID(id) {
		return restoreRecoveryMarker{ID: id}, nil
	}
	var marker restoreRecoveryMarker
	if err := json.Unmarshal(data, &marker); err != nil || !validRecoverySetID(marker.ID) ||
		!validRecoveryDigest(marker.ManifestSHA256) || !validRecoveryDigest(marker.InstallationMAC) {
		return restoreRecoveryMarker{}, errors.New("invalid restore recovery marker")
	}
	return marker, nil
}

func validRecoveryDigest(digest string) bool {
	_, err := hex.DecodeString(digest)
	return err == nil && len(digest) == 2*sha256.Size
}

// RestoreRecoveryMarkerPath is scoped to one installation's database. The
// marker survives independently of the SQLite file being replaced.
func RestoreRecoveryMarkerPath(dbPath string) string {
	return dbPath + ".restore-recovery"
}

func RestoreRecoveryPending(dbPath string) (bool, error) {
	if dbPath == "" {
		return false, nil
	}
	_, err := os.Lstat(RestoreRecoveryMarkerPath(dbPath))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

type restoreFiles struct {
	sync func(*os.File) error
}

func (fs restoreFiles) syncFile(file *os.File) error {
	if fs.sync != nil {
		return fs.sync(file)
	}
	return file.Sync()
}

func (fs restoreFiles) syncPath(path string) error {
	file, err := openRestoreSyncPath(path)
	if err != nil {
		return err
	}
	return errors.Join(fs.syncFile(file), file.Close())
}

func (fs restoreFiles) writeSnapshot(path string, source io.Reader) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err := copyFileBounded(file, source, MaxExtractedBytes); err != nil {
		return errors.Join(err, file.Close())
	}
	// A fresh descriptor can miss a writeback error that occurred while
	// the original writer was open.
	return errors.Join(fs.syncFile(file), file.Close())
}

func (fs restoreFiles) copySnapshot(path, sourcePath string) error {
	source, err := os.Open(sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	return fs.writeSnapshot(path, source)
}

func (fs restoreFiles) copyAndHashSnapshot(path, sourcePath string) (string, error) {
	source, err := os.Open(sourcePath)
	if err != nil {
		return "", err
	}
	defer source.Close()
	hash := sha256.New()
	if err := fs.writeSnapshot(path, io.TeeReader(source, hash)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func sameRestoreEntry(first, second string) (bool, error) {
	firstName, secondName := filepath.Base(first), filepath.Base(second)
	if !restoreNamesMayMatch(firstName, secondName) {
		return false, nil
	}
	if first == second {
		return true, nil
	}
	left, err := os.Stat(restoreEntryDirectory(first))
	if err != nil {
		return false, err
	}
	right, err := os.Stat(restoreEntryDirectory(second))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !os.SameFile(left, right) {
		return false, err
	}
	return sameRestoreName(restoreEntryDirectory(first), firstName, secondName)
}

var ErrRestoreHostTarget = errors.New("known_hosts restore target conflicts with installation files or could not be verified. Set DEBIAN_UPDATER_KNOWN_HOSTS to a separate file")

func (s *Service) validateRestoreHostTarget(database, hosts string, replacesHosts, isolationPossible bool) error {
	var reserved []string
	if replacesHosts || isolationPossible {
		reserved = append(reserved, RestoreIsolationPath(database), restoreIsolationPendingPath(database))
	}
	if replacesHosts {
		reserved = append(reserved, database, RestoreRecoveryMarkerPath(database), database+".lock")
		reserved = append(reserved, SQLiteSidecarPaths(database)...)
		if s.deps.ConfigPath != nil && s.deps.ConfigPath() != "" {
			reserved = append(reserved, s.deps.ConfigPath())
		}
	}
	if len(reserved) != 0 && !restoreHostNameUnambiguous(filepath.Base(hosts)) {
		return ErrRestoreHostTarget
	}
	for _, path := range reserved {
		same, err := sameRestoreEntry(path, hosts)
		if err != nil || same {
			return errors.Join(ErrRestoreHostTarget, err)
		}
		// Existing entries can have another filesystem name in addition
		// to the spelling used in the configuration.
		left, leftErr := os.Lstat(path)
		right, rightErr := os.Lstat(hosts)
		if (leftErr != nil && !errors.Is(leftErr, os.ErrNotExist)) ||
			(rightErr != nil && !errors.Is(rightErr, os.ErrNotExist)) {
			return errors.Join(ErrRestoreHostTarget, leftErr, rightErr)
		}
		if leftErr == nil && rightErr == nil && os.SameFile(left, right) {
			return ErrRestoreHostTarget
		}
	}
	return nil
}

// Preserve parent traversal through a symlink so directory operations and
// pathname operations refer to the same filesystem entries.
func restoreEntryDirectory(path string) string {
	dir, _ := filepath.Split(path)
	if dir == "" {
		return "."
	}
	if trimmed := strings.TrimRight(dir, string(filepath.Separator)); trimmed != filepath.VolumeName(dir) {
		return trimmed
	}
	return filepath.VolumeName(dir) + string(filepath.Separator)
}

func (fs restoreFiles) publishRecoveryMarker(path string, marker restoreRecoveryMarker) (created bool, err error) {
	dir := restoreEntryDirectory(path)
	file, err := privatefile.CreateTemp(dir, ".slu-restore-recovery-*")
	if err != nil {
		return false, err
	}
	defer os.Remove(file.Name())
	if marker.ManifestSHA256 == "" {
		_, err = fmt.Fprintln(file, marker.ID)
	} else {
		err = json.NewEncoder(file).Encode(marker)
	}
	if err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := errors.Join(fs.syncFile(file), file.Close()); err != nil {
		return false, err
	}
	// Make the complete marker visible atomically while preserving any marker
	// from an earlier interruption.
	if err := publishRestoreMarker(file.Name(), path); err != nil {
		return false, err
	}
	removeErr := os.Remove(file.Name())
	if errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil // On Windows publication moves the prepared marker.
	}
	return true, errors.Join(removeErr, fs.syncPath(dir))
}

func (fs restoreFiles) removeFile(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return fs.syncPath(restoreEntryDirectory(path))
}

func (fs restoreFiles) removeSQLiteSidecars(dbPath string) error {
	if err := RemoveSQLiteSidecars(dbPath); err != nil {
		return err
	}
	return fs.syncPath(restoreEntryDirectory(dbPath))
}

// Record missing ancestors before the existing private-directory helper
// creates them. Their parent entries also have to survive a power loss.
func missingRestoreDirectories(path string) ([]string, error) {
	var dirs []string
	for dir := restoreEntryDirectory(path); ; dir = restoreEntryDirectory(dir) {
		_, err := os.Stat(dir)
		if err == nil {
			return dirs, nil
		}
		if !errors.Is(err, os.ErrNotExist) || restoreEntryDirectory(dir) == dir {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
}
