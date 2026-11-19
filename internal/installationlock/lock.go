// Package installationlock coordinates processes using one persisted installation.
package installationlock

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrBusy = errors.New("installation is already in use; stop the server and other recovery commands first")

// Lock owns a stable sidecar file. It must not be unlinked on release: a restore
// replaces the database inode, while every process must continue to lock this inode.
type Lock struct{ file *os.File }

func Acquire(database string, createDirectory bool) (*Lock, error) {
	if createDirectory {
		if err := os.MkdirAll(databaseDirectory(database), 0700); err != nil {
			return nil, fmt.Errorf("create installation directory: %w", err)
		}
	}
	database, err := ResolveDatabasePath(database)
	if err != nil {
		return nil, fmt.Errorf("resolve installation directory: %w", err)
	}
	if info, statErr := os.Lstat(database); statErr == nil {
		if !info.Mode().IsRegular() || linkedDatabase(database, info) {
			return nil, errors.New("database must be a regular file without symbolic or hard links")
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect database: %w", statErr)
	}
	f, err := openLockFile(database + ".lock")
	if err != nil {
		return nil, fmt.Errorf("open installation lock: %w", err)
	}
	if err = lockFile(f); err != nil {
		_ = f.Close()
		return nil, err
	}
	return &Lock{file: f}, nil
}

// ResolveDatabasePath resolves the original parent before a prefix such as
// link/.. can be collapsed into a different installation directory. The final
// database entry is kept so callers can reject symlinks at that entry.
func ResolveDatabasePath(database string) (string, error) {
	dir, err := filepath.EvalSymlinks(databaseDirectory(database))
	if err != nil {
		return "", err
	}
	_, name := filepath.Split(database)
	return filepath.Abs(filepath.Join(dir, name))
}

func databaseDirectory(database string) string {
	dir, _ := filepath.Split(database)
	if dir == "" {
		return "."
	}
	return dir
}

func (l *Lock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	f := l.file
	l.file = nil
	return f.Close()
}
