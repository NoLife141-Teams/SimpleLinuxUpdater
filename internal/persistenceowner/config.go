package persistenceowner

// Config describes the persistence paths and numeric ownership that must be
// repaired before the application drops root privileges in the Docker image.
type Config struct {
	UID        int
	GID        int
	DBPath     string
	KnownHosts string
	WorkingDir string
	DataRoot   string
}
