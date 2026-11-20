//go:build aix || dragonfly || linux || openbsd || solaris

package main

import (
	"os"
	"syscall"
	"time"
)

func backupIntegrityChangeTime(info os.FileInfo) time.Time {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return time.Unix(int64(stat.Ctim.Sec), int64(stat.Ctim.Nsec))
	}
	return time.Time{}
}
