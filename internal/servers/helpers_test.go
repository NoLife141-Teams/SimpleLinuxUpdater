package servers

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestServerEndpointExistsUsesNormalizedEndpoint(t *testing.T) {
	servers := []Server{
		{Name: "default", Host: "Node.EXAMPLE", Port: 0},
		{Name: "ipv6", Host: "2001:db8::1", Port: 2201},
	}

	tests := []struct {
		name string
		host string
		port int
		want bool
	}{
		{name: "same host and default port", host: "node.example", port: 22, want: true},
		{name: "DNS case is ignored", host: "NODE.example", port: 0, want: true},
		{name: "same host on another port", host: "node.example", port: 2202, want: false},
		{name: "IPv6 endpoint", host: "2001:DB8::1", port: 2201, want: true},
		{name: "IPv6 different port", host: "2001:db8::1", port: 2202, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServerEndpointExists(servers, tt.host, tt.port, -1); got != tt.want {
				t.Fatalf("ServerEndpointExists(%q, %d) = %t, want %t", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestServerEndpointExistsCanonicalizesEquivalentIPv6Literals(t *testing.T) {
	servers := []Server{{Name: "compressed", Host: "2001:db8::1", Port: 22}}

	if !ServerEndpointExists(servers, "2001:0db8:0:0:0:0:0:1", 22, -1) {
		t.Fatal("expanded IPv6 literal did not match the existing compressed endpoint")
	}
}

func TestNormalizeServerHostCanonicalizesIPIdentity(t *testing.T) {
	tests := []struct {
		name string
		host string
		want string
	}{
		{name: "DNS is case insensitive", host: " NODE.EXAMPLE ", want: "node.example"},
		{name: "IPv4", host: " 192.0.2.10 ", want: "192.0.2.10"},
		{name: "ambiguous IPv4 remains a hostname", host: "010.000.000.001", want: "010.000.000.001"},
		{name: "expanded IPv6", host: "2001:0db8:0:0:0:0:0:1", want: "2001:db8::1"},
		{name: "uppercase IPv6", host: "2001:DB8::A", want: "2001:db8::a"},
		{name: "bracketed IPv6", host: "[2001:db8::1]", want: "2001:db8::1"},
		{name: "bracketed DNS is not unwrapped", host: "[EXAMPLE.COM]", want: "[example.com]"},
		{name: "mapped IPv4 is unmapped", host: "::ffff:192.0.2.10", want: "192.0.2.10"},
		{name: "IPv6 zone is preserved", host: "fe80:0:0:0::1%eth0", want: "fe80::1%eth0"},
		{name: "IPv6 zone case is preserved", host: "fe80::1%ETH0", want: "fe80::1%ETH0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NormalizeServerHost(tt.host); got != tt.want {
				t.Fatalf("NormalizeServerHost(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

func TestCanonicalServerHostPreservesDNSPresentation(t *testing.T) {
	if got := CanonicalServerHost(" NODE.EXAMPLE "); got != "NODE.EXAMPLE" {
		t.Fatalf("CanonicalServerHost(DNS) = %q, want original case without spaces", got)
	}
	if got := CanonicalServerHost("[2001:0DB8:0:0:0:0:0:1]"); got != "2001:db8::1" {
		t.Fatalf("CanonicalServerHost(IPv6) = %q, want canonical literal", got)
	}
}

func TestServerEndpointExistsCanonicalIPCases(t *testing.T) {
	servers := []Server{
		{Name: "ipv4", Host: "192.0.2.10", Port: 22},
		{Name: "ipv6", Host: "2001:db8::1", Port: 2201},
		{Name: "zoned", Host: "fe80::1%eth0", Port: 22},
	}
	tests := []struct {
		name string
		host string
		port int
		want bool
	}{
		{name: "same IPv4", host: "192.0.2.10", port: 22, want: true},
		{name: "mapped IPv4", host: "::ffff:192.0.2.10", port: 22, want: true},
		{name: "expanded IPv6", host: "2001:0db8:0:0:0:0:0:1", port: 2201, want: true},
		{name: "bracketed IPv6", host: "[2001:db8::1]", port: 2201, want: true},
		{name: "different IPv6 port", host: "2001:0db8:0:0:0:0:0:1", port: 2202, want: false},
		{name: "same IPv6 zone", host: "fe80:0:0:0::1%eth0", port: 22, want: true},
		{name: "different IPv6 zone", host: "fe80::1%eth1", port: 22, want: false},
		{name: "zone case remains distinct", host: "fe80::1%ETH0", port: 22, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ServerEndpointExists(servers, tt.host, tt.port, -1); got != tt.want {
				t.Fatalf("ServerEndpointExists(%q, %d) = %t, want %t", tt.host, tt.port, got, tt.want)
			}
		})
	}
}

func TestKnownHostsHostTokenCanonicalizesIPLiteral(t *testing.T) {
	tests := []struct {
		host string
		port int
		want string
	}{
		{host: "2001:0db8:0:0:0:0:0:1", port: 22, want: "2001:db8::1"},
		{host: "[2001:db8::1]", port: 22, want: "2001:db8::1"},
		{host: "2001:0db8:0:0:0:0:0:1", port: 2201, want: "[2001:db8::1]:2201"},
		{host: "fe80:0:0:0::1%eth0", port: 2201, want: "[fe80::1%eth0]:2201"},
		{host: "[example.com]", port: 22, want: "[example.com]"},
	}

	for _, tt := range tests {
		if got := KnownHostsHostToken(tt.host, tt.port); got != tt.want {
			t.Errorf("KnownHostsHostToken(%q, %d) = %q, want %q", tt.host, tt.port, got, tt.want)
		}
	}
}

func TestKnownHostsRecognizesAndReplacesEquivalentLegacyIPv6Token(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	legacyLine := knownhosts.Line([]string{"2001:0db8:0:0:0:0:0:1"}, hostKey)
	if err := os.WriteFile(path, []byte(legacyLine+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := KnownHostsDeps{
		Getenv: func(key string) string {
			if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
				return path
			}
			return ""
		},
	}

	exists, err := KnownHostEntryExists(deps, "[2001:db8::1]", 22)
	if err != nil || !exists {
		t.Fatalf("KnownHostEntryExists() = %t, %v, want equivalent legacy entry", exists, err)
	}
	callback, err := HostKeyCallback(deps)
	if err != nil {
		t.Fatalf("HostKeyCallback() error = %v", err)
	}
	address := "[2001:db8::1]:22"
	if err := callback(address, knownHostsRemoteAddr(address), hostKey); err != nil {
		t.Fatalf("HostKeyCallback(equivalent IPv6) error = %v", err)
	}
	checker, err := newKnownHostEntryChecker(deps)
	if err != nil {
		t.Fatalf("newKnownHostEntryChecker() error = %v", err)
	}
	if exists, err := checker("2001:db8::1", 22); err != nil || !exists {
		t.Fatalf("checker(equivalent IPv6) = %t, %v, want trusted", exists, err)
	}

	replacementKeyBytes, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	replacementKey, err := ssh.NewPublicKey(replacementKeyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := callback(address, knownHostsRemoteAddr(address), replacementKey); err == nil {
		t.Fatal("HostKeyCallback(equivalent IPv6) accepted a different host key")
	}
	replacementLine := BuildKnownHostsLine("2001:db8::1", 22, replacementKey)
	if alreadyTrusted, err := ReplaceKnownHostLine(deps, "2001:db8::1", 22, replacementLine); err != nil || alreadyTrusted {
		t.Fatalf("ReplaceKnownHostLine() = %t, %v, want replacement", alreadyTrusted, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "2001:0db8:0:0:0:0:0:1") || !strings.Contains(string(data), "2001:db8::1") {
		t.Fatalf("known_hosts after replacement = %q, want only canonical IPv6 token", data)
	}

	removed, err := RemoveKnownHostEntries(deps, "[2001:0db8:0:0:0:0:0:1]", 22)
	if err != nil || removed != 1 {
		t.Fatalf("RemoveKnownHostEntries() = %d, %v, want one equivalent entry", removed, err)
	}
}

func TestKnownHostsRecognizesEquivalentIPv6TokenOnCustomPort(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	legacyLine := knownhosts.Line([]string{"[2001:0db8:0:0:0:0:0:1]:2201"}, hostKey)
	if err := os.WriteFile(path, []byte(legacyLine+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := KnownHostsDeps{Getenv: func(key string) string {
		if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
			return path
		}
		return ""
	}}

	exists, err := KnownHostEntryExists(deps, "2001:db8::1", 2201)
	if err != nil || !exists {
		t.Fatalf("KnownHostEntryExists(custom port) = %t, %v, want equivalent legacy entry", exists, err)
	}
	callback, err := HostKeyCallback(deps)
	if err != nil {
		t.Fatalf("HostKeyCallback() error = %v", err)
	}
	address := "[2001:db8::1]:2201"
	if err := callback(address, knownHostsRemoteAddr(address), hostKey); err != nil {
		t.Fatalf("HostKeyCallback(custom port equivalent IPv6) error = %v", err)
	}
}

func TestKnownHostsCanonicalFallbackDoesNotBypassRevokedIPv6Key(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	revokedLine := "@revoked " + knownhosts.Line([]string{"2001:0db8:0:0:0:0:0:1"}, hostKey)
	if err := os.WriteFile(path, []byte(revokedLine+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	deps := KnownHostsDeps{Getenv: func(key string) string {
		if key == "DEBIAN_UPDATER_KNOWN_HOSTS" {
			return path
		}
		return ""
	}}

	callback, err := HostKeyCallback(deps)
	if err != nil {
		t.Fatalf("HostKeyCallback() error = %v", err)
	}
	address := "[2001:db8::1]:22"
	if err := callback(address, knownHostsRemoteAddr(address), hostKey); err == nil {
		t.Fatal("HostKeyCallback(equivalent IPv6) accepted a revoked host key")
	}
}

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

func TestScanHostKeyConnectsWithBracketedExpandedIPv6(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	serverConfig := &ssh.ServerConfig{NoClientAuth: true}
	serverConfig.AddHostKey(signer)

	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback is unavailable: %v", err)
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

	_, rawPort, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatal(err)
	}
	scanned, err := ScanHostKey("[0:0:0:0:0:0:0:1]", port, time.Second)
	if err != nil {
		t.Fatalf("ScanHostKey(bracketed expanded IPv6) error = %v", err)
	}
	if !bytes.Equal(scanned.Marshal(), signer.PublicKey().Marshal()) {
		t.Fatal("ScanHostKey(bracketed expanded IPv6) returned a different key")
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("IPv6 SSH server did not finish")
	}
}
