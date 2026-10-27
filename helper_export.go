package main

import (
	"fmt"
	"io"

	"debian-updater/internal/updates"
)

const helperUsage = `Usage: webserver helper export --user SSH_USER
Exports a root installation script to stdout. Review it, transfer it to the
target, then explicitly run sudo /bin/sh install-helper.sh on that target.
No installation configuration, database, SSH connection or local installer is used.
Exit codes: 0 success/help, 2 invalid arguments, 1 output error.
`

func runHelperCommand(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 || args[0] != "helper" {
		return false, 0
	}
	if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") || len(args) == 3 && args[1] == "export" && (args[2] == "--help" || args[2] == "-h") {
		if _, err := io.WriteString(stdout, helperUsage); err != nil {
			return true, 1
		}
		return true, 0
	}
	if len(args) != 4 || args[1] != "export" || args[2] != "--user" || !updates.IsValidSudoersUser(args[3]) {
		fmt.Fprint(stderr, helperUsage)
		return true, 2
	}
	script, err := updates.RootHelperInstallScript(args[3])
	if err != nil {
		fmt.Fprintln(stderr, "Cannot export helper:", err)
		return true, 2
	}
	if _, err := io.WriteString(stdout, script); err != nil {
		fmt.Fprintln(stderr, "Cannot write helper export:", err)
		return true, 1
	}
	return true, 0
}
