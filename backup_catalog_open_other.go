//go:build !unix

package main

import "os"

func openBackupCatalogFile(root *os.Root, name string) (*os.File, error) { return root.Open(name) }
func backupCatalogFileIdentity(info os.FileInfo) string                  { return "" }
