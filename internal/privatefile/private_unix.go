//go:build !windows

package privatefile

import "os"

func CreateTemp(dir, pattern string) (*os.File, error) {
	file, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return nil, err
	}
	return file, nil
}

func MkdirTemp(dir, pattern string) (string, error) {
	path, err := os.MkdirTemp(dir, pattern)
	if err != nil {
		return "", err
	}
	if err := os.Chmod(path, 0700); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return path, nil
}

func IsPrivate(file *os.File) bool {
	info, err := file.Stat()
	return err == nil && info.Mode().Perm()&0077 == 0
}
