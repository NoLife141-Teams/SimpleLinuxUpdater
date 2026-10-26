package main

import (
	"bytes"
	"database/sql"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/auth"
	"debian-updater/internal/installationlock"
)

func prepareRecoveryCommandDB(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "servers.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err = ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	s := auth.NewService(auth.ServiceOptions{DB: func() *sql.DB { return db }})
	if err = s.CreateInitialUser("admin", "OldPassword123!"); err != nil {
		t.Fatal(err)
	}
	return path
}

func commandRecovery(t *testing.T, args []string, input string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	var output bytes.Buffer
	handled, err := runAdminCommand(args, r, &output)
	if !handled {
		t.Fatal("admin command not handled")
	}
	return output.String(), err
}

func TestRecoveryCommandValidatesInputWithoutStartingRuntime(t *testing.T) {
	path := prepareRecoveryCommandDB(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
	base := []string{"admin", "reset-password", "--username", "admin", "--password-stdin"}
	for _, tc := range []struct {
		name  string
		args  []string
		input string
	}{
		{"weak", base, "weak\nweak\n"}, {"mismatch", base, "NewPassword456!\nOtherPassword789!\n"},
		{"incomplete", base, "NewPassword456!\n"}, {"oversize", base, strings.Repeat("x", 257) + "\nconfirm\n"},
		{"argv password", append(append([]string{}, base...), "--password", "ArgumentSecret123!"), ""},
		{"missing account", []string{"admin", "reset-password", "--password-stdin"}, ""},
		{"non terminal", []string{"admin", "reset-password", "--username", "admin"}, "NewPassword456!\nNewPassword456!\n"},
		{"absent", []string{"admin", "reset-password", "--username", "absent", "--password-stdin"}, "NewPassword456!\nNewPassword456!\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			output, err := commandRecovery(t, tc.args, tc.input)
			if err == nil {
				t.Fatal("invalid recovery succeeded")
			}
			if strings.Contains(output+err.Error(), "ArgumentSecret123!") || strings.Contains(output+err.Error(), "NewPassword456!") {
				t.Fatal("secret leaked")
			}
		})
	}
	output, err := commandRecovery(t, base, "NewPassword456!\nNewPassword456!\n")
	if err != nil || !strings.Contains(output, "committed") || strings.Contains(output, "NewPassword456!") {
		t.Fatal(output, err)
	}
	db, err := openRecoveryDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := auth.NewService(auth.ServiceOptions{DB: func() *sql.DB { return db }})
	if ok, err := s.Authenticate("admin", "NewPassword456!"); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if setup, err := s.SetupRequired(); err != nil || setup {
		t.Fatal(setup, err)
	}
}

func TestRecoveryCommandDoesNotCreateOrMigrateInvalidDatabase(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "missing.db")
	if _, err := openRecoveryDatabase(path); err == nil {
		t.Fatal("missing DB accepted")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing DB created", err)
	}
	path = filepath.Join(dir, "invalid.db")
	original := []byte("not sqlite")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := openRecoveryDatabase(path); err == nil {
		t.Fatal("invalid DB accepted")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(original, after) {
		t.Fatal("invalid DB changed")
	}
}

func TestRecoveryDatabaseDSNKeepsDriveLettersInPath(t *testing.T) {
	for _, tc := range []struct{ path, wantPath string }{
		{"C:/data/servers.db", "/C:/data/servers.db"},
		{"D:/private data/servers#1%.db", "/D:/private data/servers#1%.db"},
		{"/var/lib/simplelinuxupdater/servers.db", "/var/lib/simplelinuxupdater/servers.db"},
		{"//server/share/servers.db", "//server/share/servers.db"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			u, err := url.Parse(recoveryDatabaseDSN(tc.path))
			if err != nil {
				t.Fatal(err)
			}
			if u.Scheme != "file" || u.Host != "" || u.Path != tc.wantPath || u.Fragment != "" {
				t.Fatalf("recovery URI = %+v, want a local file URI for %q", u, tc.wantPath)
			}
			if u.Query().Get("mode") != "rw" || u.Query().Get("_pragma") != "busy_timeout(5000)" {
				t.Fatalf("recovery URI lost its existing-database/write timeout settings: %s", u)
			}
		})
	}
}

func TestRecoveryCommandRejectsUnwritableInstallation(t *testing.T) {
	path := prepareRecoveryCommandDB(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0600)
	// Privileged OS accounts can bypass mode bits; exercise permission rejection
	// where the OS actually denies opening this database for write.
	probe, err := os.OpenFile(path, os.O_RDWR, 0)
	if err == nil {
		_ = probe.Close()
		t.Skip("OS account bypasses read-only file mode")
	}
	output, err := commandRecovery(t, []string{"admin", "reset-password", "--username", "admin", "--password-stdin"}, "NewPassword456!\nNewPassword456!\n")
	if err == nil || strings.Contains(output, "committed") {
		t.Fatalf("unwritable reset succeeded: %s %v", output, err)
	}
	if strings.Contains(output+err.Error(), "NewPassword456!") {
		t.Fatal("permission failure leaked secret")
	}
}

type failingRecoveryOutput struct{}

func (failingRecoveryOutput) Write([]byte) (int, error) { return 0, errors.New("output unavailable") }

func TestRecoveryCommandReportsCommittedResetWhenConfirmationOutputFails(t *testing.T) {
	path := prepareRecoveryCommandDB(t)
	t.Setenv("DEBIAN_UPDATER_DB_PATH", path)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_, _ = w.WriteString("NewPassword456!\nNewPassword456!\n")
	_ = w.Close()
	_, err = runAdminCommand([]string{"admin", "reset-password", "--username", "admin", "--password-stdin"}, r, failingRecoveryOutput{})
	if err == nil || !strings.Contains(err.Error(), "recovery committed") {
		t.Fatal(err)
	}
	db, err := openRecoveryDatabase(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := auth.NewService(auth.ServiceOptions{DB: func() *sql.DB { return db }})
	if ok, err := s.Authenticate("admin", "NewPassword456!"); err != nil || !ok {
		t.Fatal(ok, err)
	}
}

func TestRecoveryCommandProcessHelper(t *testing.T) {
	if os.Getenv("SLU_TEST_RECOVERY_PROCESS") != "1" {
		return
	}
	os.Args = []string{os.Args[0], "admin", "reset-password", "--username", "admin", "--password-stdin"}
	if os.Getenv("SLU_TEST_RECOVERY_TTY") == "1" {
		os.Args = os.Args[:len(os.Args)-1]
	}
	main()
	os.Exit(0)
}

func TestRecoveryTerminalHidesSecretsAndRestoresAfterSignals(t *testing.T) {
	if os.PathSeparator != '/' {
		t.Skip("PTY harness requires Unix")
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for PTY integration")
	}
	path := prepareRecoveryCommandDB(t)
	cmd := exec.Command(python, "tools/ci/test-admin-recovery-tty.py", os.Args[0], path)
	cmd.Env = append(os.Environ(), "SLU_TEST_RECOVERY_PROCESS=1", "SLU_TEST_RECOVERY_TTY=1", "DEBIAN_UPDATER_DB_PATH="+path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("PTY validation: %v\n%s", err, out)
	}
}

func TestRecoveryCommandExcludesConcurrentProcessAndDispatchesBeforeConfiguration(t *testing.T) {
	path := prepareRecoveryCommandDB(t)
	child := func() (*exec.Cmd, io.WriteCloser, *bytes.Buffer) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRecoveryCommandProcessHelper$")
		cmd.Env = append(os.Environ(), "SLU_TEST_RECOVERY_PROCESS=1", "DEBIAN_UPDATER_DB_PATH="+path, "DEBIAN_UPDATER_LISTEN_ADDR=invalid:listen:address")
		input, err := cmd.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		output := new(bytes.Buffer)
		cmd.Stdout = output
		cmd.Stderr = output
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = input.Close()
			if cmd.ProcessState == nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
			}
		})
		return cmd, input, output
	}
	first, input, firstOutput := child()
	deadline := time.Now().Add(10 * time.Second)
	for {
		lease, err := installationlock.Acquire(path, false)
		if errors.Is(err, installationlock.ErrBusy) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		_ = lease.Close()
		if time.Now().After(deadline) {
			t.Fatal("command did not acquire installation lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
	second, secondInput, secondOutput := child()
	_ = secondInput.Close()
	if err := second.Wait(); err == nil || !strings.Contains(secondOutput.String(), "already in use") {
		t.Fatal(secondOutput.String(), err)
	}
	if _, err := io.WriteString(input, "NewPassword456!\nNewPassword456!\n"); err != nil {
		t.Fatal(err)
	}
	_ = input.Close()
	if err := first.Wait(); err != nil || !strings.Contains(firstOutput.String(), "committed") {
		t.Fatal(firstOutput.String(), err)
	}
	if strings.Contains(firstOutput.String()+secondOutput.String(), "NewPassword456!") {
		t.Fatal("password leaked to process output")
	}
}
