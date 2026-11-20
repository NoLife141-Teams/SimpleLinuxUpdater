//go:build !windows

package backup

import "os"

func openRestoreSyncPath(path string) (*os.File, error) { return os.Open(path) }

func renameRestoreFile(from, to string) error { return os.Rename(from, to) }

func publishRestoreMarker(from, to string) error { return os.Link(from, to) }
