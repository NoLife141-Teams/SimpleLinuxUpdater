package main

import (
	"context"
	"errors"
	"net"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestSSHServerAddressCanonicalizesIPLiteral(t *testing.T) {
	tests := []struct {
		name   string
		server Server
		want   string
	}{
		{name: "bracketed expanded IPv6", server: Server{Host: "[2001:0db8:0:0:0:0:0:1]", Port: 22}, want: "[2001:0db8:0:0:0:0:0:1]:22"},
		{name: "IPv6 zone", server: Server{Host: "fe80:0:0:0::1%eth0", Port: 2201}, want: "[fe80:0:0:0::1%eth0]:2201"},
		{name: "mapped IPv4", server: Server{Host: "::ffff:192.0.2.10", Port: 22}, want: "[::ffff:192.0.2.10]:22"},
		{name: "DNS presentation", server: Server{Host: "NODE.EXAMPLE", Port: 22}, want: "NODE.EXAMPLE:22"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := sshServerAddress(tt.server); got != tt.want {
				t.Fatalf("sshServerAddress(%+v) = %q, want %q", tt.server, got, tt.want)
			}
		})
	}
}

func TestDialRealSSHConnectionWithContextCancelsHandshake(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- conn
		}
	}()

	host, rawPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("Atoi(port) error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan error, 1)
	go func() {
		_, dialErr := dialRealSSHConnectionWithContext(ctx, Server{Host: host, Port: port, User: "root"}, &ssh.ClientConfig{
			User:            "root",
			HostKeyCallback: ssh.InsecureIgnoreHostKey(),
			Timeout:         5 * time.Second,
		})
		resultCh <- dialErr
	}()

	var serverConn net.Conn
	select {
	case serverConn = <-accepted:
		defer serverConn.Close()
	case <-time.After(time.Second):
		t.Fatal("SSH dial did not reach the local listener")
	}
	cancel()

	select {
	case dialErr := <-resultCh:
		if !errors.Is(dialErr, context.Canceled) {
			t.Fatalf("dial error = %v, want context canceled", dialErr)
		}
	case <-time.After(time.Second):
		t.Fatal("SSH handshake did not stop after context cancellation")
	}
}
