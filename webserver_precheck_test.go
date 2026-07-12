package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	updatespkg "debian-updater/internal/updates"

	"golang.org/x/crypto/ssh"
)

const precheckDiskSpaceCmd = "df -Pk /var / | awk 'NR>1 {print $2, $4}'"
const postcheckFailedUnitsCmd = "systemctl --failed --no-legend --plain"
const postcheckRebootRequiredCmd = "sh -c \"if [ -f /var/run/reboot-required ]; then echo required; fi\""
const serverFactsOSCmd = "sh -c '. /etc/os-release 2>/dev/null; printf \"%s\\n\" \"${PRETTY_NAME:-unknown}\"'"
const serverFactsUptimeCmd = "cat /proc/uptime"
const postcheckNameFailedUnits = updatespkg.PostcheckNameFailedUnits
const postcheckNameRebootRequired = updatespkg.PostcheckNameRebootNeeded

var precheckLocksCmd = updatespkg.RootOrSudoCommand("/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock")
var precheckDpkgAuditCmd = updatespkg.RootOrSudoCommand("dpkg --audit")
var precheckAptCheckCmd = updatespkg.RootOrSudoCommand("apt-get check")

type fakeExitStatusError struct {
	code int
	msg  string
}

func (e fakeExitStatusError) Error() string {
	if strings.TrimSpace(e.msg) != "" {
		return e.msg
	}
	return "exit status"
}

func (e fakeExitStatusError) ExitStatus() int { return e.code }

type scriptedResponse struct {
	stdout string
	stderr string
	err    error
	delay  time.Duration
}

type scriptedSSHConnection struct {
	responses         map[string]scriptedResponse
	sequenceResponses map[string][]scriptedResponse
	commandCalls      map[string]int
	commands          []string
	closed            bool
	mu                sync.Mutex
}

func (c *scriptedSSHConnection) NewSession() (sshSessionRunner, error) {
	return &scriptedSSHSession{conn: c}, nil
}

func (c *scriptedSSHConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

type scriptedSSHSession struct {
	conn   *scriptedSSHConnection
	stdout io.Writer
	stderr io.Writer
}

func (s *scriptedSSHSession) SetStdin(io.Reader) {}

func (s *scriptedSSHSession) SetStdout(w io.Writer) { s.stdout = w }

func (s *scriptedSSHSession) SetStderr(w io.Writer) { s.stderr = w }

func (s *scriptedSSHSession) Run(cmd string) error {
	s.conn.mu.Lock()
	defer s.conn.mu.Unlock()

	s.conn.commands = append(s.conn.commands, cmd)
	if seq, ok := s.conn.sequenceResponses[cmd]; ok && len(seq) > 0 {
		if s.conn.commandCalls == nil {
			s.conn.commandCalls = make(map[string]int)
		}
		idx := s.conn.commandCalls[cmd]
		s.conn.commandCalls[cmd] = idx + 1
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		resp := seq[idx]
		if resp.delay > 0 {
			time.Sleep(resp.delay)
		}
		if s.stdout != nil && resp.stdout != "" {
			_, _ = io.WriteString(s.stdout, resp.stdout)
		}
		if s.stderr != nil && resp.stderr != "" {
			_, _ = io.WriteString(s.stderr, resp.stderr)
		}
		return resp.err
	}
	resp, ok := s.conn.responses[cmd]
	if !ok {
		return errors.New("unexpected command: " + cmd)
	}
	if resp.delay > 0 {
		time.Sleep(resp.delay)
	}
	if s.stdout != nil && resp.stdout != "" {
		_, _ = io.WriteString(s.stdout, resp.stdout)
	}
	if s.stderr != nil && resp.stderr != "" {
		_, _ = io.WriteString(s.stderr, resp.stderr)
	}
	return resp.err
}

func (s *scriptedSSHSession) Close() error { return nil }

func TestRunUpdateWithActorPrecheckFailureStopsBeforeAptUpdate(t *testing.T) {
	preserveServerState(t)
	preserveDBState(t)

	dbFile := filepath.Join(t.TempDir(), "precheck.db")
	t.Setenv("DEBIAN_UPDATER_DB_PATH", dbFile)
	knownHostsPath := filepath.Join(t.TempDir(), "known_hosts")
	if err := os.WriteFile(knownHostsPath, []byte(""), 0600); err != nil {
		t.Fatalf("write known_hosts: %v", err)
	}
	t.Setenv("DEBIAN_UPDATER_KNOWN_HOSTS", knownHostsPath)

	server := Server{Name: "srv-precheck-fail", Host: "example.org", Port: 22, User: "root", Pass: "pw"}
	mu.Lock()
	servers = []Server{server}
	statusMap = map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: "idle", Upgradable: []string{}},
	}
	mu.Unlock()

	conn := &scriptedSSHConnection{
		responses: map[string]scriptedResponse{
			precheckDiskSpaceCmd: {stdout: "1024\n2048000\n"},
		},
	}
	origDial := getDialSSHConnection()
	setDialSSHConnection(func(_ Server, _ *ssh.ClientConfig) (sshConnection, error) {
		return conn, nil
	})
	t.Cleanup(func() { setDialSSHConnection(origDial) })

	runUpdateWithActor(server, "tester", "127.0.0.1", loadRetryPolicyFromEnv())

	mu.Lock()
	finalStatus := statusMap[server.Name].Status
	logs := statusMap[server.Name].Logs
	mu.Unlock()
	if finalStatus != "error" {
		t.Fatalf("final status = %q, want error", finalStatus)
	}
	if !strings.Contains(logs, "Pre-check failed (disk_space)") {
		t.Fatalf("missing pre-check failure log, got: %s", logs)
	}
	for _, cmd := range conn.commands {
		if cmd == aptUpdateCmd {
			t.Fatalf("apt update executed despite pre-check failure")
		}
	}

	db := getDB()
	var metaJSON string
	if err := db.QueryRow("SELECT meta_json FROM audit_events WHERE action = ? AND target_name = ? ORDER BY id DESC LIMIT 1", "update.complete", server.Name).Scan(&metaJSON); err != nil {
		t.Fatalf("query audit metadata: %v", err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		t.Fatalf("parse audit metadata: %v", err)
	}
	if _, ok := meta["prechecks_passed"]; !ok {
		t.Fatalf("missing prechecks_passed in audit metadata: %v", meta)
	}
	if _, ok := meta["precheck_failed"]; !ok {
		t.Fatalf("missing precheck_failed in audit metadata: %v", meta)
	}
	if _, ok := meta["precheck_results"]; !ok {
		t.Fatalf("missing precheck_results in audit metadata: %v", meta)
	}
	totalElapsedMS, ok := meta["total_elapsed_ms"].(float64)
	if !ok {
		t.Fatalf("missing total_elapsed_ms in audit metadata: %v", meta)
	}
	if totalElapsedMS < 0 {
		t.Fatalf("total_elapsed_ms = %v, want >= 0", totalElapsedMS)
	}
	if _, ok := meta["execution_duration_ms"]; ok {
		t.Fatalf("execution_duration_ms should not be set before approval: %v", meta)
	}
}
