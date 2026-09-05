package main

import (
	"context"
	"errors"
	"io"
	"net"
	"reflect"
	"strconv"
	"sync"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"golang.org/x/crypto/ssh"
)

type sshSessionRunner = updatespkg.SSHSessionRunner
type sshConnection = updatespkg.SSHConnection

type realSSHSession struct{ session *ssh.Session }

func (s *realSSHSession) SetStdin(r io.Reader)  { s.session.Stdin = r }
func (s *realSSHSession) SetStdout(w io.Writer) { s.session.Stdout = w }
func (s *realSSHSession) SetStderr(w io.Writer) { s.session.Stderr = w }
func (s *realSSHSession) Run(cmd string) error  { return s.session.Run(cmd) }
func (s *realSSHSession) Close() error          { return s.session.Close() }

type realSSHConnection struct{ client *ssh.Client }

func (c *realSSHConnection) NewSession() (sshSessionRunner, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}
	return &realSSHSession{session: session}, nil
}

func (c *realSSHConnection) Close() error { return c.client.Close() }

func defaultDialSSHConnection(server Server, config *ssh.ClientConfig) (sshConnection, error) {
	return dialRealSSHConnectionWithContext(context.Background(), server, config)
}

var dialSSHConnectionMu sync.RWMutex
var dialSSHConnection = defaultDialSSHConnection

// dialSSHConnectionWithContext preserves the package-level injectable dial hook
// used by tests while ensuring the real production TCP + SSH handshake is
// cancellable. Custom dial hooks are isolated behind a cancellation-aware
// adapter so lifecycle tests can also exercise blocked dials safely.
func dialSSHConnectionWithContext(ctx context.Context, dial func(Server, *ssh.ClientConfig) (sshConnection, error), server Server, config *ssh.ClientConfig) (sshConnection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if dial == nil || reflect.ValueOf(dial).Pointer() == reflect.ValueOf(defaultDialSSHConnection).Pointer() {
		return dialRealSSHConnectionWithContext(ctx, server, config)
	}

	type dialResult struct {
		conn       sshConnection
		err        error
		panicValue any
	}
	resultCh := make(chan dialResult, 1)
	go func() {
		result := dialResult{}
		defer func() {
			if recovered := recover(); recovered != nil {
				result.panicValue = recovered
			}
			resultCh <- result
		}()
		result.conn, result.err = dial(server, config)
	}()
	select {
	case result := <-resultCh:
		if result.panicValue != nil {
			panic(result.panicValue)
		}
		return result.conn, result.err
	case <-ctx.Done():
		go func() {
			result := <-resultCh
			if result.conn != nil {
				_ = result.conn.Close()
			}
		}()
		return nil, ctx.Err()
	}
}

func dialRealSSHConnectionWithContext(ctx context.Context, server Server, config *ssh.ClientConfig) (sshConnection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config == nil {
		return nil, errors.New("missing SSH client config")
	}
	address := sshServerAddress(server)
	dialer := &net.Dialer{Timeout: config.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = conn.Close() })
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, address, config)
	stopCancellation()
	if err != nil {
		_ = conn.Close()
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		return nil, err
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		_ = sshConn.Close()
		return nil, ctxErr
	}
	return &realSSHConnection{client: ssh.NewClient(sshConn, chans, reqs)}, nil
}

func sshServerAddress(server Server) string {
	return net.JoinHostPort(serverpkg.ServerHostForTransport(server.Host), strconv.Itoa(normalizePort(server.Port)))
}

func getDialSSHConnection() func(Server, *ssh.ClientConfig) (sshConnection, error) {
	dialSSHConnectionMu.RLock()
	defer dialSSHConnectionMu.RUnlock()
	return dialSSHConnection
}
