package updates

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

type maintenanceTestConnection struct {
	closed int
}

func (*maintenanceTestConnection) NewSession() (SSHSessionRunner, error) {
	return nil, errors.New("unused")
}

func (c *maintenanceTestConnection) Close() error {
	c.closed++
	return nil
}

func TestProductionHostMaintenanceSessionOwnsReconnectRetryAndClose(t *testing.T) {
	server := servers.Server{Name: "srv", User: "root"}
	first := &maintenanceTestConnection{}
	second := &maintenanceTestConnection{}
	connections := []SSHConnection{first, second}
	dials := 0
	commands := 0
	stdinBuilds := 0
	var events []HostRetryEvent

	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(got servers.Server) ([]ssh.AuthMethod, error) {
			if got.Name != server.Name {
				t.Fatalf("server = %q, want %q", got.Name, server.Name)
			}
			return []ssh.AuthMethod{ssh.Password("secret")}, nil
		},
		HostKeyCallback: func() (ssh.HostKeyCallback, error) {
			return ssh.InsecureIgnoreHostKey(), nil
		},
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			conn := connections[dials]
			dials++
			return conn, nil
		},
		RunCommandWithTimeout: func(conn SSHConnection, cmd string, stdin io.Reader, _ time.Duration) (string, string, error) {
			commands++
			if _, err := io.ReadAll(stdin); err != nil {
				t.Fatalf("read command stdin: %v", err)
			}
			if commands == 1 {
				if conn != first {
					t.Fatalf("first command connection = %T, want first", conn)
				}
				return "", "mirror temporarily unavailable", errors.New("exit status 100")
			}
			if conn != second {
				t.Fatalf("second command connection = %T, want replacement", conn)
			}
			return "updated", "", nil
		},
		Sleep: func(time.Duration) {},
	})

	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         server,
		RetryPolicy:    RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
		DialOperation:  "update.ssh_dial",
		CommandTimeout: time.Second,
		OnRetry: func(event HostRetryEvent) {
			events = append(events, event)
		},
	})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	result, err := session.RunCommand(context.Background(), HostCommandRequest{
		Operation: "update.apt_update",
		Command:   AptUpdateCmd,
		Stdin: func() io.Reader {
			stdinBuilds++
			return strings.NewReader("password")
		},
		ReplayPolicy: ReplayRetryableOutputErrors,
	})
	if err != nil {
		t.Fatalf("RunCommand() error = %v", err)
	}
	if result.Stdout != "updated" || result.Attempts != 2 {
		t.Fatalf("RunCommand() = %+v, want updated after two attempts", result)
	}
	if stdinBuilds != 2 {
		t.Fatalf("stdin builds = %d, want one fresh reader per attempt", stdinBuilds)
	}
	if len(events) != 1 || events[0].Operation != "update.apt_update" || events[0].Attempt != 1 {
		t.Fatalf("retry events = %+v, want one apt-update retry", events)
	}
	stats := session.Stats()
	if stats.DialAttempts != 1 || stats.OperationAttempts["update.apt_update"] != 2 || stats.Reconnects != 1 {
		t.Fatalf("Stats() = %+v", stats)
	}
	if first.closed != 1 {
		t.Fatalf("first connection close count = %d, want 1 during reconnect", first.closed)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if second.closed != 1 {
		t.Fatalf("replacement close count = %d, want idempotent close", second.closed)
	}
}
