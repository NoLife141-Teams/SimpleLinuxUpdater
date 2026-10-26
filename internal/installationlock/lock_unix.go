//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package installationlock

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"syscall"
)

func linkedFile(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink != 1
}
func linkedDatabase(_ string, info os.FileInfo) bool { return linkedFile(info) }

func openLockFile(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || linkedFile(info) {
		_ = f.Close()
		return nil, errors.New("installation lock must be a regular file without links")
	}
	return f, nil
}
func lockFile(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
		return ErrBusy
	}
	return err
}
