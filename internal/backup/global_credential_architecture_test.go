package backup

import (
	"os"
	"strings"
	"testing"
)

func TestBackupLifecycleDoesNotOwnGlobalSSHCredentialStorageDetails(t *testing.T) {
	source, err := os.ReadFile("backup.go")
	if err != nil {
		t.Fatalf("read backup.go: %v", err)
	}
	if strings.Contains(string(source), "global_ssh_key") {
		t.Fatal("backup.go contains the Global SSH Credential setting key; storage details belong to internal/servers")
	}
}
