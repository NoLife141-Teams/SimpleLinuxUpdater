//go:build darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"golang.org/x/sys/unix"
	"os"
)

func flushRecoveryInput(input *os.File) {
	_ = unix.IoctlSetPointerInt(int(input.Fd()), unix.TIOCFLUSH, unix.FREAD)
}
