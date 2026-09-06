package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func writeEntrypointTestExecutable(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatalf("write fake %s: %v", name, err)
	}
}

func entrypointTestEnv(overrides ...string) []string {
	blocked := map[string]struct{}{
		"PATH":                       {},
		"CHOWN_LOG":                  {},
		"DEBIAN_UPDATER_DB_PATH":     {},
		"DEBIAN_UPDATER_KNOWN_HOSTS": {},
	}
	env := make([]string, 0, len(os.Environ())+len(overrides))
	for _, item := range os.Environ() {
		key, _, ok := strings.Cut(item, "=")
		if ok {
			if _, skip := blocked[key]; skip {
				continue
			}
		}
		env = append(env, item)
	}
	return append(env, overrides...)
}

func runDockerEntrypointTest(t *testing.T, dbPath string, extraEnv ...string) (string, string, error) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("Docker entrypoint ownership repair is specific to the Linux/Alpine image")
	}

	fakeRoot := t.TempDir()
	fakeBin := filepath.Join(fakeRoot, "bin")
	if err := os.MkdirAll(fakeBin, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	chownLog := filepath.Join(fakeRoot, "chown.log")

	writeEntrypointTestExecutable(t, fakeBin, "id", `#!/bin/sh
if [ "${1:-}" = "-u" ]; then
    printf '0\n'
    exit 0
fi
exit 1
`)
	writeEntrypointTestExecutable(t, fakeBin, "chown", `#!/bin/sh
printf '%s\n' "$*" >> "$CHOWN_LOG"
exit 0
`)
	writeEntrypointTestExecutable(t, fakeBin, "su-exec", `#!/bin/sh
shift
if [ "$#" -eq 0 ]; then
    exit 0
fi
exec "$@"
`)

	scriptPath, err := filepath.Abs("docker-entrypoint.sh")
	if err != nil {
		t.Fatalf("resolve docker-entrypoint.sh: %v", err)
	}
	cmd := exec.Command("sh", scriptPath, "true")
	cmd.Dir = fakeRoot
	env := []string{
		"PATH=" + fakeBin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"CHOWN_LOG=" + chownLog,
		"DEBIAN_UPDATER_DB_PATH=" + dbPath,
	}
	env = append(env, extraEnv...)
	cmd.Env = entrypointTestEnv(env...)
	output, runErr := cmd.CombinedOutput()

	logBytes, readErr := os.ReadFile(chownLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		t.Fatalf("read chown log: %v", readErr)
	}
	return string(output), string(logBytes), runErr
}

func TestDockerEntrypointRejectsSymlinkPersistenceFile(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "servers.db")
	if err := os.Symlink(sentinel, dbPath); err != nil {
		t.Fatal(err)
	}

	output, chownLog, err := runDockerEntrypointTest(t, dbPath)
	if err == nil {
		t.Fatal("docker-entrypoint.sh succeeded with a symlink DB, want safe startup failure")
	}
	if !strings.Contains(output, "refusing symbolic link in persistence path") {
		t.Fatalf("entrypoint output = %q, want symlink refusal", output)
	}
	if strings.Contains(chownLog, dbPath) || strings.Contains(chownLog, sentinel) {
		t.Fatalf("chown log = %q, symlink or target must not be chowned", chownLog)
	}
	contents, readErr := os.ReadFile(sentinel)
	if readErr != nil || string(contents) != "unchanged" {
		t.Fatalf("sentinel after rejected startup = %q, %v", contents, readErr)
	}
}

func TestDockerEntrypointRejectsSymlinkDirectoryComponent(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real-data")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(root, "linked-data")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(linkedDir, "servers.db")

	output, chownLog, err := runDockerEntrypointTest(t, dbPath)
	if err == nil {
		t.Fatal("docker-entrypoint.sh succeeded through a symlink directory, want safe startup failure")
	}
	if !strings.Contains(output, "refusing symbolic link in persistence path") {
		t.Fatalf("entrypoint output = %q, want symlink refusal", output)
	}
	if strings.Contains(chownLog, linkedDir) || strings.Contains(chownLog, realDir) {
		t.Fatalf("chown log = %q, symlinked directory must not be chowned", chownLog)
	}
}

func TestDockerEntrypointRejectsFilesystemRootOwnership(t *testing.T) {
	output, chownLog, err := runDockerEntrypointTest(t, "/servers.db")
	if err == nil {
		t.Fatal("docker-entrypoint.sh succeeded with DB in filesystem root, want safe startup failure")
	}
	if !strings.Contains(output, "refusing to change ownership of filesystem root") {
		t.Fatalf("entrypoint output = %q, want filesystem-root refusal", output)
	}
	for _, line := range strings.Split(chownLog, "\n") {
		if line == "-h app:app /" {
			t.Fatalf("chown log = %q, filesystem root must never be chowned", chownLog)
		}
	}
}

func TestDockerEntrypointNormalizesDatabasePathLikeApplication(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "servers.db")
	if err := os.WriteFile(dbPath, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}

	output, chownLog, err := runDockerEntrypointTest(t, " \t"+dbPath+"  ")
	if err != nil {
		t.Fatalf("docker-entrypoint.sh error = %v, output = %q", err, output)
	}
	if !strings.Contains(chownLog, "-h app:app "+dbPath) {
		t.Fatalf("chown log = %q, want normalized DB path %q", chownLog, dbPath)
	}
	if strings.Contains(chownLog, " \t"+dbPath) {
		t.Fatalf("chown log = %q, raw whitespace-padded DB path was used", chownLog)
	}
}

func TestDockerEntrypointRepairsNestedDataDirectoryAncestors(t *testing.T) {
	output, chownLog, err := runDockerEntrypointTest(t, "data/tenant/state/servers.db")
	if err != nil {
		t.Fatalf("docker-entrypoint.sh error = %v, output = %q", err, output)
	}
	for _, dir := range []string{"data", "data/tenant", "data/tenant/state"} {
		if !strings.Contains(chownLog, "-h app:app "+dir) {
			t.Fatalf("chown log = %q, want ancestor ownership repair for %q", chownLog, dir)
		}
	}
	if strings.Contains(chownLog, "-R") {
		t.Fatalf("chown log = %q, recursive ownership repair is forbidden", chownLog)
	}
}

func TestDockerEntrypointRepairsConfiguredKnownHostsWriteTarget(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "data")
	sshDir := filepath.Join(dbDir, "ssh")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "servers.db")
	knownHostsPath := filepath.Join(sshDir, "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte("host key"), 0o600); err != nil {
		t.Fatal(err)
	}

	rawKnownHosts := "  " + knownHostsPath + " : /etc/ssh/ssh_known_hosts  "
	output, chownLog, err := runDockerEntrypointTest(t, dbPath, "DEBIAN_UPDATER_KNOWN_HOSTS="+rawKnownHosts)
	if err != nil {
		t.Fatalf("docker-entrypoint.sh error = %v, output = %q", err, output)
	}
	for _, path := range []string{sshDir, knownHostsPath} {
		if !strings.Contains(chownLog, "-h app:app "+path) {
			t.Fatalf("chown log = %q, want configured known-hosts ownership repair for %q", chownLog, path)
		}
	}
	if strings.Contains(chownLog, "/etc/ssh/ssh_known_hosts") {
		t.Fatalf("chown log = %q, read-only secondary known-hosts path must not be chowned", chownLog)
	}
}

func TestDockerEntrypointDoesNotChownKnownHostsOutsidePersistenceArea(t *testing.T) {
	root := t.TempDir()
	dbDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dbDir, "servers.db")

	output, chownLog, err := runDockerEntrypointTest(t, dbPath, "DEBIAN_UPDATER_KNOWN_HOSTS=/etc/ssh/ssh_known_hosts")
	if err != nil {
		t.Fatalf("docker-entrypoint.sh error = %v, output = %q", err, output)
	}
	if strings.Contains(chownLog, "/etc/ssh") {
		t.Fatalf("chown log = %q, configured system known-hosts paths must remain untouched", chownLog)
	}
}

func TestDockerEntrypointChownsOnlyKnownRegularPersistencePaths(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dataDir, "servers.db")
	paths := []string{
		dbPath,
		dbPath + "-wal",
		dbPath + "-shm",
		filepath.Join(dataDir, "config.json"),
		filepath.Join(dataDir, "known_hosts"),
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("test"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	output, chownLog, err := runDockerEntrypointTest(t, dbPath)
	if err != nil {
		t.Fatalf("docker-entrypoint.sh error = %v, output = %q", err, output)
	}
	for _, path := range append([]string{dataDir}, paths...) {
		want := "-h app:app " + path
		if !strings.Contains(chownLog, want) {
			t.Fatalf("chown log = %q, want %q", chownLog, want)
		}
	}
	if strings.Contains(chownLog, "-R") {
		t.Fatalf("chown log = %q, recursive ownership repair is forbidden", chownLog)
	}
}

func TestDockerEntrypointSourceHasNoRecursiveDataChown(t *testing.T) {
	contents, err := os.ReadFile("docker-entrypoint.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(contents)
	if strings.Contains(script, "chown -R") {
		t.Fatal("docker-entrypoint.sh must not recursively chown app-writable persistence trees")
	}
	if !strings.Contains(script, `chown_regular_file_if_present "/data/servers.json"`) {
		t.Fatal("docker-entrypoint.sh must preserve legacy /data/servers.json ownership repair")
	}
	if !strings.Contains(script, `DEBIAN_UPDATER_KNOWN_HOSTS`) {
		t.Fatal("docker-entrypoint.sh must account for the configured known-hosts write target")
	}
}
