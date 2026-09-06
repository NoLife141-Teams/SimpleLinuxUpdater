package main

import (
	"fmt"
	"log"
	"os"
	"strconv"

	"debian-updater/internal/persistenceowner"
)

func run(args []string, getenv func(string) string) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: persistence-owner <uid> <gid>")
	}
	uid, err := strconv.Atoi(args[1])
	if err != nil || uid < 0 {
		return fmt.Errorf("invalid uid %q", args[1])
	}
	gid, err := strconv.Atoi(args[2])
	if err != nil || gid < 0 {
		return fmt.Errorf("invalid gid %q", args[2])
	}
	return persistenceowner.Repair(persistenceowner.Config{
		UID:        uid,
		GID:        gid,
		DBPath:     getenv("DEBIAN_UPDATER_DB_PATH"),
		KnownHosts: getenv("DEBIAN_UPDATER_KNOWN_HOSTS"),
	})
}

func main() {
	if err := run(os.Args, os.Getenv); err != nil {
		log.Fatalf("persistence ownership repair failed: %v", err)
	}
}
