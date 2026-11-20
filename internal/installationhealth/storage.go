package installationhealth

// VolumeCapacity identifies a filesystem device and reports bytes available to
// an unprivileged caller. A probe can identify a device without knowing its capacity.
type VolumeCapacity struct {
	Device         uint64
	Identified     bool
	AvailableBytes *uint64
}
