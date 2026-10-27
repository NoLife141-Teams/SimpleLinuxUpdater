//go:build linux

package main

import "golang.org/x/sys/unix"

func installationAvailableBytes(path string) *uint64 {
	var stat unix.Statfs_t
	if unix.Statfs(path, &stat) != nil {
		return nil
	}
	n := stat.Bavail * uint64(stat.Bsize)
	return &n
}
