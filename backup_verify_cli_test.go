package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOfflineArchiveVerifyReportsIncompatibilityWithoutLeakingEnvelope(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "fixture.slubkp")
	const untrustedName = "untrusted-format-canary"
	if err := os.WriteFile(archive, []byte(`{"format":"`+untrustedName+`","version":9}`), 0600); err != nil {
		t.Fatal(err)
	}
	secret := filepath.Join(root, "passphrase")
	const passphrase = "public-test-passphrase-fixture"
	if err := os.WriteFile(secret, []byte(passphrase+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DEBIAN_UPDATER_DB_PATH", filepath.Join(root, "no-active-database"))
	args := []string{"backup", "verify", "--archive", archive, "--work-dir", root, "--passphrase-file", secret, "--json"}
	var output, diagnostics bytes.Buffer
	if handled, code := runOfflineArchiveVerifyCommand(args, nil, &output, &diagnostics); !handled || code != offlineVerifyIncompatible {
		t.Fatalf("verification = %v, %d, %s", handled, code, output.String())
	}
	var report offlineArchiveReport
	if err := json.Unmarshal(output.Bytes(), &report); err != nil || report.ArchiveVerified || report.TargetStatus != "not_inspected" ||
		report.Status != "incompatible_archive" {
		t.Fatalf("verification report = %+v, %v", report, err)
	}
	if diagnostics.Len() != 0 || bytes.Contains(output.Bytes(), []byte(untrustedName)) ||
		bytes.Contains(output.Bytes(), []byte(passphrase)) || bytes.Contains(output.Bytes(), []byte(root)) {
		t.Fatal("verification exposed diagnostic text, the passphrase, path, or untrusted metadata")
	}
	if err := os.Chmod(secret, 0644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if _, code := runOfflineArchiveVerifyCommand(args, nil, &output, &diagnostics); code != offlineVerifyError ||
		!bytes.Contains(output.Bytes(), []byte("passphrase_unavailable")) {
		t.Fatalf("public passphrase file accepted: %d, %s", code, output.String())
	}
}
