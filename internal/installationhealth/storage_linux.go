//go:build linux

package installationhealth

import (
	"math"

	"golang.org/x/sys/unix"
)

func FilesystemCapacity(path string) VolumeCapacity {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return VolumeCapacity{}
	}
	defer unix.Close(fd)
	var info unix.Stat_t
	if unix.Fstat(fd, &info) != nil {
		return VolumeCapacity{}
	}
	volume := VolumeCapacity{Device: uint64(info.Dev), Identified: true}
	var stat unix.Statfs_t
	if unix.Fstatfs(fd, &stat) != nil {
		return volume
	}
	fragmentSize := stat.Frsize
	if fragmentSize <= 0 {
		fragmentSize = stat.Bsize
	}
	if fragmentSize <= 0 {
		return volume
	}
	blockSize := uint64(fragmentSize)
	available := uint64(math.MaxUint64)
	if stat.Bavail <= available/blockSize {
		available = stat.Bavail * blockSize
	}
	volume.AvailableBytes = &available
	return volume
}
