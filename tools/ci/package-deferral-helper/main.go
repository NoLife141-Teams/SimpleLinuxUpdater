// Print the production helper for the disposable real-APT qualification.
package main

import (
	"fmt"

	"debian-updater/internal/updates"
)

func main() { fmt.Print(updates.RootHelperScript()) }
