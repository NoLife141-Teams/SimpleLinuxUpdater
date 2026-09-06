//go:build !linux

package persistenceowner

import "fmt"

func Repair(Config) error {
	return fmt.Errorf("persistence ownership repair is supported only on linux")
}
