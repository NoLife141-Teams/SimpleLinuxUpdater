package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

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
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(fs.syncFile(file), file.Close())
}

func (fs restoreFiles) publishRecoveryMarker(path, snapshotDir string) (created bool, err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return false, err
	}
	if _, err := fmt.Fprintln(file, filepath.Base(snapshotDir)); err != nil {
		return true, errors.Join(err, file.Close())
	}
	if err := errors.Join(fs.syncFile(file), file.Close()); err != nil {
		return true, err
	}
	return true, fs.syncPath(filepath.Dir(path))
}

func (fs restoreFiles) removeFile(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return fs.syncPath(filepath.Dir(path))
}

func (fs restoreFiles) removeSQLiteSidecars(dbPath string) error {
	if err := RemoveSQLiteSidecars(dbPath); err != nil {
		return err
	}
	return fs.syncPath(filepath.Dir(dbPath))
}

// Record missing ancestors before the existing private-directory helper
// creates them. Their parent entries also have to survive a power loss.
func missingRestoreDirectories(path string) ([]string, error) {
	var dirs []string
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		_, err := os.Stat(dir)
		if err == nil {
			return dirs, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(dir) == dir {
			return nil, err
		}
		dirs = append(dirs, dir)
	}
}
