//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// TIOCFLUSH uses the BSD FREAD mask, which x/sys/unix does not export on Darwin.
const recoveryFlushReadMask = 0x01

func flushRecoveryInput(input *os.File) {
	_ = unix.IoctlSetPointerInt(int(input.Fd()), unix.TIOCFLUSH, recoveryFlushReadMask)
}
