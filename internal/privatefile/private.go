package privatefile

import "os"

// IsPrivatePath checks access rights on the expected entry after opening it.
func IsPrivatePath(path string, expected os.FileInfo) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	current, err := file.Stat()
	return err == nil && os.SameFile(current, expected) && IsPrivate(file)
}
