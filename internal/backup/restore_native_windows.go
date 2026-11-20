package backup

import (
	"os"
	"strings"
	"unsafe"

	"debian-updater/internal/privatefile"

	"golang.org/x/sys/windows"
)

func restoreNamesMayMatch(first, second string) bool {
	return strings.EqualFold(strings.TrimRight(first, " ."), strings.TrimRight(second, " ."))
}

func sameRestoreName(directory, first, second string) (bool, error) {
	if strings.TrimRight(first, " .") == strings.TrimRight(second, " .") {
		return true, nil
	}
	dir, err := os.Open(directory)
	if err != nil {
		return false, err
	}
	defer dir.Close()
	var flags uint32
	err = windows.GetFileInformationByHandleEx(windows.Handle(dir.Fd()), windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&flags)), uint32(unsafe.Sizeof(flags)))
	const caseSensitive = 1
	return err == nil && flags&caseSensitive == 0, err
}

func openRestoreSyncPath(path string) (*os.File, error) {
	name, err := privatefile.WindowsPath(path)
	if err != nil {
		return nil, err
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open for restore synchronization", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}

func renameRestoreFile(from, to string) error {
	return moveRestoreFile(from, to, windows.MOVEFILE_REPLACE_EXISTING)
}

func publishRestoreMarker(from, to string) error {
	return moveRestoreFile(from, to, 0)
}

func moveRestoreFile(from, to string, flags uint32) error {
	source, err := privatefile.WindowsPath(from)
	if err != nil {
		return err
	}
	destination, err := privatefile.WindowsPath(to)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(source, destination, flags|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "move restore file", Old: from, New: to, Err: err}
	}
	return nil
}
