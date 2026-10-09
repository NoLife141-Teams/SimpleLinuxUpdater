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
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// OnboardingDraft holds credentials only for the lifetime of a request. It is
// deliberately separate from Server, whose response serialization hides secrets.
type OnboardingDraft struct {
	Name        string   `json:"name"`
	Host        string   `json:"host"`
	Port        int      `json:"port"`
	User        string   `json:"user"`
	Pass        string   `json:"pass"`
	Key         string   `json:"key"`
	Tags        []string `json:"tags"`
	AuthMethod  string   `json:"auth_method"`
	Fingerprint string   `json:"fingerprint_sha256"`
	Confirmed   bool     `json:"confirmed"`
}

type OnboardingCheck struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Message     string `json:"message"`
	Remediation string `json:"remediation,omitempty"`
}

type OnboardingReport struct {
	Ready           bool              `json:"ready"`
	Checks          []OnboardingCheck `json:"checks"`
	Distribution    string            `json:"distribution,omitempty"`
	DiskAvailableKB int64             `json:"disk_available_kb,omitempty"`
}

type OnboardingSession interface {
	Read(context.Context, string) (string, error)
	Close() error
}

type OnboardingDeps struct {
	Inventory        *Service
	ResolveGlobalKey func(context.Context) (string, error)
	Open             func(context.Context, Server, string) (OnboardingSession, error)
}

type OnboardingService struct {
	deps  OnboardingDeps
	slots chan struct{}
}

var ErrOnboardingBusy = errors.New("server verification is busy; retry shortly")

func NewOnboardingService(deps OnboardingDeps) *OnboardingService {
	return &OnboardingService{deps: deps, slots: make(chan struct{}, 2)}
}

func (s *OnboardingService) prepare(ctx context.Context, draft OnboardingDraft) (Server, error) {
	server := Server{Name: strings.TrimSpace(draft.Name), Host: strings.TrimSpace(draft.Host), Port: draft.Port, User: strings.TrimSpace(draft.User), Tags: draft.Tags}
	if server.Name == "" || server.Host == "" || server.User == "" {
		return server, ErrRequiredFields
	}
	if len(server.Name) > 128 || len(server.Host) > 253 || len(draft.Tags) > 64 || len(draft.Pass) > 4096 || len(draft.Key) > 64*1024 {
		return server, errors.New("server details exceed the allowed size")
	}
	if !IsValidSSHUsername(server.User) {
		return server, ErrInvalidSSHUsername
	}
	if server.Port == 0 {
		server.Port = 22
	}
	if server.Port < 1 || server.Port > 65535 {
		return server, errors.New("SSH port must be between 1 and 65535")
	}
	if !validOnboardingHost(server.Host) {
		return server, errors.New("enter an IP address or hostname, without a URL or port")
	}
	if !draft.Confirmed || !strings.HasPrefix(draft.Fingerprint, "SHA256:") {
		return server, errors.New("verify and confirm the SSH fingerprint first")
	}
	state := s.deps.Inventory.state()
	state.Lock()
	nameExists := ServerNameExists(state.Servers(), server.Name, -1)
	endpointExists := ServerEndpointExists(state.Servers(), server.Host, server.Port, -1)
	state.Unlock()
	if nameExists {
		return server, ErrNameExists
	}
	if endpointExists {
		return server, ErrEndpointExists
	}
	switch draft.AuthMethod {
	case "password":
		if draft.Pass == "" {
			return server, errors.New("a password is required")
		}
		server.Pass = draft.Pass
	case "per-server-key":
		if strings.TrimSpace(draft.Key) == "" {
			return server, errors.New("choose a per-server SSH key")
		}
		server.Key = draft.Key
	case "global-key":
		key, err := s.deps.ResolveGlobalKey(ctx)
		if err != nil || key == "" {
			return server, errors.New("the Global SSH Credential is unavailable")
		}
		server.Key = key
	default:
		return server, errors.New("choose an authentication method")
	}
	if _, err := BuildAuthMethods(server); err != nil {
		return server, errors.New("the SSH key is invalid or encrypted; use an unencrypted private key")
	}
	return server, nil
}

func validOnboardingHost(host string) bool {
	if net.ParseIP(ServerHostForTransport(host)) != nil {
		return true
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

// Fixed read-only commands: no metadata refresh, package installation, repair,
// reboot, or sudoers provisioning occurs during onboarding.
const OnboardingOSCommand = "cat /etc/os-release"
const OnboardingAPTCommand = "test -x /usr/bin/apt-get && test -x /usr/bin/apt-cache && test -x /usr/bin/dpkg"
const OnboardingSudoCommand = `if [ "$(id -u)" -eq 0 ]; then printf root; else test -x /usr/local/sbin/simplelinuxupdater-root-helper && sudo -n -l -- /usr/local/sbin/simplelinuxupdater-root-helper update >/dev/null 2>&1 && sudo -n -l -- /usr/local/sbin/simplelinuxupdater-root-helper upgrade >/dev/null 2>&1 && sudo -n /usr/local/sbin/simplelinuxupdater-root-helper dpkg-audit; fi`
const OnboardingDiskCommand = "LC_ALL=C df -Pk / /var /boot"
const OnboardingPackageCommand = "/usr/bin/dpkg --audit"

func (s *OnboardingService) Check(ctx context.Context, draft OnboardingDraft) (OnboardingReport, error) {
	_, report, err := s.verify(ctx, draft, false)
	return report, err
}

// Create verifies the submitted draft again, so edited credentials and stale
// browser results cannot skip checks. Both secrets are saved in one transaction.
func (s *OnboardingService) Create(ctx context.Context, draft OnboardingDraft) (CommandResult, OnboardingReport, error) {
	return s.verify(ctx, draft, true)
}

func (s *OnboardingService) verify(ctx context.Context, draft OnboardingDraft, create bool) (CommandResult, OnboardingReport, error) {
	report := OnboardingReport{Checks: []OnboardingCheck{}}
	select {
	case s.slots <- struct{}{}:
		defer func() { <-s.slots }()
	default:
		return CommandResult{}, report, ErrOnboardingBusy
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	server, err := s.prepare(ctx, draft)
	if err != nil {
		return CommandResult{}, report, err
	}
	session, err := s.deps.Open(ctx, server, draft.Fingerprint)
	if err != nil {
		report.Checks = append(report.Checks, OnboardingCheck{ID: "ssh", Status: "failed", Message: "SSH connection or identity verification failed.", Remediation: "Check the address, SSH port, firewall, user and credential. If the fingerprint changed, scan and verify it again."})
		return CommandResult{}, report, nil
	}
	defer session.Close()
	report.Checks = append(report.Checks, OnboardingCheck{ID: "ssh", Status: "passed", Message: "Authenticated SSH connection with the confirmed fingerprint."})
	checks := []struct{ id, command, success, failure, remediation string }{
		{"distribution", OnboardingOSCommand, "Supported Linux distribution.", "Cannot verify a supported distribution.", "Use a Debian or Ubuntu server and ensure /etc/os-release is readable."},
		{"apt", OnboardingAPTCommand, "APT and dpkg are available.", "APT or dpkg is unavailable.", "Install APT and dpkg on a supported Debian or Ubuntu server."},
		{"sudo", OnboardingSudoCommand, "Root access or restricted maintenance policy detected.", "Restricted maintenance permissions could not be verified.", "Install the SimpleLinuxUpdater managed root helper and sudoers policy, or use root. Generic passwordless apt rules do not replace the managed helper."},
		{"disk", OnboardingDiskCommand, "Disk space verified.", "Cannot verify free disk space.", "Check that df can read /, /var and /boot."},
		{"packages", OnboardingPackageCommand, "No unfinished dpkg configuration detected.", "The package state could not be verified.", "Inspect dpkg --audit on the server and resolve unfinished package configuration."},
	}
	report.Ready = true
	for _, check := range checks {
		output, runErr := session.Read(ctx, check.command)
		result := OnboardingCheck{ID: check.id, Status: "passed", Message: check.success}
		if runErr != nil {
			result.Status, result.Message, result.Remediation = "failed", check.failure, check.remediation
		} else {
			switch check.id {
			case "distribution":
				id, version := osReleaseValue(output, "ID"), osReleaseValue(output, "VERSION_ID")
				if id != "debian" && id != "ubuntu" {
					result.Status, result.Message, result.Remediation = "failed", "Only Debian and Ubuntu are supported.", check.remediation
				} else {
					report.Distribution = id + " " + version
					result.Message = report.Distribution
				}
			case "disk":
				available, parseErr := onboardingDiskAvailable(output)
				if parseErr != nil {
					result.Status, result.Message, result.Remediation = "failed", check.failure, check.remediation
				} else {
					report.DiskAvailableKB = available
					result.Message = fmt.Sprintf("At least %.1f GiB available across /, /var and /boot.", float64(available)/(1024*1024))
					if available < 200*1024 {
						result.Status, result.Remediation = "failed", "Free at least 200 MiB on each checked filesystem before adding this server."
					} else if available < 1024*1024 {
						result.Status, result.Remediation = "warning", "Less than 1 GiB is available. Actual update disk requirements are checked again before each update."
					}
				}
			case "packages":
				if strings.TrimSpace(output) != "" {
					result.Status, result.Message, result.Remediation = "failed", "dpkg reports unfinished package configuration.", check.remediation
				}
			}
		}
		if result.Status == "failed" {
			report.Ready = false
		}
		report.Checks = append(report.Checks, result)
		if ctx.Err() != nil {
			report.Ready = false
			break
		}
	}
	if !create || !report.Ready {
		return CommandResult{}, report, nil
	}
	if err := ctx.Err(); err != nil {
		return CommandResult{}, report, err
	}
	if draft.AuthMethod == "global-key" {
		currentKey, err := s.deps.ResolveGlobalKey(ctx)
		if err != nil || !s.deps.Inventory.deps.KnownHosts.constantTimeCompare(currentKey, server.Key) {
			return CommandResult{}, report, errors.New("the Global SSH Credential changed; run checks again")
		}
	}
	if err := s.deps.Inventory.pinOnboardingHostKey(ctx, server.Host, server.Port, draft.Fingerprint); err != nil {
		return CommandResult{}, report, errors.New("the SSH identity changed or its trust could not be saved; verify the fingerprint again")
	}
	if draft.AuthMethod == "global-key" {
		server.Key = ""
	}
	result := NewCommandService(s.deps.Inventory).CreateServer(server)
	if result.Succeeded() {
		result.Audit.Meta["onboarding_verified"] = true
		result.Audit.Meta["fingerprint_sha256"] = draft.Fingerprint
	}
	return result, report, nil
}

// OnboardingHostKeyCallback pins the confirmed key before sending credentials,
// while preserving changed and revoked entries in the configured trust stores.
func OnboardingHostKeyCallback(deps KnownHostsDeps, fingerprint string) (ssh.HostKeyCallback, error) {
	var existing ssh.HostKeyCallback
	for _, path := range KnownHostsPaths(deps) {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		existing, err = HostKeyCallback(deps)
		if err != nil {
			return nil, err
		}
		break
	}
	return func(host string, remote net.Addr, key ssh.PublicKey) error {
		if !deps.constantTimeCompare(ssh.FingerprintSHA256(key), fingerprint) {
			return ErrFingerprintMismatch
		}
		if existing != nil {
			err := existing(host, remote, key)
			var unknown *knownhosts.KeyError
			if err != nil && !(errors.As(err, &unknown) && len(unknown.Want) == 0) {
				return err
			}
		}
		return nil
	}, nil
}

// Adding a server never replaces existing host trust. Recheck the latest trust
// under the same lock used by inventory trust mutations and append atomically.
func (s *Service) pinOnboardingHostKey(ctx context.Context, host string, port int, fingerprint string) error {
	deps := s.deps.KnownHosts
	key, err := deps.scanHostKey(host, port)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := KnownHostsWritePath(deps)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	mu := deps.knownHostsMu()
	mu.Lock()
	defer mu.Unlock()
	callback, err := OnboardingHostKeyCallback(deps, fingerprint)
	if err != nil {
		return err
	}
	cleanHost := ServerHostForTransport(host)
	if err := callback(net.JoinHostPort(cleanHost, strconv.Itoa(port)), &net.TCPAddr{IP: net.ParseIP(cleanHost), Port: port}, key); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	line := BuildKnownHostsLine(host, port, key)
	if strings.Contains("\n"+string(data)+"\n", "\n"+line+"\n") {
		return nil
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, []byte(line+"\n")...)
	return writeKnownHostsAtomically(path, data, deps.createTemp)
}

func osReleaseValue(output, name string) string {
	for _, line := range strings.Split(output, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			value = strings.Trim(value, "\"'")
			if len(value) > 80 {
				value = value[:80]
			}
			return value
		}
	}
	return ""
}

func onboardingDiskAvailable(output string) (int64, error) {
	minimum := int64(1<<63 - 1)
	rows := strings.Split(strings.TrimSpace(output), "\n")
	if len(rows) != 4 {
		return 0, errors.New("unexpected disk output")
	}
	for _, row := range rows[1:] {
		fields := strings.Fields(row)
		if len(fields) != 6 {
			return 0, errors.New("unexpected disk row")
		}
		available, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || available < 0 {
			return 0, errors.New("invalid free disk space")
		}
		if available < minimum {
			minimum = available
		}
	}
	return minimum, nil
}
