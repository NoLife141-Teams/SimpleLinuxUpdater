//go:build !linux

package main

import (
	"os"
	"time"
)

func backupIntegrityChangeTime(os.FileInfo) time.Time { return time.Time{} }
