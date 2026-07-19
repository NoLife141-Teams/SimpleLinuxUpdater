package servers

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type KnownHostsDeps struct {
	DBPath              func() string
	UserHomeDir         func() (string, error)
	Getenv              func(string) string
	ScanHostKey         func(string, int) (ssh.PublicKey, error)
	KnownHostsMu        *sync.Mutex
	SSHConnectTimeout   time.Duration
	ConstantTimeCompare func(string, string) bool
}

func (d KnownHostsDeps) dbPath() string {
	if d.DBPath != nil {
		return d.DBPath()
	}
	return "updater.db"
}

func (d KnownHostsDeps) userHomeDir() (string, error) {
	if d.UserHomeDir != nil {
		return d.UserHomeDir()
	}
	return os.UserHomeDir()
}

func (d KnownHostsDeps) getenv(key string) string {
	if d.Getenv != nil {
		return d.Getenv(key)
	}
	return os.Getenv(key)
}

func (d KnownHostsDeps) knownHostsMu() *sync.Mutex {
	if d.KnownHostsMu != nil {
		return d.KnownHostsMu
	}
	return &fallbackKnownHostsMu
}

func (d KnownHostsDeps) scanHostKey(host string, port int) (ssh.PublicKey, error) {
	if d.ScanHostKey != nil {
		return d.ScanHostKey(host, port)
	}
	return ScanHostKey(host, port, d.SSHConnectTimeout)
}

func (d KnownHostsDeps) constantTimeCompare(a, b string) bool {
	if d.ConstantTimeCompare != nil {
		return d.ConstantTimeCompare(a, b)
	}
	return a == b
}

var fallbackKnownHostsMu sync.Mutex

func NormalizePort(port int) int {
	if port <= 0 || port > 65535 {
		return 22
	}
	return port
}

func ParseTags(raw string) []string {
	parts := strings.Split(raw, ",")
	var tags []string
	for _, part := range parts {
		tag := strings.TrimSpace(part)
		if tag != "" {
			tags = append(tags, tag)
		}
	}
	return tags
}

func JoinTags(tags []string) string {
	if len(tags) == 0 {
		return ""
	}
	cleaned := make([]string, 0, len(tags))
	seen := make(map[string]struct{})
	for _, tag := range tags {
		clean := strings.TrimSpace(tag)
		if clean == "" {
			continue
		}
		if _, exists := seen[clean]; exists {
			continue
		}
		seen[clean] = struct{}{}
		cleaned = append(cleaned, clean)
	}
	return strings.Join(cleaned, ", ")
}

func NormalizeServerName(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func NormalizeServerHost(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func ServerNameExists(servers []Server, name string, skipIndex int) bool {
	normalized := NormalizeServerName(name)
	for i, existing := range servers {
		if i == skipIndex {
			continue
		}
		if NormalizeServerName(existing.Name) == normalized {
			return true
		}
	}
	return false
}

func ServerHostExists(servers []Server, host string, skipIndex int) bool {
	normalized := NormalizeServerHost(host)
	for i, existing := range servers {
		if i == skipIndex {
			continue
		}
		if NormalizeServerHost(existing.Host) == normalized {
			return true
		}
	}
	return false
}

func IsValidSSHUsername(username string) bool {
	trimmed := strings.TrimSpace(username)
	if trimmed == "" || len(trimmed) > 64 {
		return false
	}
	for _, r := range trimmed {
		if (r >= 'a' && r <= 'z') ||
			(r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') ||
			r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func NormalizeApprovalScope(scope string) string {
	normalized := strings.ToLower(strings.TrimSpace(scope))
	switch normalized {
	case "security", "security_kept_back", "all", "full_upgrade":
		return normalized
	default:
		return "all"
	}
}

func KnownHostsPaths(deps KnownHostsDeps) []string {
	if raw := strings.TrimSpace(deps.getenv("DEBIAN_UPDATER_KNOWN_HOSTS")); raw != "" {
		parts := strings.Split(raw, ":")
		paths := make([]string, 0, len(parts))
		for _, part := range parts {
			path := strings.TrimSpace(part)
			if path != "" {
				paths = append(paths, path)
			}
		}
		return paths
	}
	paths := []string{filepath.Join(filepath.Dir(deps.dbPath()), "known_hosts")}
	if home, err := deps.userHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		paths = append(paths, filepath.Join(home, ".ssh", "known_hosts"))
	}
	paths = append(paths, "/etc/ssh/ssh_known_hosts")
	seen := make(map[string]struct{}, len(paths))
	unique := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		unique = append(unique, path)
	}
	return unique
}

func KnownHostsDefaultWritePath(deps KnownHostsDeps) string {
	return filepath.Join(filepath.Dir(deps.dbPath()), "known_hosts")
}

func HostKeyCallback(deps KnownHostsDeps) (ssh.HostKeyCallback, error) {
	candidates := KnownHostsPaths(deps)
	existing := make([]string, 0, len(candidates))
	for _, path := range candidates {
		if _, err := os.Stat(path); err == nil {
			existing = append(existing, path)
		}
	}
	if len(existing) == 0 {
		return nil, errors.New("no known_hosts file found; set DEBIAN_UPDATER_KNOWN_HOSTS or create ~/.ssh/known_hosts")
	}
	cb, err := knownhosts.New(existing...)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts: %w", err)
	}
	return cb, nil
}

func KnownHostsWritePath(deps KnownHostsDeps) (string, error) {
	if raw := strings.TrimSpace(deps.getenv("DEBIAN_UPDATER_KNOWN_HOSTS")); raw != "" {
		for _, part := range strings.Split(raw, ":") {
			path := strings.TrimSpace(part)
			if path != "" {
				return path, nil
			}
		}
		return "", errors.New("no known_hosts path configured")
	}
	return KnownHostsDefaultWritePath(deps), nil
}

func KnownHostsHostToken(host string, port int) string {
	cleanHost := strings.Trim(strings.TrimSpace(host), "[]")
	if NormalizePort(port) == 22 {
		return cleanHost
	}
	return fmt.Sprintf("[%s]:%d", cleanHost, NormalizePort(port))
}

func AppendKnownHostLine(deps KnownHostsDeps, line string) (bool, error) {
	cleanLine := strings.TrimSpace(line)
	if cleanLine == "" {
		return false, errors.New("empty known_hosts line")
	}
	path, err := KnownHostsWritePath(deps)
	if err != nil {
		return false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return false, fmt.Errorf("create known_hosts dir: %w", err)
	}
	knownHostsMu := deps.knownHostsMu()
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return false, fmt.Errorf("read known_hosts: %w", err)
	}
	if strings.Contains("\n"+string(data)+"\n", "\n"+cleanLine+"\n") {
		return false, nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return false, fmt.Errorf("open known_hosts for append: %w", err)
	}
	defer f.Close()
	if _, err := f.WriteString(cleanLine + "\n"); err != nil {
		return false, fmt.Errorf("append known_hosts line: %w", err)
	}
	return true, nil
}

func KnownHostLineExists(deps KnownHostsDeps, line string) (bool, error) {
	cleanLine := strings.TrimSpace(line)
	if cleanLine == "" {
		return false, errors.New("empty known_hosts line")
	}
	path, err := KnownHostsWritePath(deps)
	if err != nil {
		return false, err
	}
	knownHostsMu := deps.knownHostsMu()
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read known_hosts: %w", err)
	}
	return strings.Contains("\n"+string(data)+"\n", "\n"+cleanLine+"\n"), nil
}

func KnownHostEntryExists(deps KnownHostsDeps, host string, port int) (bool, error) {
	token := KnownHostsHostToken(host, port)
	if strings.TrimSpace(token) == "" {
		return false, errors.New("host is required")
	}
	path, err := KnownHostsWritePath(deps)
	if err != nil {
		return false, err
	}
	knownHostsMu := deps.knownHostsMu()
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read known_hosts: %w", err)
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		fields := strings.Fields(trimmed)
		if len(fields) == 0 {
			continue
		}
		for _, hostToken := range strings.Split(fields[0], ",") {
			if strings.TrimSpace(hostToken) == token {
				return true, nil
			}
		}
	}
	return false, nil
}

func RemoveKnownHostEntries(deps KnownHostsDeps, host string, port int) (int, error) {
	token := KnownHostsHostToken(host, port)
	if strings.TrimSpace(token) == "" {
		return 0, errors.New("host is required")
	}
	path, err := KnownHostsWritePath(deps)
	if err != nil {
		return 0, err
	}
	knownHostsMu := deps.knownHostsMu()
	knownHostsMu.Lock()
	defer knownHostsMu.Unlock()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read known_hosts: %w", err)
	}
	content := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(content, "\n")
	kept := make([]string, 0, len(lines))
	removed := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			kept = append(kept, line)
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 0 {
			kept = append(kept, line)
			continue
		}
		hostTokens := strings.Split(fields[0], ",")
		keptHostTokens := make([]string, 0, len(hostTokens))
		removedOnLine := 0
		for _, hostToken := range hostTokens {
			trimmedToken := strings.TrimSpace(hostToken)
			if trimmedToken == "" {
				continue
			}
			if trimmedToken == token {
				removedOnLine++
				continue
			}
			keptHostTokens = append(keptHostTokens, trimmedToken)
		}
		if removedOnLine == 0 {
			kept = append(kept, line)
			continue
		}
		removed += removedOnLine
		if len(keptHostTokens) == 0 {
			continue
		}
		fields[0] = strings.Join(keptHostTokens, ",")
		kept = append(kept, strings.Join(fields, " "))
	}
	if removed == 0 {
		return 0, nil
	}
	updated := strings.Join(kept, "\n")
	updated = strings.TrimRight(updated, "\n")
	if updated != "" {
		updated += "\n"
	}
	if err := os.WriteFile(path, []byte(updated), 0600); err != nil {
		return 0, fmt.Errorf("write known_hosts: %w", err)
	}
	return removed, nil
}

func ScanHostKey(host string, port int, timeout time.Duration) (ssh.PublicKey, error) {
	cleanHost := strings.TrimSpace(host)
	if cleanHost == "" {
		return nil, errors.New("host is required")
	}
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	address := net.JoinHostPort(cleanHost, strconv.Itoa(NormalizePort(port)))
	var scanned ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "hostkey-scan",
		Auth: []ssh.AuthMethod{
			ssh.Password("invalid"),
		},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			scanned = key
			return nil
		},
		Timeout: timeout,
	}
	client, err := ssh.Dial("tcp", address, cfg)
	if client != nil {
		_ = client.Close()
	}
	if err != nil {
		msg := strings.ToLower(err.Error())
		isAuthErr := strings.Contains(msg, "unable to authenticate") ||
			strings.Contains(msg, "no auth") ||
			strings.Contains(msg, "permission denied") ||
			strings.Contains(msg, "authentication")
		if scanned != nil && isAuthErr {
			return scanned, nil
		}
		return nil, err
	}
	if scanned != nil {
		return scanned, nil
	}
	return nil, errors.New("unable to scan host key")
}

func BuildKnownHostsLine(host string, port int, key ssh.PublicKey) string {
	return knownhosts.Line([]string{KnownHostsHostToken(host, port)}, key)
}

func TrustHostKey(deps KnownHostsDeps, host string, port int, expectedFingerprint string) (string, string, bool, error) {
	key, err := deps.scanHostKey(host, port)
	if err != nil {
		return "", "", false, err
	}
	fingerprint := ssh.FingerprintSHA256(key)
	if strings.TrimSpace(expectedFingerprint) != "" && !deps.constantTimeCompare(fingerprint, strings.TrimSpace(expectedFingerprint)) {
		return "", "", false, ErrFingerprintMismatch
	}
	line := BuildKnownHostsLine(host, port, key)
	added, err := AppendKnownHostLine(deps, line)
	if err != nil {
		return "", "", false, err
	}
	return fingerprint, line, !added, nil
}

func BuildAuthMethods(server Server) ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod
	key := strings.TrimSpace(server.Key)
	if key != "" {
		signer, err := ssh.ParsePrivateKey([]byte(key))
		if err != nil {
			return nil, fmt.Errorf("parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if server.Pass != "" {
		methods = append(methods, ssh.Password(server.Pass))
	}
	if len(methods) == 0 {
		return nil, errors.New("missing password or SSH key")
	}
	return methods, nil
}
