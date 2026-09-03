package main

import (
	"io"
	"net"
	"strconv"
	"sync"

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

var dialSSHConnectionMu sync.RWMutex
var dialSSHConnection = func(server Server, config *ssh.ClientConfig) (sshConnection, error) {
	client, err := ssh.Dial("tcp", net.JoinHostPort(server.Host, strconv.Itoa(normalizePort(server.Port))), config)
	if err != nil {
		return nil, err
	}
	return &realSSHConnection{client: client}, nil
}

func getDialSSHConnection() func(Server, *ssh.ClientConfig) (sshConnection, error) {
	dialSSHConnectionMu.RLock()
	defer dialSSHConnectionMu.RUnlock()
	return dialSSHConnection
}
