package servers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestKnownHostsConfiguredPathListSemantics(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		separator string
		want      []string
	}{
		{
			name:      "Unix path list",
			raw:       "/a:/b",
			separator: ":",
			want:      []string{"/a", "/b"},
		},
		{
			name:      "Windows path list",
			raw:       `C:\Users\Alice\.ssh\known_hosts;D:\shared\known_hosts`,
			separator: ";",
			want:      []string{`C:\Users\Alice\.ssh\known_hosts`, `D:\shared\known_hosts`},
		},
		{
			name:      "single Windows path",
			raw:       `C:\Users\Alice\.ssh\known_hosts`,
			separator: ";",
			want:      []string{`C:\Users\Alice\.ssh\known_hosts`},
		},
		{
			name:      "spaces and empty entries",
			raw:       ` ; C:\Users\Alice\.ssh\known_hosts ; ; D:\shared\known_hosts ; `,
			separator: ";",
			want:      []string{`C:\Users\Alice\.ssh\known_hosts`, `D:\shared\known_hosts`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deps := KnownHostsDeps{
				Getenv: func(key string) string {
					if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
						return tt.raw
					}
					return ""
				},
				SplitPathList: func(raw string) []string {
					return strings.Split(raw, tt.separator)
				},
			}

			if got := KnownHostsPaths(deps); !slices.Equal(got, tt.want) {
				t.Fatalf("KnownHostsPaths() = %q, want %q", got, tt.want)
			}
			gotWrite, err := KnownHostsWritePath(deps)
			if err != nil {
				t.Fatalf("KnownHostsWritePath() error = %v", err)
			}
			if gotWrite != tt.want[0] {
				t.Fatalf("KnownHostsWritePath() = %q, want first configured path %q", gotWrite, tt.want[0])
			}
		})
	}
}

func TestKnownHostsPathsUsesNativePathListSeparator(t *testing.T) {
	want := []string{"first-known-hosts", "second-known-hosts"}
	raw := strings.Join(want, string(os.PathListSeparator))
	deps := KnownHostsDeps{Getenv: func(key string) string {
		if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
			return raw
		}
		return ""
	}}

	if got := KnownHostsPaths(deps); !slices.Equal(got, want) {
		t.Fatalf("KnownHostsPaths() = %q, want native path list %q", got, want)
	}
}

func TestKnownHostsConfiguredPathListRejectsOnlyEmptyEntries(t *testing.T) {
	deps := KnownHostsDeps{
		Getenv: func(key string) string {
			if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
				return " ; ; "
			}
			return ""
		},
		SplitPathList: func(raw string) []string {
			return strings.Split(raw, ";")
		},
	}

	if got := KnownHostsPaths(deps); len(got) != 0 {
		t.Fatalf("KnownHostsPaths() = %q, want no configured paths", got)
	}
	if _, err := KnownHostsWritePath(deps); err == nil {
		t.Fatal("KnownHostsWritePath() error = nil, want no configured path error")
	}
}

func TestCaptureHostKeyCallbackRecordsKeyAndRejectsHandshake(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	hostKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatalf("NewPublicKey() error = %v", err)
	}

	var captured ssh.PublicKey
	callback := captureHostKeyCallback(&captured)
	err = callback("example.com:22", nil, hostKey)
	if !errors.Is(err, errHostKeyCaptured) {
		t.Fatalf("callback() error = %v, want %v", err, errHostKeyCaptured)
	}
	if captured == nil || !bytes.Equal(captured.Marshal(), hostKey.Marshal()) {
		t.Fatal("callback() did not retain the presented host key")
	}
}

func TestScanHostKeyStopsBeforeAuthentication(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatalf("NewSignerFromKey() error = %v", err)
	}

	var authenticationAttempted atomic.Bool
	serverConfig := &ssh.ServerConfig{
		PasswordCallback: func(ssh.ConnMetadata, []byte) (*ssh.Permissions, error) {
			authenticationAttempted.Store(true)
			return nil, errors.New("authentication rejected")
		},
	}
	serverConfig.AddHostKey(signer)

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _, _, _ = ssh.NewServerConn(conn, serverConfig)
	}()

	host, rawPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("SplitHostPort() error = %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("Atoi() error = %v", err)
	}

	scanned, err := ScanHostKey(host, port, time.Second)
	if err != nil {
		t.Fatalf("ScanHostKey() error = %v", err)
	}
	if !bytes.Equal(scanned.Marshal(), signer.PublicKey().Marshal()) {
		t.Fatal("ScanHostKey() returned a different host key")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("SSH server did not observe the rejected handshake")
	}
	if authenticationAttempted.Load() {
		t.Fatal("ScanHostKey() attempted authentication before the host key was trusted")
	}
}
