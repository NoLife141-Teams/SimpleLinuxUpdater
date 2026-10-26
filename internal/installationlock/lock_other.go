//go:build !linux && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly && !windows

package installationlock

import (
	"errors"
	"os"
)

func linkedDatabase(string, os.FileInfo) bool { return false }
func openLockFile(string) (*os.File, error) {
	return nil, errors.New("installation locking is unsupported on this operating system")
}
func lockFile(*os.File) error {
	return errors.New("installation locking is unsupported on this operating system")
}
