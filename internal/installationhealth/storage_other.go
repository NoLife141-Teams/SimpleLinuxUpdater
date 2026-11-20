//go:build !linux

package installationhealth

func FilesystemCapacity(string) VolumeCapacity { return VolumeCapacity{} }
