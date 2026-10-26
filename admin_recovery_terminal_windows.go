//go:build windows

package main

import (
	"golang.org/x/sys/windows"
	"os"
)

func flushRecoveryInput(input *os.File) {
	_ = windows.FlushConsoleInputBuffer(windows.Handle(input.Fd()))
}
