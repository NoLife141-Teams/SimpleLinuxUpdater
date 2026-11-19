package backup

import (
	"errors"
	"fmt"
	"io"
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

func sameRestoreEntry(first, second string) (bool, error) {
	if filepath.Base(first) != filepath.Base(second) {
		return false, nil
	}
	if filepath.Clean(first) == filepath.Clean(second) {
		return true, nil
	}
	left, err := os.Stat(filepath.Dir(first))
	if err != nil {
		return false, err
	}
	right, err := os.Stat(filepath.Dir(second))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil && os.SameFile(left, right), err
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
