//go:build !linux

package main

// Filesystem capacity is unavailable on platforms without the Linux collector.
func installationAvailableBytes(string) *uint64 { return nil }
