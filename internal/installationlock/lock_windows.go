//go:build windows

package installationlock

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func linkedDatabase(path string, _ os.FileInfo) bool {
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	var info windows.ByHandleFileInformation
	return windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info) != nil || info.NumberOfLinks != 1
}
func openLockFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return nil, errors.New("installation lock must be a regular file without links")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(windows.Handle(f.Fd()), &info); err != nil || info.NumberOfLinks != 1 {
		_ = f.Close()
		return nil, errors.New("installation lock must not have hard links")
	}
	return f, nil
}
func lockFile(f *os.File) error {
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &windows.Overlapped{})
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return ErrBusy
	}
	return err
}
