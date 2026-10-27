// Developer-only fixture data for the owned-host distribution qualification.
package main

import (
	"encoding/json"
	"os"

	"debian-updater/internal/helperprotocol"
	"debian-updater/internal/updates"
)

func main() {
	sudoers, err := updates.ManagedSudoersContent("operator")
	if err != nil {
		panic(err)
	}
	legacy, err := updates.LegacyAptSudoersContent("operator")
	if err != nil {
		panic(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(map[string]string{
		"helper": updates.RootHelperScript(), "sudoers": sudoers, "legacy": legacy,
		"required_command": helperprotocol.RequiredCommand, "capabilities": helperprotocol.Capabilities,
	}); err != nil {
		panic(err)
	}
}
