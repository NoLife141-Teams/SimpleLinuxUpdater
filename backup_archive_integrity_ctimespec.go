//go:build darwin || freebsd || netbsd

package main

import (
	"os"
	"syscall"
	"time"
)

func backupIntegrityChangeTime(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(stat.Ctimespec.Sec), int64(stat.Ctimespec.Nsec))
	}
	return time.Time{}
}
