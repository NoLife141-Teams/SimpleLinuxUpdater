package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"debian-updater/internal/updates"
)

func TestHelperExportProductionBytesAndValidation(t *testing.T) {
	for _, user := range []string{"operator", "Admin", "ops.admin", "ops-admin_2", "123", "-ops", ".ops"} {
		var out, diagnostic bytes.Buffer
		handled, code := runHelperCommand([]string{"helper", "export", "--user", user}, &out, &diagnostic)
		want, err := updates.RootHelperInstallScript(user)
		if err != nil || !handled || code != 0 || out.String() != want || diagnostic.Len() != 0 {
			t.Fatalf("export %q: handled=%v code=%d err=%v diagnostic=%s", user, handled, code, err, &diagnostic)
		}
	}
	for _, args := range [][]string{
		{"helper"}, {"helper", "install"}, {"helper", "export"},
		{"helper", "export", "--user", "operator", "--user", "root"},
		{"helper", "export", "--password", "secret"},
	} {
		assertInvalidHelperExport(t, args)
	}
	for _, user := range []string{"", " root", "root\n", "$(touch /tmp/unsafe)", "ops;id", "ops ALL=(ALL) ALL", "ops,name", "../ops", "'", strings.Repeat("a", 65)} {
		assertInvalidHelperExport(t, []string{"helper", "export", "--user", user})
	}
	for _, args := range [][]string{{"helper", "--help"}, {"helper", "export", "--help"}} {
		var out bytes.Buffer
		if handled, code := runHelperCommand(args, &out, io.Discard); !handled || code != 0 || out.String() != helperUsage {
			t.Fatal("missing help")
		}
	}
	if handled, code := runHelperCommand([]string{"helper", "export", "--user", "operator"}, failingHelperWriter{}, io.Discard); !handled || code != 1 {
		t.Fatal("output failure is not reported")
	}
}

type failingHelperWriter struct{}

func (failingHelperWriter) Write([]byte) (int, error) { return 0, errors.New("fixture output error") }

func assertInvalidHelperExport(t *testing.T, args []string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	if handled, code := runHelperCommand(args, &out, &diagnostic); !handled || code != 2 || out.Len() != 0 || diagnostic.Len() == 0 {
		t.Fatalf("unsafe args accepted: %q", args)
	}
}

// Exercise main in a fresh process with deliberately unusable installation and
// HTTP configuration. Routing after lock/DB/listen/config would fail this test.
func TestHelperExportProcess(t *testing.T) {
	if os.Getenv("SLU_HELPER_EXPORT_CHILD") == "1" {
		os.Args = []string{"webserver", "helper", "export", "--user", "operator"}
		main()
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	command := exec.Command(executable, "-test.run=^TestHelperExportProcess$")
	command.Dir = dir
	command.Env = []string{"SLU_HELPER_EXPORT_CHILD=1", "PATH=" + os.Getenv("PATH"), "HOME=" + dir,
		"DEBIAN_UPDATER_DB_PATH=" + filepath.Join(dir, "absent", "servers.db"),
		"DEBIAN_UPDATER_LISTEN_ADDR=invalid-address", "DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED=invalid"}
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := updates.RootHelperInstallScript("operator")
	if string(output) != want {
		t.Fatal("main did not export exact production script")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("export created installation files: %v %v", files, err)
	}
}
