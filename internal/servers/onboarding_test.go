package servers

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

const onboardingTestDisk = "Filesystem 1024-blocks Used Available Capacity Mounted on\n/dev/root 9999999 1000 5000000 1% /\n/dev/root 9999999 1000 5000000 1% /\n/dev/root 9999999 1000 5000000 1% /\n"

type onboardingTestSession struct {
	outputs  map[string]string
	errors   map[string]error
	commands []string
	closed   bool
}

func (s *onboardingTestSession) Read(ctx context.Context, command string) (string, error) {
	s.commands = append(s.commands, command)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.outputs[command], s.errors[command]
}
func (s *onboardingTestSession) Close() error { s.closed = true; return nil }

func newOnboardingTest(t *testing.T) (*OnboardingService, *onboardingTestSession, *fakeRepo, OnboardingDraft, string) {
	t.Helper()
	repo := &fakeRepo{}
	inventory, _, _, _ := newTestService(repo, nil)
	key, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "known_hosts")
	inventory.deps.KnownHosts = KnownHostsDeps{Getenv: func(name string) string {
		if name == "DEBIAN_UPDATER_KNOWN_HOSTS" {
			return path
		}
		return ""
	}, UserHomeDir: func() (string, error) { return t.TempDir(), nil }, ScanHostKey: func(string, int) (ssh.PublicKey, error) { return key.PublicKey(), nil }}
	session := &onboardingTestSession{outputs: map[string]string{OnboardingOSCommand: "ID=debian\nVERSION_ID=13\n", OnboardingDiskCommand: onboardingTestDisk}, errors: map[string]error{}}
	service := NewOnboardingService(OnboardingDeps{Inventory: inventory, Open: func(context.Context, Server, string) (OnboardingSession, error) { return session, nil }, ResolveGlobalKey: func(context.Context) (string, error) { return testPrivateKeyPEM(t), nil }})
	draft := OnboardingDraft{Name: "lab", Host: "192.0.2.24", User: "deployer", Port: 22, Pass: "secret-do-not-log", AuthMethod: "password", Fingerprint: ssh.FingerprintSHA256(key.PublicKey()), Confirmed: true}
	return service, session, repo, draft, path
}

func TestOnboardingChecksAreReadOnlyAndGateCreation(t *testing.T) {
	for _, scenario := range []struct {
		name, command, output string
		commandErr            error
		ready                 bool
		warning               bool
	}{
		{name: "success", ready: true},
		{name: "unsupported OS", command: OnboardingOSCommand, output: "ID=fedora\n"},
		{name: "missing apt", command: OnboardingAPTCommand, commandErr: errors.New("secret-do-not-log")},
		{name: "missing permissions", command: OnboardingSudoCommand, commandErr: errors.New("sudo denied")},
		{name: "invalid disk", command: OnboardingDiskCommand, output: "invalid"},
		{name: "low disk blocks", command: OnboardingDiskCommand, output: strings.ReplaceAll(onboardingTestDisk, "5000000", "100")},
		{name: "low disk warns", command: OnboardingDiskCommand, output: strings.ReplaceAll(onboardingTestDisk, "5000000", "800000"), ready: true, warning: true},
		{name: "broken packages", command: OnboardingPackageCommand, output: "package configuration pending"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			svc, session, repo, draft, path := newOnboardingTest(t)
			if scenario.command != "" {
				session.outputs[scenario.command] = scenario.output
				session.errors[scenario.command] = scenario.commandErr
			}
			report, err := svc.Check(context.Background(), draft)
			if err != nil || report.Ready != scenario.ready {
				t.Fatalf("ready = %v, error = %v", report.Ready, err)
			}
			if repo.saveCalls != 0 || !session.closed {
				t.Fatal("check persisted inventory or leaked its connection")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("check persisted host trust")
			}
			allowed := map[string]bool{OnboardingOSCommand: true, OnboardingAPTCommand: true, OnboardingSudoCommand: true, OnboardingDiskCommand: true, OnboardingPackageCommand: true}
			for _, command := range session.commands {
				if !allowed[command] {
					t.Fatalf("unexpected remote command %q", command)
				}
			}
			warnings := 0
			for _, check := range report.Checks {
				if strings.Contains(check.Message+check.Remediation, draft.Pass) {
					t.Fatal("secret leaked in diagnostic")
				}
				if check.Status == "warning" {
					warnings++
				}
			}
			if scenario.warning && warnings != 1 {
				t.Fatal("missing disk warning")
			}
			if !scenario.ready {
				_, _, err = svc.Create(context.Background(), draft)
				if err != nil || repo.saveCalls != 0 {
					t.Fatal("failed checks saved the server")
				}
			}
		})
	}
}

func TestOnboardingCreateRechecksAndSavesCredentialsTogether(t *testing.T) {
	svc, session, repo, draft, _ := newOnboardingTest(t)
	draft.AuthMethod, draft.Key = "per-server-key", testPrivateKeyPEM(t)
	if report, err := svc.Check(context.Background(), draft); err != nil || !report.Ready {
		t.Fatal("initial check failed")
	}
	session.errors[OnboardingSudoCommand] = errors.New("permissions changed")
	_, report, err := svc.Create(context.Background(), draft)
	if err != nil || report.Ready || repo.saveCalls != 0 {
		t.Fatal("stale browser result bypassed fresh checks")
	}
	delete(session.errors, OnboardingSudoCommand)
	result, report, err := svc.Create(context.Background(), draft)
	if err != nil || !report.Ready || !result.Succeeded() {
		t.Fatalf("creation failed: %v %+v", err, result)
	}
	if repo.saveCalls != 1 || len(repo.saved) != 1 || repo.saved[0].Key != draft.Key || repo.saved[0].Pass != "" {
		t.Fatal("selected credential was not persisted atomically")
	}
}

func TestOnboardingValidationAndAuthentication(t *testing.T) {
	for _, name := range []string{"unconfirmed", "invalid port", "invalid user", "URL host", "invalid key", "missing password", "oversize key", "duplicate", "missing global"} {
		t.Run(name, func(t *testing.T) {
			svc, session, repo, draft, _ := newOnboardingTest(t)
			switch name {
			case "unconfirmed":
				draft.Confirmed = false
			case "invalid port":
				draft.Port = 65536
			case "invalid user":
				draft.User = "root;id"
			case "URL host":
				draft.Host = "http://192.0.2.24:22"
			case "invalid key":
				draft.AuthMethod, draft.Key = "per-server-key", "invalid-private-key"
			case "missing password":
				draft.Pass = ""
			case "oversize key":
				draft.Key = strings.Repeat("x", 65537)
			case "duplicate":
				_, _ = svc.deps.Inventory.Create(Server{Name: draft.Name, Host: "other", User: "root"})
				repo.saveCalls = 0
			case "missing global":
				draft.AuthMethod = "global-key"
				svc.deps.ResolveGlobalKey = func(context.Context) (string, error) { return "", errors.New("unavailable") }
			}
			if _, err := svc.Check(context.Background(), draft); err == nil {
				t.Fatal("invalid draft accepted")
			}
			if len(session.commands) != 0 || repo.saveCalls != 0 {
				t.Fatal("invalid request performed work")
			}
		})
	}
}

func TestOnboardingConnectionFailureDoesNotEchoSecrets(t *testing.T) {
	svc, _, repo, draft, _ := newOnboardingTest(t)
	svc.deps.Open = func(context.Context, Server, string) (OnboardingSession, error) { return nil, errors.New(draft.Pass) }
	report, err := svc.Check(context.Background(), draft)
	if err != nil || report.Ready || len(report.Checks) != 1 || strings.Contains(report.Checks[0].Message, draft.Pass) || repo.saveCalls != 0 {
		t.Fatal("unsafe connection failure")
	}
}

func TestOnboardingNeverReplacesChangedOrRevokedTrustAtConfirmation(t *testing.T) {
	for _, revoked := range []bool{false, true} {
		t.Run(map[bool]string{false: "changed", true: "revoked"}[revoked], func(t *testing.T) {
			svc, _, repo, draft, path := newOnboardingTest(t)
			key, err := svc.deps.Inventory.deps.KnownHosts.scanHostKey(draft.Host, draft.Port)
			if err != nil {
				t.Fatal(err)
			}
			if !revoked {
				signer, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
				if err != nil {
					t.Fatal(err)
				}
				key = signer.PublicKey()
			}
			line := knownhosts.Line([]string{draft.Host}, key)
			if revoked {
				line = "@revoked " + line
			}
			if err := os.WriteFile(path, []byte(line+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			_, _, err = svc.Create(context.Background(), draft)
			if err == nil || repo.saveCalls != 0 {
				t.Fatal("changed trust was replaced during confirmation")
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != line+"\n" {
				t.Fatal("existing trust was modified")
			}
		})
	}
}

func TestOnboardingGlobalCredentialIsReferencedAndMustRemainCurrent(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "stable", true: "changed"}[changed], func(t *testing.T) {
			svc, _, repo, draft, _ := newOnboardingTest(t)
			draft.AuthMethod = "global-key"
			key, replacement := testPrivateKeyPEM(t), testPrivateKeyPEM(t)
			resolutions := 0
			svc.deps.ResolveGlobalKey = func(context.Context) (string, error) {
				resolutions++
				if changed && resolutions > 1 {
					return replacement, nil
				}
				return key, nil
			}
			result, report, err := svc.Create(context.Background(), draft)
			if changed {
				if err == nil || repo.saveCalls != 0 {
					t.Fatal("changed global credential was accepted")
				}
			} else if err != nil || !report.Ready || !result.Succeeded() || len(repo.saved) != 1 || repo.saved[0].Key != "" {
				t.Fatal("global credential was not saved as a reference")
			}
		})
	}
}

func TestOnboardingAcceptsScopedIPv6Addresses(t *testing.T) {
	for _, host := range []string{"fe80::1%eth0", "[fe80::1%ETH0]"} {
		t.Run(host, func(t *testing.T) {
			svc, _, repo, draft, path := newOnboardingTest(t)
			draft.Host = host
			report, err := svc.Check(context.Background(), draft)
			if err != nil || !report.Ready || repo.saveCalls != 0 {
				t.Fatalf("scoped IPv6 verification failed: ready=%v, err=%v", report.Ready, err)
			}
			result, report, err := svc.Create(context.Background(), draft)
			if err != nil || !report.Ready || !result.Succeeded() || len(repo.saved) != 1 || repo.saved[0].Host != host {
				t.Fatalf("scoped IPv6 creation failed: err=%v, result=%+v", err, result)
			}
			key, err := svc.deps.Inventory.deps.KnownHosts.scanHostKey(host, draft.Port)
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil || string(data) != BuildKnownHostsLine(host, draft.Port, key)+"\n" {
				t.Fatal("scoped IPv6 host trust was not persisted")
			}
		})
	}
}

func TestOnboardingHostKeyCallbackPreservesCanonicalEndpointTrust(t *testing.T) {
	for _, tc := range []struct {
		name, storedHost, submittedHost string
		port                            int
	}{
		{"expanded IPv6", "2001:0db8:0:0:0:0:0:1", "2001:db8::1", 22},
		{"custom port IPv6", "2001:0db8:0:0:0:0:0:1", "2001:db8::1", 2222},
		{"mapped IPv4", "::ffff:192.0.2.24", "192.0.2.24", 22},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, repo, draft, path := newOnboardingTest(t)
			draft.Host, draft.Port = tc.submittedHost, tc.port
			key, err := svc.deps.Inventory.deps.KnownHosts.scanHostKey(draft.Host, draft.Port)
			if err != nil {
				t.Fatal(err)
			}
			other, err := ssh.ParsePrivateKey([]byte(testPrivateKeyPEM(t)))
			if err != nil {
				t.Fatal(err)
			}
			storedAddress := tc.storedHost
			if tc.port != 22 {
				storedAddress = fmt.Sprintf("[%s]:%d", tc.storedHost, tc.port)
			}
			address := net.JoinHostPort(tc.submittedHost, strconv.Itoa(tc.port))
			for _, sameKey := range []bool{true, false} {
				storedKey := key
				if !sameKey {
					storedKey = other.PublicKey()
				}
				line := knownhosts.Line([]string{storedAddress}, storedKey) + "\n"
				if err := os.WriteFile(path, []byte(line), 0600); err != nil {
					t.Fatal(err)
				}
				callback, err := OnboardingHostKeyCallback(svc.deps.Inventory.deps.KnownHosts, draft.Fingerprint)
				if err != nil {
					t.Fatal(err)
				}
				if accepted := callback(address, knownHostsRemoteAddr(address), key) == nil; accepted != sameKey {
					t.Fatalf("canonical saved key accepted=%v, sameKey=%v", accepted, sameKey)
				}
				if !sameKey {
					if _, _, err := svc.Create(context.Background(), draft); err == nil || repo.saveCalls != 0 {
						t.Fatal("confirmation bypassed changed canonical trust")
					}
					if data, err := os.ReadFile(path); err != nil || string(data) != line {
						t.Fatal("confirmation appended trust despite a changed canonical identity")
					}
				}
			}
		})
	}
}
