package privatefile

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

func IsPrivate(file *os.File) bool {
	handle := windows.Handle(file.Fd())
	var filesystemFlags uint32
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &filesystemFlags, nil, 0); err != nil ||
		filesystemFlags&windows.FILE_PERSISTENT_ACLS == 0 {
		return false
	}
	sd, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT,
		windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return false
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return false
	}
	trusted := func(sid *windows.SID) bool {
		return sid != nil && (sid.Equals(user.User.Sid) || sid.IsWellKnown(windows.WinLocalSystemSid) ||
			sid.IsWellKnown(windows.WinBuiltinAdministratorsSid))
	}
	owner, _, err := sd.Owner()
	if err != nil || !trusted(owner) {
		return false
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return false
	}
	for i := range uint32(dacl.AceCount) {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			return false
		}
		switch ace.Header.AceType {
		case windows.ACCESS_DENIED_ACE_TYPE:
			continue
		case windows.ACCESS_ALLOWED_ACE_TYPE:
			if ace.Mask == 0 || trusted((*windows.SID)(unsafe.Pointer(&ace.SidStart))) {
				continue
			}
		}
		return false
	}
	return true
}

func privateAttributes(directory bool) (*windows.SecurityAttributes, error) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, err
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	allow := "(A;" + inherit + ";FA;;;"
	dacl := "D:P" + allow + user.User.Sid.String() + ")" + allow + "SY)" + allow + "BA)"
	sd, err := windows.SecurityDescriptorFromString(dacl)
	if err != nil {
		return nil, err
	}
	return &windows.SecurityAttributes{Length: uint32(unsafe.Sizeof(windows.SecurityAttributes{})), SecurityDescriptor: sd}, nil
}

func CreateTemp(dir, pattern string) (*os.File, error) {
	attrs, err := privateAttributes(false)
	if err != nil {
		return nil, err
	}
	var handle windows.Handle
	path, err := createTemp(dir, pattern, func(name *uint16) error {
		var openErr error
		handle, openErr = windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, attrs,
			windows.CREATE_NEW, windows.FILE_ATTRIBUTE_NORMAL, 0)
		return openErr
	})
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(handle), path)
	if !IsPrivate(file) {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, errors.New("could not establish private access to the temporary file")
	}
	return file, nil
}

func MkdirTemp(dir, pattern string) (string, error) {
	attrs, err := privateAttributes(true)
	if err != nil {
		return "", err
	}
	path, err := createTemp(dir, pattern, func(name *uint16) error { return windows.CreateDirectory(name, attrs) })
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || !IsPrivatePath(path, info) {
		_ = os.Remove(path)
		return "", errors.New("could not establish private access to the temporary directory")
	}
	return path, nil
}

func createTemp(dir, pattern string, create func(*uint16) error) (string, error) {
	if strings.ContainsAny(pattern, `/\`) {
		return "", errors.New("temporary name contains a path separator")
	}
	if dir == "" {
		dir = os.TempDir()
	}
	prefix, suffix := pattern, ""
	if star := strings.LastIndexByte(pattern, '*'); star >= 0 {
		prefix, suffix = pattern[:star], pattern[star+1:]
	}
	for range 32 {
		var random [8]byte
		_, _ = rand.Read(random[:])
		path := filepath.Join(dir, prefix+strconv.FormatUint(binary.LittleEndian.Uint64(random[:]), 10)+suffix)
		name, err := WindowsPath(path)
		if err != nil {
			return "", err
		}
		if err := create(name); !errors.Is(err, os.ErrExist) {
			if err != nil {
				return "", &os.PathError{Op: "create private temporary entry", Path: path, Err: err}
			}
			return path, nil
		}
	}
	return "", os.ErrExist
}

// WindowsPath converts a filesystem path into the long-path form for native calls.
func WindowsPath(path string) (*uint16, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	switch {
	case strings.HasPrefix(absolute, `\\?\`):
	case strings.HasPrefix(absolute, `\\`):
		absolute = `\\?\UNC\` + absolute[2:]
	default:
		absolute = `\\?\` + absolute
	}
	return windows.UTF16PtrFromString(absolute)
}
