//go:build !linux && !windows && !darwin && !freebsd && !netbsd && !openbsd && !dragonfly

package main

import "os"

func flushRecoveryInput(*os.File) {}
