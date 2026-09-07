package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	updatespkg "debian-updater/internal/updates"
	"golang.org/x/crypto/ssh"
)

func TestSSHOutputLimitDrainsMutationAndRejectsIncompleteParsedOutput(t *testing.T) {
	for _, effect := range []updatespkg.HostCommandEffect{updatespkg.HostCommandEffectReadOnly, updatespkg.HostCommandEffectPackageStateMutation} {
		stdout := sshCommandOutputWriter{buffer: commandOutputBuffer(effect)}
		stderr := sshCommandOutputWriter{buffer: commandOutputBuffer(effect)}
		forwarded := 0
		stdout.onOutput = func(output updatespkg.HostCommandOutput) { forwarded += len(output.Data) }
		chunk := strings.Repeat("x", 8192)
		for range 1024 {
			if n, err := stdout.WriteString(chunk); n != len(chunk) || err != nil {
				t.Fatalf("drain = %d/%v", n, err)
			}
		}
		_, _ = stdout.WriteString("FINAL EXIT OUTPUT")
		out, _, err := commandOutputResult(effect, "fixture", &stdout, &stderr, nil)
		if forwarded != 8192*1024+len("FINAL EXIT OUTPUT") || !strings.HasSuffix(out, "FINAL EXIT OUTPUT") {
			t.Fatal("output was not fully drained or lost its tail")
		}
		if len(out) > stdout.buffer.Limit+len(updatespkg.OutputTruncationMarker) {
			t.Fatal("retained SSH output is unbounded")
		}
		if effect == updatespkg.HostCommandEffectReadOnly {
			if err == nil || updatespkg.IsRetryableError(err) {
				t.Fatalf("incomplete parsed output must fail closed: %v", err)
			}
		} else if err != nil {
			t.Fatalf("successful streamed mutation failed: %v", err)
		}
	}
}

func TestBoundedLivePreviewPreservesCompletePersistedMutationLog(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "large-log", Host: "192.0.2.91", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	payload := "PERSISTED HEAD\n" + strings.Repeat("package diagnostic\n", 24000) + "PERSISTED TAIL\n"
	conn := &reviewMutationConn{output: payload}
	factory := newHostMaintenanceSessionFactory(func(Server) ([]ssh.AuthMethod, error) { return nil, nil }, func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil }, func(Server, *ssh.ClientConfig) (sshConnection, error) { return conn, nil })
	service := NewUpdateService(UpdateServiceDeps{HostMaintenanceSessions: factory, ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager, LoadCommandTimeout: func() time.Duration { return time.Second }, AuditWithActor: func(_, _, _, _, _, _, _ string, _ map[string]any) {}})
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindAutoremove, server.Name, "review", "", RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	service.RunAutoremoveJob(AutoremoveRunRequest{Server: server, JobID: job.ID, Policy: RetryPolicy{MaxAttempts: 1}})
	status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
	if status.Status != "done" || len(status.Logs) > updatespkg.LiveStatusLogLimit {
		t.Fatalf("live preview status=%s bytes=%d", status.Status, len(status.Logs))
	}
	stored, err := app.Deps.CurrentJobManager().GetJobWithLogs(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stored.LogsText, payload) {
		t.Fatalf("completion replaced complete persisted output with the preview: stored=%d payload=%d", len(stored.LogsText), len(payload))
	}
}

func TestPackageInitializationDoesNotOpenApplicationDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-be-created.db")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^$")
	cmd.Env = append(os.Environ(), "DEBIAN_UPDATER_DB_PATH="+path)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("empty test process: %v: %s", err, output)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("package initialization touched database: %v", err)
	}
}
