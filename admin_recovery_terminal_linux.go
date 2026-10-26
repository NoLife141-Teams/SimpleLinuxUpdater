//go:build linux

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func flushRecoveryInput(input *os.File) {
	_ = unix.IoctlSetInt(int(input.Fd()), unix.TCFLSH, unix.TCIFLUSH)
}
