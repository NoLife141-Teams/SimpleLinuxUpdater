package main

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	appshell "debian-updater/internal/app"
	auditpkg "debian-updater/internal/audit"
	authpkg "debian-updater/internal/auth"
	"debian-updater/internal/events"
	observabilitypkg "debian-updater/internal/observability"
	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
	_ "modernc.org/sqlite"
)

var db *sql.DB
var dbOnce sync.Once
var keyOnce sync.Once
var encryptionKey []byte
var runtimeStateMu sync.RWMutex
var globalKeyMu sync.RWMutex
var globalKey string
var metricsBearerTokenHashMu sync.RWMutex
var metricsBearerTokenHash string
var metricsBearerTokenHashLoaded bool
var metricsBearerTokenHashDBPath string
var saveServersFunc = saveServers
var auditPruneTickerOnce sync.Once
var rebootCheckErrorRe = regexp.MustCompile(`\b(error|failed|failure|unable|cannot|can't)\b`)
var rebootRequiredPhraseRe = regexp.MustCompile(`\b(reboot required|requires reboot|restart required|system restart required|needs reboot|need reboot)\b`)

const configFileName = "config.json"
const legacyServersFileName = "servers.json"
const globalKeySetting = "global_ssh_key"
const metricsBearerTokenHashSetting = "metrics_bearer_token_hash"
const metricsBearerTokenEntropyBytes = 32
const maxUploadedKeyBytes = 64 * 1024
const maxUploadedKeyRequestBytes = maxUploadedKeyBytes + 1024*1024
const sshConnectTimeout = 15 * time.Second
const auditRetentionDays = 90
const auditPruneInterval = 12 * time.Hour
const retryMaxAttemptsEnv = "DEBIAN_UPDATER_RETRY_MAX_ATTEMPTS"
const retryBaseDelayMSEnv = "DEBIAN_UPDATER_RETRY_BASE_DELAY_MS"
const retryMaxDelayMSEnv = "DEBIAN_UPDATER_RETRY_MAX_DELAY_MS"
const retryJitterPctEnv = "DEBIAN_UPDATER_RETRY_JITTER_PCT"
const sshCommandTimeoutSecondsEnv = "DEBIAN_UPDATER_SSH_COMMAND_TIMEOUT_SECONDS"
const trustedProxiesEnv = "DEBIAN_UPDATER_TRUSTED_PROXIES"
const postchecksEnabledEnv = "DEBIAN_UPDATER_POSTCHECKS_ENABLED"
const postcheckBlockOnAptHealthEnv = "DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_APT_HEALTH"
const postcheckBlockOnFailedUnitsEnv = "DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_FAILED_UNITS"
const postcheckRebootRequiredWarningEnv = "DEBIAN_UPDATER_POSTCHECK_REBOOT_REQUIRED_WARNING"
const postcheckCustomCmdEnv = "DEBIAN_UPDATER_POSTCHECK_CMD"
const updatePrecheckMinFreeKB int64 = 1024 * 1024
const precheckOutputMaxLen = 240
const precheckDiskSpaceCmd = "df -Pk /var / | awk 'NR>1 {print $2, $4}'"

var precheckLocksCmd = updatespkg.RootOrSudoCommand("/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock")
var precheckDpkgAuditCmd = updatespkg.RootOrSudoCommand("dpkg --audit")
var precheckAptCheckCmd = updatespkg.RootOrSudoCommand("apt-get check")
var aptUpdateCmd = updatespkg.AptUpdateCmd
var aptUpgradeCmd = updatespkg.AptUpgradeCmd
var aptAutoremoveCmd = updatespkg.AptAutoremoveCmd
var aptListUpgradableCmd = updatespkg.AptListUpgradableCmd
var aptListMetadataCmd = updatespkg.AptListMetadataCmd
var aptFullUpgradeSimCmd = updatespkg.AptFullUpgradeSimCmd

const defaultSSHCommandTimeout = 5 * time.Minute
const minSSHCommandTimeout = 1 * time.Second
const maxSSHCommandTimeout = 30 * time.Minute
const cveLookupMaxPerPackage = 12
const cveLookupCommandTimeout = 20 * time.Second
const postcheckFailedUnitsCmd = "systemctl --failed --no-legend --plain"
const postcheckRebootRequiredCmd = "sh -c \"if [ -f /var/run/reboot-required ]; then echo required; fi\""
const postcheckNameAptHealth = "post_apt_health"
const sqliteBusyTimeoutMS = 5000
const postcheckNameFailedUnits = "failed_units"
const postcheckNameRebootRequired = "reboot_required"
const postcheckNameCustomCmd = "custom_command"
const updateCompleteAction = "update.complete"
const serverFactsRefreshAction = "server.facts.refresh"
const serverFactsOSCmd = "sh -c '. /etc/os-release 2>/dev/null; printf \"%s\\n\" \"${PRETTY_NAME:-unknown}\"'"
const serverFactsUptimeCmd = "cat /proc/uptime"
const defaultContentSecurityPolicy = "default-src 'self'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'; object-src 'none'; script-src 'self'; style-src 'self' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'"

var errUploadedKeyTooLarge = errors.New("key file too large (max 64KB)")
var errUploadedKeyEmpty = errors.New("empty key")
var errInvalidWindow = observabilitypkg.ErrInvalidWindow

var dashboardEventBroker = events.NewBroker()

func notifyDashboardEvent(reason string) {
	if dashboardEventBroker != nil {
		dashboardEventBroker.Publish(reason)
	}
}

type serverFactsRecord = updatespkg.ServerFactsRecord

type RetryPolicy = updatespkg.RetryPolicy
type PostUpdateCheckConfig = updatespkg.PostUpdateCheckConfig
type updatePrecheckResult = updatespkg.PrecheckResult
type updatePrecheckSummary = updatespkg.PrecheckSummary
type updatePostcheckSummary = updatespkg.PostcheckSummary
type sshSessionRunner = updatespkg.SSHSessionRunner
type sshConnection = updatespkg.SSHConnection

type realSSHSession struct {
	session *ssh.Session
}

func (s *realSSHSession) SetStdin(r io.Reader)  { s.session.Stdin = r }
func (s *realSSHSession) SetStdout(w io.Writer) { s.session.Stdout = w }
func (s *realSSHSession) SetStderr(w io.Writer) { s.session.Stderr = w }
func (s *realSSHSession) Run(cmd string) error  { return s.session.Run(cmd) }
func (s *realSSHSession) Close() error          { return s.session.Close() }

type realSSHConnection struct {
	client *ssh.Client
}

func (c *realSSHConnection) NewSession() (sshSessionRunner, error) {
	session, err := c.client.NewSession()
	if err != nil {
		return nil, err
	}
	return &realSSHSession{session: session}, nil
}

func (c *realSSHConnection) Close() error {
	return c.client.Close()
}

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

var updateRunnerWG sync.WaitGroup

func startTrackedActionRunner(run func()) {
	updateRunnerWG.Add(1)
	go func() {
		defer updateRunnerWG.Done()
		run()
	}()
}

func waitForUpdateRunners() {
	updateRunnerWG.Wait()
}

func dbPath() string {
	if p := strings.TrimSpace(os.Getenv("DEBIAN_UPDATER_DB_PATH")); p != "" {
		return p
	}
	if dirExists("/data") {
		return filepath.Join("/data", "servers.db")
	}
	return filepath.Join("data", "servers.db")
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

func configPath() string {
	return filepath.Join(filepath.Dir(dbPath()), configFileName)
}

func ensurePrivateDirForFile(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return err
	}
	return nil
}

func chmodIfExists(path string, mode os.FileMode) error {
	if err := os.Chmod(path, mode); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func hardenRuntimeDataFilePermissions(path string) error {
	if err := chmodIfExists(path, 0600); err != nil {
		return err
	}
	for _, sidecar := range sqliteSidecarPaths(path) {
		if err := chmodIfExists(sidecar, 0600); err != nil {
			return err
		}
	}
	return nil
}

func getDB() *sql.DB {
	runtimeStateMu.RLock()
	if db != nil {
		cached := db
		runtimeStateMu.RUnlock()
		return cached
	}
	runtimeStateMu.RUnlock()

	runtimeStateMu.Lock()
	defer runtimeStateMu.Unlock()
	dbOnce.Do(func() {
		path := dbPath()
		if err := ensurePrivateDirForFile(path); err != nil {
			log.Fatalf("Failed to create db directory: %v", err)
		}
		var err error
		db, err = sql.Open("sqlite", path)
		if err != nil {
			log.Fatalf("Failed to open sqlite db: %v", err)
		}
		// Keep a single connection and set a busy timeout to avoid SQLITE_BUSY
		// errors when multiple API requests concurrently touch sessions/audit.
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if _, err := db.Exec(fmt.Sprintf("PRAGMA busy_timeout=%d", sqliteBusyTimeoutMS)); err != nil {
			log.Fatalf("Failed to set sqlite busy_timeout: %v", err)
		}
		if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
			log.Fatalf("Failed to set sqlite journal_mode: %v", err)
		}
		if _, err := db.Exec("PRAGMA synchronous=NORMAL"); err != nil {
			log.Fatalf("Failed to set sqlite synchronous mode: %v", err)
		}
		if err := ensureSchema(db); err != nil {
			log.Fatalf("Failed to migrate db schema: %v", err)
		}
		if err := hardenRuntimeDataFilePermissions(path); err != nil {
			log.Fatalf("Failed to harden db file permissions: %v", err)
		}
	})
	return db
}

func decodeEncryptionKeyValue(keyStr string) ([]byte, error) {
	keyStr = strings.TrimSpace(keyStr)
	if keyStr == "" {
		return nil, errors.New("missing encryption_key")
	}
	keyBytes, err := base64.StdEncoding.DecodeString(keyStr)
	if err != nil {
		return nil, err
	}
	if len(keyBytes) != 32 {
		return nil, errors.New("encryption_key must be base64 32 bytes")
	}
	return keyBytes, nil
}

func getEncryptionKey() []byte {
	keyOnce.Do(func() {
		path := configPath()
		if err := ensurePrivateDirForFile(path); err != nil {
			log.Fatalf("Failed to create config dir: %v", err)
		}
		var cfg map[string]string
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &cfg); err != nil {
				log.Fatalf("Failed to parse %s: %v", path, err)
			}
			if err := chmodIfExists(path, 0600); err != nil {
				log.Fatalf("Failed to harden %s permissions: %v", path, err)
			}
		} else if !os.IsNotExist(err) {
			log.Fatalf("Failed to read %s: %v", path, err)
		}

		keyStr := ""
		if cfg != nil {
			keyStr = strings.TrimSpace(cfg["encryption_key"])
		}

		if keyStr == "" {
			keyBytes := make([]byte, 32)
			if _, err := rand.Read(keyBytes); err != nil {
				log.Fatalf("Failed to generate encryption key: %v", err)
			}
			keyStr = base64.StdEncoding.EncodeToString(keyBytes)
			cfg = map[string]string{"encryption_key": keyStr}
			data, err := json.MarshalIndent(cfg, "", "  ")
			if err != nil {
				log.Fatalf("Failed to serialize config: %v", err)
			}
			if err := os.WriteFile(path, data, 0600); err != nil {
				log.Fatalf("Failed to write %s: %v", path, err)
			}
			if err := chmodIfExists(path, 0600); err != nil {
				log.Fatalf("Failed to harden %s permissions: %v", path, err)
			}
		}

		keyBytes, err := decodeEncryptionKeyValue(keyStr)
		if err != nil || len(keyBytes) != 32 {
			log.Fatalf("Invalid encryption_key in %s (must be base64 32 bytes)", path)
		}

		runtimeStateMu.Lock()
		encryptionKey = keyBytes
		runtimeStateMu.Unlock()
	})

	runtimeStateMu.RLock()
	key := encryptionKey
	runtimeStateMu.RUnlock()
	if key == nil {
		log.Fatalf("Encryption key initialization failed")
	}
	return key
}

func ensureSchema(db *sql.DB) error {
	if err := serverpkg.EnsureSchema(db); err != nil {
		return err
	}
	if err := ensureSettingsSchema(db); err != nil {
		return err
	}
	if err := authpkg.EnsureSchema(db); err != nil {
		return err
	}
	if err := auditpkg.EnsureSchema(db); err != nil {
		return err
	}
	if err := updatespkg.EnsureServerFactsSchema(db); err != nil {
		return err
	}
	if err := ensureJobSchema(db); err != nil {
		return err
	}
	return ensureUpdatePolicySchema(db)
}

func ensureSettingsSchema(db *sql.DB) error {
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)"); err != nil {
		return err
	}
	return nil
}

func truncateString(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= maxLen {
		return string(runes)
	}
	return string(runes[:maxLen])
}

func normalizeAuditFilterTimestamp(raw string) (string, error) {
	parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	return parsed.UTC().Format(time.RFC3339), nil
}

func updateCompletionOutcome(finalStatus string) string {
	return updatespkg.UpdateCompletionOutcome(finalStatus)
}

func parseIntEnvWithDefault(envKey string, defaultValue int) int {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return defaultValue
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("Invalid %s=%q, using default %d", envKey, raw, defaultValue)
		return defaultValue
	}
	return parsed
}

func parseBoolEnvWithDefault(envKey string, defaultValue bool) bool {
	raw := strings.TrimSpace(os.Getenv(envKey))
	if raw == "" {
		return defaultValue
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		log.Printf("Invalid %s=%q, using default %t", envKey, raw, defaultValue)
		return defaultValue
	}
	return parsed
}

func loadRetryPolicyFromEnv() RetryPolicy {
	p := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   1 * time.Second,
		MaxDelay:    8 * time.Second,
		JitterPct:   20,
	}
	attempts := parseIntEnvWithDefault(retryMaxAttemptsEnv, p.MaxAttempts)
	if attempts < 1 || attempts > 10 {
		log.Printf("Invalid %s=%d, must be in [1,10], using default %d", retryMaxAttemptsEnv, attempts, p.MaxAttempts)
	} else {
		p.MaxAttempts = attempts
	}
	baseDelayMs := parseIntEnvWithDefault(retryBaseDelayMSEnv, int(p.BaseDelay/time.Millisecond))
	if baseDelayMs <= 0 {
		log.Printf("Invalid %s=%d, must be > 0, using default %d", retryBaseDelayMSEnv, baseDelayMs, int(p.BaseDelay/time.Millisecond))
	} else {
		p.BaseDelay = time.Duration(baseDelayMs) * time.Millisecond
	}
	maxDelayMs := parseIntEnvWithDefault(retryMaxDelayMSEnv, int(p.MaxDelay/time.Millisecond))
	if maxDelayMs <= 0 {
		log.Printf("Invalid %s=%d, must be > 0, using default %d", retryMaxDelayMSEnv, maxDelayMs, int(p.MaxDelay/time.Millisecond))
	} else {
		p.MaxDelay = time.Duration(maxDelayMs) * time.Millisecond
	}
	if p.MaxDelay < p.BaseDelay {
		log.Printf("Invalid retry delay configuration: max delay %v lower than base delay %v, using defaults", p.MaxDelay, p.BaseDelay)
		p.BaseDelay = 1 * time.Second
		p.MaxDelay = 8 * time.Second
	}
	jitterPct := parseIntEnvWithDefault(retryJitterPctEnv, p.JitterPct)
	if jitterPct < 0 || jitterPct > 50 {
		log.Printf("Invalid %s=%d, must be in [0,50], using default %d", retryJitterPctEnv, jitterPct, p.JitterPct)
	} else {
		p.JitterPct = jitterPct
	}
	return p
}

func loadSSHCommandTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(sshCommandTimeoutSecondsEnv))
	if raw == "" {
		return defaultSSHCommandTimeout
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil {
		log.Printf("Invalid %s=%q, must be an integer in [1,1800], using default %s", sshCommandTimeoutSecondsEnv, raw, defaultSSHCommandTimeout)
		return defaultSSHCommandTimeout
	}
	timeout := time.Duration(seconds) * time.Second
	if timeout < minSSHCommandTimeout || timeout > maxSSHCommandTimeout {
		log.Printf("Invalid %s=%d, must be in [1,1800], using default %s", sshCommandTimeoutSecondsEnv, seconds, defaultSSHCommandTimeout)
		return defaultSSHCommandTimeout
	}
	return timeout
}

func loadPostUpdateCheckConfigFromEnv() PostUpdateCheckConfig {
	cfg := PostUpdateCheckConfig{
		Enabled:               true,
		BlockOnAptHealth:      true,
		BlockOnFailedUnits:    true,
		RebootRequiredWarning: true,
		CustomCommand:         "",
	}
	cfg.Enabled = parseBoolEnvWithDefault(postchecksEnabledEnv, cfg.Enabled)
	cfg.BlockOnAptHealth = parseBoolEnvWithDefault(postcheckBlockOnAptHealthEnv, cfg.BlockOnAptHealth)
	cfg.BlockOnFailedUnits = parseBoolEnvWithDefault(postcheckBlockOnFailedUnitsEnv, cfg.BlockOnFailedUnits)
	cfg.RebootRequiredWarning = parseBoolEnvWithDefault(postcheckRebootRequiredWarningEnv, cfg.RebootRequiredWarning)
	cfg.CustomCommand = strings.TrimSpace(os.Getenv(postcheckCustomCmdEnv))
	return cfg
}

func isRetryableError(err error) bool {
	return updatespkg.IsRetryableError(err)
}

func markRetryableFromOutput(err error, output string) error {
	return updatespkg.MarkRetryableFromOutput(err, output)
}

func runWithRetryWithSleep(
	policy RetryPolicy,
	opName string,
	fn func() error,
	onRetry func(attempt int, wait time.Duration, err error),
	sleepFn func(time.Duration),
) error {
	return updatespkg.RunWithRetryWithSleep(policy, opName, fn, onRetry, sleepFn, log.Printf)
}

func runWithRetry(policy RetryPolicy, opName string, fn func() error, onRetry func(attempt int, wait time.Duration, err error)) error {
	return updatespkg.RunWithRetry(policy, opName, fn, onRetry, log.Printf)
}

func reconnectSSHClient(server Server, config *ssh.ClientConfig, clientRef *sshConnection) error {
	if clientRef != nil && *clientRef != nil {
		(*clientRef).Close()
		*clientRef = nil
	}
	dial := getDialSSHConnection()
	conn, err := dial(server, config)
	if err != nil {
		return err
	}
	if clientRef != nil {
		*clientRef = conn
	} else {
		conn.Close()
	}
	return nil
}

func appendStatusRetryLog(serverName string, format string, args ...any) {
	mu.Lock()
	if status := statusMap[serverName]; status != nil {
		status.Logs += fmt.Sprintf(format, args...)
	}
	mu.Unlock()
}

func dialSSHWithRetry(server Server, config *ssh.ClientConfig, policy RetryPolicy, opName string, attemptsUsed *int) (sshConnection, error) {
	var client sshConnection
	err := runWithRetry(policy, opName, func() error {
		if attemptsUsed != nil {
			*attemptsUsed += 1
		}
		dial := getDialSSHConnection()
		c, dialErr := dial(server, config)
		if dialErr == nil {
			if client != nil {
				_ = client.Close()
			}
			client = c
		}
		return dialErr
	}, func(attempt int, wait time.Duration, retryErr error) {
		appendStatusRetryLog(
			server.Name,
			"\nSSH dial attempt %d/%d failed: %v; retrying in %s",
			attempt,
			policy.MaxAttempts,
			retryErr,
			wait.Round(time.Millisecond),
		)
	})
	return client, err
}

func runSSHOperationWithRetry(
	server Server,
	config *ssh.ClientConfig,
	clientRef *sshConnection,
	policy RetryPolicy,
	opName string,
	retryLogFormat string,
	attemptsUsed *int,
	operation func() error,
) error {
	attempt := 0
	return runWithRetry(policy, opName, func() error {
		if attemptsUsed != nil {
			*attemptsUsed += 1
		}
		attempt++
		if attempt > 1 {
			if reconnectErr := reconnectSSHClient(server, config, clientRef); reconnectErr != nil {
				return reconnectErr
			}
		}
		return operation()
	}, func(retryAttempt int, wait time.Duration, retryErr error) {
		appendStatusRetryLog(
			server.Name,
			retryLogFormat,
			retryAttempt,
			policy.MaxAttempts,
			retryErr,
			wait.Round(time.Millisecond),
		)
	})
}

func compactCommandOutput(stdout, stderr string) string {
	combined := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
	if combined == "" {
		return ""
	}
	return truncateString(combined, precheckOutputMaxLen)
}

func kbToGiB(kb int64) float64 {
	return float64(kb) / (1024.0 * 1024.0)
}

func isSudoPolicyOrPasswordError(msg string) bool {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(normalized, "a password is required") ||
		strings.Contains(normalized, "not allowed to run sudo") ||
		strings.Contains(normalized, "is not in the sudoers file")
}

func isSudoCommandNotFoundError(msg string) bool {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(normalized, "sudo: command not found") ||
		strings.Contains(normalized, "sudo: not found")
}

func isCommandNotFoundError(msg string) bool {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	return strings.Contains(normalized, "/usr/bin/fuser: command not found") ||
		strings.Contains(normalized, "/usr/bin/fuser: not found") ||
		strings.Contains(normalized, "unable to execute /usr/bin/fuser") ||
		strings.Contains(normalized, "sudo: /usr/bin/fuser: no such file or directory") ||
		(strings.Contains(normalized, "command not found") && strings.Contains(normalized, "fuser"))
}

func isBenignNoLockStateOutput(msg string) bool {
	normalized := strings.ToLower(strings.TrimSpace(msg))
	if normalized == "" {
		return true
	}
	if strings.Contains(normalized, "no process found") {
		return true
	}
	lockPathMentioned := strings.Contains(normalized, "/var/lib/dpkg/lock-frontend") ||
		strings.Contains(normalized, "/var/lib/dpkg/lock") ||
		strings.Contains(normalized, "/var/cache/apt/archives/lock")
	if lockPathMentioned && (strings.Contains(normalized, "does not exist") || strings.Contains(normalized, "no such file or directory")) {
		return true
	}
	return false
}

func runSSHCommandNoTimeout(client sshConnection, cmd string, stdin io.Reader) (string, string, error) {
	if client == nil {
		return "", "", errors.New("missing SSH connection")
	}
	session, err := client.NewSession()
	if err != nil {
		return "", "", err
	}
	defer session.Close()
	var stdout, stderr bytes.Buffer
	session.SetStdout(&stdout)
	session.SetStderr(&stderr)
	if stdin != nil {
		session.SetStdin(stdin)
	}
	err = session.Run(cmd)
	return stdout.String(), stderr.String(), err
}

func runSSHCommandWithTimeout(client sshConnection, cmd string, stdin io.Reader, timeout time.Duration) (string, string, error) {
	if timeout <= 0 {
		return runSSHCommandNoTimeout(client, cmd, stdin)
	}
	if client == nil {
		return "", "", errors.New("missing SSH connection")
	}
	type sessionResult struct {
		session sshSessionRunner
		err     error
	}
	newSessionCh := make(chan sessionResult, 1)
	go func() {
		session, err := client.NewSession()
		newSessionCh <- sessionResult{session: session, err: err}
	}()

	sessionTimeout := time.NewTimer(timeout)
	defer sessionTimeout.Stop()

	var session sshSessionRunner
	select {
	case result := <-newSessionCh:
		if result.err != nil {
			return "", "", result.err
		}
		session = result.session
	case <-sessionTimeout.C:
		_ = client.Close()
		select {
		case result := <-newSessionCh:
			if result.session != nil {
				_ = result.session.Close()
			}
			if result.err == nil {
				return "", "", fmt.Errorf("ssh session setup timed out after %s", timeout)
			}
			return "", "", fmt.Errorf("ssh session setup timed out after %s: %w", timeout, result.err)
		case <-time.After(1 * time.Second):
			go func() {
				result := <-newSessionCh
				if result.session != nil {
					_ = result.session.Close()
				}
			}()
			return "", "", fmt.Errorf("ssh session setup timed out after %s", timeout)
		}
	}

	var stdout, stderr bytes.Buffer
	session.SetStdout(&stdout)
	session.SetStderr(&stderr)
	if stdin != nil {
		session.SetStdin(stdin)
	}

	runErrCh := make(chan error, 1)
	go func() {
		runErrCh <- session.Run(cmd)
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case runErr := <-runErrCh:
		_ = session.Close()
		return stdout.String(), stderr.String(), runErr
	case <-timer.C:
		_ = session.Close()
		select {
		case runErr := <-runErrCh:
			timeoutStdout := stdout.String()
			timeoutStderr := stderr.String()
			if runErr == nil {
				runErr = fmt.Errorf("command timed out after %s", timeout)
			} else {
				runErr = fmt.Errorf("command timed out after %s: %w", timeout, runErr)
			}
			return timeoutStdout, timeoutStderr, runErr
		case <-time.After(1 * time.Second):
			go func() { <-runErrCh }()
			return "", "", fmt.Errorf("command timed out after %s", timeout)
		}
	}
}

func runSSHCommand(client sshConnection, cmd string, stdin io.Reader) (string, string, error) {
	return runSSHCommandWithTimeout(client, cmd, stdin, loadSSHCommandTimeoutFromEnv())
}

func sshExitCode(err error) (int, bool) {
	return updatespkg.SSHExitCode(err)
}

func checkDiskSpace(client sshConnection) updatePrecheckResult {
	stdout, stderr, err := runSSHCommand(client, precheckDiskSpaceCmd, nil)
	output := compactCommandOutput(stdout, stderr)
	if err != nil {
		return updatePrecheckResult{
			Name:    "disk_space",
			Passed:  false,
			Details: fmt.Sprintf("Failed to read free disk space: %v", err),
			Output:  output,
		}
	}
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return updatePrecheckResult{
			Name:    "disk_space",
			Passed:  false,
			Details: "Could not parse free disk space output.",
			Output:  output,
		}
	}
	minFreeKB := int64(-1)
	for _, field := range fields {
		value, convErr := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if convErr != nil {
			return updatePrecheckResult{
				Name:    "disk_space",
				Passed:  false,
				Details: fmt.Sprintf("Invalid free space value %q.", field),
				Output:  output,
			}
		}
		if minFreeKB == -1 || value < minFreeKB {
			minFreeKB = value
		}
	}
	if minFreeKB < updatePrecheckMinFreeKB {
		return updatePrecheckResult{
			Name:    "disk_space",
			Passed:  false,
			Details: fmt.Sprintf("Insufficient disk space: %.2f GiB free (minimum %.2f GiB).", kbToGiB(minFreeKB), kbToGiB(updatePrecheckMinFreeKB)),
			Output:  "",
		}
	}
	return updatePrecheckResult{
		Name:    "disk_space",
		Passed:  true,
		Details: fmt.Sprintf("Disk space OK: %.2f GiB free (minimum %.2f GiB).", kbToGiB(minFreeKB), kbToGiB(updatePrecheckMinFreeKB)),
		Output:  "",
	}
}

func checkAptLocks(client sshConnection) updatePrecheckResult {
	stdout, stderr, err := runSSHCommand(client, precheckLocksCmd, nil)
	output := compactCommandOutput(stdout, stderr)
	if err == nil {
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  false,
			Details: "APT/DPKG lock files are currently in use.",
			Output:  output,
		}
	}
	if isSudoPolicyOrPasswordError(output + "\n" + err.Error()) {
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  false,
			Details: "Lock pre-check requires passwordless sudo for `/usr/bin/fuser`. Click \"Enable passwordless apt\" for this server, then retry.",
			Output:  output,
		}
	}
	if isSudoCommandNotFoundError(output + "\n" + err.Error()) {
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  false,
			Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.",
			Output:  output,
		}
	}
	if isCommandNotFoundError(output + "\n" + err.Error()) {
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  false,
			Details: "Lock check command not found. Install package `psmisc` (provides /usr/bin/fuser).",
			Output:  output,
		}
	}
	if exitCode, ok := sshExitCode(err); ok && exitCode == 1 {
		trimmedOutput := strings.TrimSpace(output)
		if !isBenignNoLockStateOutput(trimmedOutput) {
			return updatePrecheckResult{
				Name:    "apt_locks",
				Passed:  false,
				Details: "Could not determine apt/dpkg lock state from lock check output.",
				Output:  output,
			}
		}
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  true,
			Details: "No apt/dpkg lock contention detected.",
			Output:  output,
		}
	}
	if exitCode, ok := sshExitCode(err); ok && exitCode == 127 {
		return updatePrecheckResult{
			Name:    "apt_locks",
			Passed:  false,
			Details: "Lock check command failed because a required command was not found. Install `sudo` for non-root users or `psmisc` for `/usr/bin/fuser`.",
			Output:  output,
		}
	}
	return updatePrecheckResult{
		Name:    "apt_locks",
		Passed:  false,
		Details: fmt.Sprintf("Failed to evaluate apt/dpkg lock state: %v", err),
		Output:  output,
	}
}

func runAptHealthCheck(client sshConnection, checkName string) updatePrecheckResult {
	dpkgStdout, dpkgStderr, dpkgErr := runSSHCommand(client, precheckDpkgAuditCmd, nil)
	dpkgOutput := compactCommandOutput(dpkgStdout, dpkgStderr)
	if dpkgErr != nil {
		if isSudoPolicyOrPasswordError(dpkgOutput + "\n" + dpkgErr.Error()) {
			return updatePrecheckResult{
				Name:    checkName,
				Passed:  false,
				Details: "APT health pre-check requires passwordless sudo for `/usr/bin/dpkg`. Click \"Enable passwordless apt\" for this server, then retry.",
				Output:  dpkgOutput,
			}
		}
		if isSudoCommandNotFoundError(dpkgOutput + "\n" + dpkgErr.Error()) {
			return updatePrecheckResult{
				Name:    checkName,
				Passed:  false,
				Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.",
				Output:  dpkgOutput,
			}
		}
		return updatePrecheckResult{
			Name:    checkName,
			Passed:  false,
			Details: fmt.Sprintf("dpkg audit failed: %v", dpkgErr),
			Output:  dpkgOutput,
		}
	}
	if strings.TrimSpace(dpkgStdout+dpkgStderr) != "" {
		return updatePrecheckResult{
			Name:    checkName,
			Passed:  false,
			Details: "dpkg audit reported package state issues.",
			Output:  dpkgOutput,
		}
	}
	aptStdout, aptStderr, aptErr := runSSHCommand(client, precheckAptCheckCmd, nil)
	aptOutput := compactCommandOutput(aptStdout, aptStderr)
	if aptErr != nil {
		if isSudoPolicyOrPasswordError(aptOutput + "\n" + aptErr.Error()) {
			return updatePrecheckResult{
				Name:    checkName,
				Passed:  false,
				Details: "APT health pre-check requires passwordless sudo for `/usr/bin/apt-get`. Click \"Enable passwordless apt\" for this server, then retry.",
				Output:  aptOutput,
			}
		}
		if isSudoCommandNotFoundError(aptOutput + "\n" + aptErr.Error()) {
			return updatePrecheckResult{
				Name:    checkName,
				Passed:  false,
				Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.",
				Output:  aptOutput,
			}
		}
		return updatePrecheckResult{
			Name:    checkName,
			Passed:  false,
			Details: fmt.Sprintf("apt-get check failed: %v", aptErr),
			Output:  aptOutput,
		}
	}
	return updatePrecheckResult{
		Name:    checkName,
		Passed:  true,
		Details: "APT health checks passed.",
		Output:  compactCommandOutput(dpkgOutput, aptOutput),
	}
}

func checkAptHealth(client sshConnection) updatePrecheckResult {
	return runAptHealthCheck(client, "apt_health")
}

func checkPostAptHealth(client sshConnection) updatePrecheckResult {
	result := runAptHealthCheck(client, postcheckNameAptHealth)
	result.Details = strings.Replace(result.Details, "pre-check", "post-check", 1)
	return result
}

func listFailedSystemdUnits(client sshConnection) ([]string, string, error) {
	stdout, stderr, err := runSSHCommand(client, postcheckFailedUnitsCmd, nil)
	output := compactCommandOutput(stdout, stderr)
	if err != nil {
		return nil, output, err
	}
	units := updatespkg.ParseFailedSystemdUnits(stdout)
	return units, output, nil
}

func checkFailedSystemdUnits(client sshConnection, preUpdateFailedUnits map[string]struct{}) updatePrecheckResult {
	units, output, err := listFailedSystemdUnits(client)
	if err != nil {
		return updatePrecheckResult{
			Name:    postcheckNameFailedUnits,
			Passed:  false,
			Details: fmt.Sprintf("failed to evaluate systemd unit health: %v", err),
			Output:  output,
		}
	}
	if len(units) == 0 {
		return updatePrecheckResult{
			Name:    postcheckNameFailedUnits,
			Passed:  true,
			Details: "No failed systemd units detected.",
			Output:  "",
		}
	}
	newlyFailed := make([]string, 0, len(units))
	for _, unit := range units {
		if _, existedBefore := preUpdateFailedUnits[unit]; existedBefore {
			continue
		}
		newlyFailed = append(newlyFailed, unit)
	}
	if len(newlyFailed) == 0 {
		return updatePrecheckResult{
			Name:    postcheckNameFailedUnits,
			Passed:  true,
			Details: fmt.Sprintf("No new failed systemd units detected after upgrade (%d pre-existing).", len(units)),
			Output:  output,
		}
	}
	return updatePrecheckResult{
		Name:    postcheckNameFailedUnits,
		Passed:  false,
		Details: "systemd reports newly failed units after upgrade.",
		Output: func() string {
			fullOutput := strings.Join(newlyFailed, "\n")
			if trimmed := strings.TrimSpace(output); trimmed != "" {
				fullOutput += "\n\n" + trimmed
			}
			return truncateString(fullOutput, precheckOutputMaxLen)
		}(),
	}
}

func checkRebootRequired(client sshConnection) updatePrecheckResult {
	stdout, stderr, err := runSSHCommand(client, postcheckRebootRequiredCmd, nil)
	output := compactCommandOutput(stdout, stderr)
	if err != nil {
		return updatePrecheckResult{
			Name:    postcheckNameRebootRequired,
			Passed:  false,
			Details: fmt.Sprintf("failed to evaluate reboot-required state: %v", err),
			Output:  output,
			Error:   err.Error(),
		}
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(stdout)), "required") {
		return updatePrecheckResult{
			Name:    postcheckNameRebootRequired,
			Passed:  false,
			Details: "Reboot required to fully apply updates.",
			Output:  output,
		}
	}
	return updatePrecheckResult{
		Name:    postcheckNameRebootRequired,
		Passed:  true,
		Details: "No reboot requirement detected.",
		Output:  "",
	}
}

func checkCustomPostUpdateCommand(client sshConnection, cmd string) updatePrecheckResult {
	stdout, stderr, err := runSSHCommand(client, cmd, nil)
	output := compactCommandOutput(stdout, stderr)
	if err != nil {
		return updatePrecheckResult{
			Name:    postcheckNameCustomCmd,
			Passed:  false,
			Details: fmt.Sprintf("custom post-check command failed: %v", err),
			Output:  output,
		}
	}
	return updatePrecheckResult{
		Name:    postcheckNameCustomCmd,
		Passed:  true,
		Details: "Custom post-check command passed.",
		Output:  output,
	}
}

func isPostcheckFailureBlocking(name string, cfg PostUpdateCheckConfig) bool {
	switch name {
	case postcheckNameAptHealth:
		return cfg.BlockOnAptHealth
	case postcheckNameFailedUnits:
		return cfg.BlockOnFailedUnits
	case postcheckNameRebootRequired:
		return false
	case postcheckNameCustomCmd:
		// Custom command runs only when configured and is blocking by design.
		return strings.TrimSpace(cfg.CustomCommand) != ""
	default:
		return true
	}
}

func runPostUpdateHealthChecks(client sshConnection, cfg PostUpdateCheckConfig, preUpdateFailedUnits map[string]struct{}) updatePostcheckSummary {
	summary := updatePostcheckSummary{
		AllPassed: true,
		Results:   make([]updatePrecheckResult, 0, 4),
	}
	if !cfg.Enabled {
		return summary
	}
	checks := []func(sshConnection) updatePrecheckResult{
		checkPostAptHealth,
		func(client sshConnection) updatePrecheckResult {
			return checkFailedSystemdUnits(client, preUpdateFailedUnits)
		},
	}
	if cfg.RebootRequiredWarning {
		checks = append(checks, checkRebootRequired)
	}
	for _, check := range checks {
		result := check(client)
		summary.Results = append(summary.Results, result)
		if result.Passed {
			continue
		}
		if isPostcheckFailureBlocking(result.Name, cfg) {
			summary.AllPassed = false
			if summary.FailedCheck == "" {
				summary.FailedCheck = result.Name
			}
			continue
		}
		summary.Warnings++
	}
	if strings.TrimSpace(cfg.CustomCommand) != "" {
		result := checkCustomPostUpdateCommand(client, cfg.CustomCommand)
		summary.Results = append(summary.Results, result)
		if !result.Passed {
			summary.AllPassed = false
			if summary.FailedCheck == "" {
				summary.FailedCheck = result.Name
			}
		}
	}
	return summary
}

func runUpdatePrechecks(client sshConnection) updatePrecheckSummary {
	checks := []func(sshConnection) updatePrecheckResult{
		checkDiskSpace,
		checkAptLocks,
		checkAptHealth,
	}
	summary := updatePrecheckSummary{
		AllPassed: true,
		Results:   make([]updatePrecheckResult, 0, len(checks)),
	}
	for _, check := range checks {
		result := check(client)
		summary.Results = append(summary.Results, result)
		if !result.Passed {
			summary.AllPassed = false
			summary.FailedCheck = result.Name
			return summary
		}
	}
	return summary
}

func handleAuditEvents(c *gin.Context) {
	handleAuditEventsWithService(c, defaultAuditService())
}

func handleAuditEventsWithService(c *gin.Context, service *AuditService) {
	if service == nil {
		service = defaultAuditService()
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	if page < 1 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "50"))
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	targetName := strings.TrimSpace(c.Query("target_name"))
	action := strings.TrimSpace(c.Query("action"))
	status := strings.TrimSpace(c.Query("status"))
	from := strings.TrimSpace(c.Query("from"))
	to := strings.TrimSpace(c.Query("to"))

	if from != "" {
		normalizedFrom, err := normalizeAuditFilterTimestamp(from)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid from timestamp; expected RFC3339"})
			return
		}
		from = normalizedFrom
	}
	if to != "" {
		normalizedTo, err := normalizeAuditFilterTimestamp(to)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid to timestamp; expected RFC3339"})
			return
		}
		to = normalizedTo
	}

	result, err := service.List(AuditListFilter{
		Page:       page,
		PageSize:   pageSize,
		TargetName: targetName,
		Action:     action,
		Status:     status,
		From:       from,
		To:         to,
	})
	if err != nil {
		message := "failed to load audit events"
		var listErr *AuditListError
		if errors.As(err, &listErr) {
			switch listErr.Stage {
			case "count":
				message = "failed to count audit events"
			case "parse":
				message = "failed to parse audit events"
			case "iterate":
				message = "failed to iterate audit events"
			}
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": message})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"items":     result.Items,
		"page":      result.Page,
		"page_size": result.PageSize,
		"total":     result.Total,
	})
}

func handleDashboardEventsWithBroker(c *gin.Context, broker *events.Broker) {
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unsupported"})
		return
	}
	if broker == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "streaming unavailable"})
		return
	}
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	dashboardEvents := broker.Subscribe()
	defer broker.Unsubscribe(dashboardEvents)

	fmt.Fprintf(c.Writer, "event: dashboard\n")
	fmt.Fprintf(c.Writer, "data: {\"reason\":\"connected\"}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(25 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case reason := <-dashboardEvents:
			fmt.Fprintf(c.Writer, "event: dashboard\n")
			fmt.Fprintf(c.Writer, "data: {\"reason\":%q}\n\n", reason)
			flusher.Flush()
		case <-heartbeat.C:
			fmt.Fprintf(c.Writer, ": keepalive\n\n")
			flusher.Flush()
		case <-c.Request.Context().Done():
			return
		}
	}
}

func handleObservabilitySummary(c *gin.Context) {
	handleObservabilitySummaryWithNow(c, func() time.Time { return time.Now().UTC() })
}

func handleObservabilitySummaryWithNow(c *gin.Context, now func() time.Time) {
	handleObservabilitySummaryWithService(c, defaultObservabilityService(), now)
}

func handleObservabilitySummaryWithService(c *gin.Context, service *ObservabilityService, now func() time.Time) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if service == nil {
		service = defaultObservabilityService()
	}
	window := c.Query("window")
	summary, err := service.BuildSummary(window, now())
	if err != nil {
		if errors.Is(err, errInvalidWindow) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window; allowed values: 24h, 7d, 30d"})
			return
		}
		log.Printf("handleObservabilitySummary: failed to build summary for window=%q: %v", window, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build observability summary"})
		return
	}
	c.JSON(http.StatusOK, summary)
}

func handleHealthTrendsWithService(c *gin.Context, service *ObservabilityService, now func() time.Time) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if service == nil {
		service = defaultObservabilityService()
	}
	trends, err := service.BuildHealthTrends(c.Query("window"), c.Query("server"), now())
	if err != nil {
		if errors.Is(err, errInvalidWindow) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window; allowed values: 7d, 30d"})
			return
		}
		log.Printf("handleHealthTrends: failed to build trends for window=%q server=%q: %v", c.Query("window"), c.Query("server"), err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build health trends"})
		return
	}
	c.JSON(http.StatusOK, trends)
}

func saveServerFacts(record serverFactsRecord) error {
	record.ServerName = strings.TrimSpace(record.ServerName)
	if record.ServerName == "" {
		return errors.New("server name is required")
	}
	if strings.TrimSpace(record.CollectedAt) == "" {
		record.CollectedAt = time.Now().UTC().Format(time.RFC3339)
	}
	if strings.TrimSpace(record.DiskStatus) == "" {
		record.DiskStatus = "unknown"
	}
	if strings.TrimSpace(record.AptStatus) == "" {
		record.AptStatus = "unknown"
	}
	if strings.TrimSpace(record.RawJSON) == "" {
		record.RawJSON = "{}"
	}
	return defaultServerFactsRepository().Save(record)
}

func loadServerFacts() (map[string]serverFactsRecord, error) {
	return defaultServerFactsRepository().LoadAll()
}

func renameServerFactsTx(tx *sql.Tx, oldName, newName string) error {
	return defaultServerFactsRepository().RenameServerTx(tx, oldName, newName)
}

func diskFreeKBFromOutput(output string) (int64, bool) {
	if freeKB, _, ok := diskFreeTotalKBFromOutput(output); ok {
		return freeKB, true
	}
	var minFree int64
	found := false
	for _, field := range strings.Fields(output) {
		value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil {
			continue
		}
		if !found || value < minFree {
			minFree = value
			found = true
		}
	}
	return minFree, found
}

func diskFreeTotalKBFromOutput(output string) (int64, int64, bool) {
	var minFree int64
	var totalForMin int64
	found := false
	for _, line := range strings.Split(output, "\n") {
		var values []int64
		for _, field := range strings.Fields(line) {
			value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err != nil || value < 0 {
				continue
			}
			values = append(values, value)
		}
		if len(values) < 2 {
			continue
		}
		totalKB := values[0]
		freeKB := values[1]
		if !found || freeKB < minFree {
			minFree = freeKB
			totalForMin = totalKB
			found = true
		}
	}
	return minFree, totalForMin, found
}

func healthStatusFromResult(result updatePrecheckResult) string {
	if result.Passed {
		return "ok"
	}
	return "critical"
}

func parseUptimeSeconds(output string) int64 {
	fields := strings.Fields(output)
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 || math.IsNaN(seconds) || math.IsInf(seconds, 0) {
		return 0
	}
	return int64(seconds)
}

func rebootResultRequiresRestart(result updatePrecheckResult) (bool, bool) {
	if strings.TrimSpace(result.Error) != "" {
		return false, false
	}
	if result.Passed {
		return false, true
	}
	text := strings.ToLower(result.Details + " " + result.Output)
	if rebootCheckErrorRe.MatchString(text) {
		return false, false
	}
	if rebootRequiredPhraseRe.MatchString(text) {
		return true, true
	}
	return false, true
}

func collectServerFactsWithConnection(server Server, client sshConnection, timeout time.Duration) serverFactsRecord {
	record := serverFactsRecord{
		ServerName:  server.Name,
		CollectedAt: time.Now().UTC().Format(time.RFC3339),
		DiskStatus:  "unknown",
		AptStatus:   "unknown",
		RawJSON:     "{}",
	}
	osOut, osErrOut, osErr := runSSHCommandWithTimeout(client, serverFactsOSCmd, nil, timeout)
	if osErr == nil {
		record.OSPrettyName = truncateString(osOut, 160)
	} else {
		record.OSPrettyName = "Unknown"
	}
	uptimeOut, _, uptimeErr := runSSHCommandWithTimeout(client, serverFactsUptimeCmd, nil, timeout)
	if uptimeErr == nil {
		record.UptimeSeconds = parseUptimeSeconds(uptimeOut)
	}
	diskOut, _, _ := runSSHCommandWithTimeout(client, precheckDiskSpaceCmd, nil, timeout)
	disk := checkDiskSpace(client)
	record.DiskStatus = healthStatusFromResult(disk)
	if diskFreeKB, diskTotalKB, ok := diskFreeTotalKBFromOutput(diskOut); ok {
		record.DiskFreeKB = diskFreeKB
		record.DiskTotalKB = diskTotalKB
	} else if diskFreeKB, ok := diskFreeKBFromOutput(diskOut); ok {
		record.DiskFreeKB = diskFreeKB
	}
	record.DiskDetails = disk.Details
	apt := checkAptHealth(client)
	record.AptStatus = healthStatusFromResult(apt)
	record.AptDetails = apt.Details
	reboot := checkRebootRequired(client)
	if required, known := rebootResultRequiresRestart(reboot); known {
		record.RebootRequired = &required
	}
	raw, err := json.Marshal(map[string]any{
		"os_stderr":     truncateString(osErrOut, 160),
		"os_error":      errorString(osErr),
		"uptime_error":  errorString(uptimeErr),
		"disk_result":   disk,
		"apt_result":    apt,
		"reboot_result": reboot,
	})
	if err != nil {
		log.Printf("collectServerFactsWithConnection: failed to marshal raw facts for %q: %v", server.Name, err)
		record.RawJSON = "{}"
	} else {
		record.RawJSON = string(raw)
	}
	return record
}

func errorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func refreshServerFactsWithUpdateDeps(server Server, deps UpdateServiceDeps) (serverFactsRecord, error) {
	deps = updateServiceDepsWithDefaults(deps)
	authMethods, err := deps.BuildAuthMethods(server)
	if err != nil {
		return serverFactsRecord{}, err
	}
	hostKeyCallback, err := deps.HostKeyCallback()
	if err != nil {
		return serverFactsRecord{}, err
	}
	config := &ssh.ClientConfig{
		User:            server.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         deps.SSHConnectTimeout,
	}
	conn, err := deps.DialSSH(server, config)
	if err != nil {
		return serverFactsRecord{}, err
	}
	defer conn.Close()
	record := deps.CollectServerFacts(server, conn, deps.LoadCommandTimeout())
	if err := deps.SaveServerFacts(record); err != nil {
		return serverFactsRecord{}, err
	}
	return record, nil
}

func handleServerFactsRefreshWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	name := strings.TrimSpace(c.Param("name"))
	state := deps.ServerState
	if state == nil {
		state = serverStateForContext(c)
	}
	if state == nil {
		state = globalServerState()
	}
	server, preRefreshStatus, err := state.BeginTransientAction(name, "facts_refresh")
	if errors.Is(err, sql.ErrNoRows) {
		audit(c, serverFactsRefreshAction, "server", name, "failure", "Server not found", nil)
		c.JSON(http.StatusNotFound, gin.H{"error": "Server not found"})
		return
	}
	if errors.Is(err, errActionInProgress) {
		_, status := state.ActionStatusInProgress(name)
		audit(c, serverFactsRefreshAction, "server", name, "failure", "Server action already in progress", map[string]any{"status": status})
		c.JSON(http.StatusConflict, gin.H{"error": "wait for the active server action to finish before refreshing host facts"})
		return
	}
	if err != nil {
		audit(c, serverFactsRefreshAction, "server", name, "failure", "Facts refresh unavailable", map[string]any{"error": err.Error()})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to start host facts refresh"})
		return
	}
	defer state.RestoreStatusSnapshot(name, preRefreshStatus)

	record, err := refreshServerFactsWithUpdateDeps(server, updateServiceEnsureDeps(deps.UpdateService))
	if err != nil {
		audit(c, serverFactsRefreshAction, "server", name, "failure", "Facts refresh failed", map[string]any{"error": err.Error()})
		c.JSON(http.StatusBadGateway, gin.H{"error": fmt.Sprintf("failed to refresh host facts: %v", err)})
		return
	}
	audit(c, serverFactsRefreshAction, "server", name, "success", "Host facts refreshed", map[string]any{
		"collected_at":    record.CollectedAt,
		"disk_status":     record.DiskStatus,
		"apt_status":      record.AptStatus,
		"reboot_required": record.RebootRequired,
		"uptime_seconds":  record.UptimeSeconds,
		"os_pretty_name":  record.OSPrettyName,
	})
	c.JSON(http.StatusOK, record)
}

func handleDashboardSummaryWithService(c *gin.Context, service *ObservabilityService, now func() time.Time) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	if service == nil {
		service = defaultObservabilityService()
	}
	summary, err := service.BuildDashboardSummary(c.Query("window"), now())
	if err != nil {
		if errors.Is(err, errInvalidWindow) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid window; allowed values: 24h, 7d, 30d"})
			return
		}
		log.Printf("handleDashboardSummary: %v", err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build dashboard summary"})
		return
	}
	c.JSON(http.StatusOK, summary)
}

func handleMetrics(c *gin.Context) {
	handleMetricsWithService(c, defaultObservabilityService())
}

func handleMetricsWithService(c *gin.Context, service *ObservabilityService) {
	if service == nil {
		service = defaultObservabilityService()
	}
	body, err := service.BuildMetrics(time.Now().UTC())
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to build metrics\n")
		return
	}
	c.Data(http.StatusOK, "text/plain; version=0.0.4", []byte(body))
}

func handleMetricsTokenStatusWithService(c *gin.Context, service *MetricsTokenService) {
	if service == nil {
		service = metricsTokenService
	}
	enabled := service.Status()
	if service == metricsTokenService {
		syncMetricsTokenGlobals(service)
	}
	c.JSON(http.StatusOK, gin.H{"enabled": enabled})
}

func handleMetricsTokenRotateWithService(c *gin.Context, service *MetricsTokenService) {
	if service == nil {
		service = metricsTokenService
	}
	token, err := service.Rotate()
	if err != nil {
		audit(c, "metrics.token.rotate", "metrics_token", "metrics", "failure", "Failed to rotate metrics API token", map[string]any{"error": err.Error()})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to rotate metrics token"})
		return
	}
	if service == metricsTokenService {
		syncMetricsTokenGlobals(service)
	}
	audit(c, "metrics.token.rotate", "metrics_token", "metrics", "success", "Metrics API token rotated", nil)
	c.JSON(http.StatusOK, gin.H{"enabled": true, "token": token})
}

func handleMetricsTokenClearWithService(c *gin.Context, service *MetricsTokenService) {
	if service == nil {
		service = metricsTokenService
	}
	if err := service.Clear(); err != nil {
		audit(c, "metrics.token.clear", "metrics_token", "metrics", "failure", "Failed to disable metrics API token", map[string]any{"error": err.Error()})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to disable metrics token"})
		return
	}
	if service == metricsTokenService {
		syncMetricsTokenGlobals(service)
	}
	audit(c, "metrics.token.clear", "metrics_token", "metrics", "success", "Metrics API token disabled", nil)
	c.JSON(http.StatusOK, gin.H{"enabled": false, "message": "Metrics token disabled"})
}

func actorFromContext(c *gin.Context) string {
	if c == nil {
		return "system"
	}
	if actor, ok := c.Get("actor"); ok {
		if s := strings.TrimSpace(fmt.Sprintf("%v", actor)); s != "" {
			return s
		}
	}
	return "unknown"
}

func clientIPFromContext(c *gin.Context) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.ClientIP())
}

func audit(c *gin.Context, action, targetType, targetName, status, message string, meta map[string]any) {
	if service := auditServiceForContext(c); service != nil {
		if err := service.Record(actorFromContext(c), clientIPFromContext(c), action, targetType, targetName, status, message, meta); err != nil {
			log.Printf("audit write failed: action=%s target=%s err=%v", action, targetName, err)
		}
		return
	}
	auditWithActor(actorFromContext(c), clientIPFromContext(c), action, targetType, targetName, status, message, meta)
}

func auditWithService(service *AuditService, c *gin.Context, action, targetType, targetName, status, message string, meta map[string]any) {
	if service == nil {
		audit(c, action, targetType, targetName, status, message, meta)
		return
	}
	if err := service.Record(actorFromContext(c), clientIPFromContext(c), action, targetType, targetName, status, message, meta); err != nil {
		log.Printf("audit write failed: action=%s target=%s err=%v", action, targetName, err)
	}
}

func startAuditPruner(ctx context.Context) {
	auditPruneTickerOnce.Do(func() {
		if err := pruneAuditEvents(auditRetentionDays); err != nil {
			log.Printf("audit prune failed: %v", err)
		}
		go func() {
			t := time.NewTicker(auditPruneInterval)
			for {
				select {
				case <-t.C:
					if err := pruneAuditEvents(auditRetentionDays); err != nil {
						log.Printf("audit prune failed: %v", err)
					}
				case <-ctx.Done():
					t.Stop()
					return
				}
			}
		}()
	})
}

func encryptSecret(secret string) (string, error) {
	return encryptSecretWithKey(secret, getEncryptionKey())
}

func encryptSecretWithKey(secret string, key []byte) (string, error) {
	if secret == "" {
		return "", nil
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	ciphertext := gcm.Seal(nil, nonce, []byte(secret), nil)
	return base64.StdEncoding.EncodeToString(nonce) + ":" + base64.StdEncoding.EncodeToString(ciphertext), nil
}

func decryptSecret(encoded string) (string, error) {
	return decryptSecretWithKey(encoded, getEncryptionKey())
}

func decryptSecretWithKey(encoded string, key []byte) (string, error) {
	if encoded == "" {
		return "", nil
	}
	parts := strings.SplitN(encoded, ":", 2)
	if len(parts) != 2 {
		return "", errors.New("invalid encrypted value format")
	}
	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", err
	}
	ciphertext, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func loadLegacyServersIntoService(service *ServerInventoryService, state *serverpkg.State) bool {
	if service == nil || state == nil {
		return false
	}
	paths := []string{}
	if dirExists("/data") {
		paths = append(paths, filepath.Join("/data", legacyServersFileName))
	}
	paths = append(paths, legacyServersFileName)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var legacy []Server
		if err := json.Unmarshal(data, &legacy); err != nil {
			log.Printf("Failed to parse legacy %s: %v", path, err)
			continue
		}
		if len(legacy) == 0 {
			continue
		}
		state.Lock()
		prevServers := state.CloneServers()
		state.SetServers(legacy)
		state.Unlock()
		if err := service.SaveWithTxHook(nil); err != nil {
			state.Lock()
			state.SetServers(prevServers)
			state.Unlock()
			log.Printf("Failed to import legacy servers from %s: %v", path, err)
			continue
		}
		log.Printf("Imported legacy servers from %s", path)
		return true
	}
	return false
}

func loadServers() {
	service := newServerInventoryService()
	service.Load()
}

type saveServersTxHook func(*sql.Tx) error

func saveServersWithTxHook(txHook saveServersTxHook) error {
	return newServerInventoryService().SaveWithTxHook(serverInventoryTxHook(txHook))
}

func saveServers() error {
	return saveServersWithTxHook(nil)
}

func cloneServers(src []Server) []Server {
	if src == nil {
		return nil
	}
	dst := make([]Server, len(src))
	for i, server := range src {
		server.Tags = append([]string(nil), server.Tags...)
		dst[i] = server
	}
	return dst
}

func cloneStatusMap(src map[string]*ServerStatus) map[string]*ServerStatus {
	dst := make(map[string]*ServerStatus, len(src))
	for name, status := range src {
		dst[name] = cloneServerStatus(status)
	}
	return dst
}

func clonePendingUpdates(src []PendingUpdate) []PendingUpdate {
	if src == nil {
		return nil
	}
	dst := make([]PendingUpdate, len(src))
	for i, update := range src {
		dst[i] = update
		dst[i].CVEs = append([]string(nil), update.CVEs...)
	}
	return dst
}

func stringPtr(s string) *string {
	return &s
}

func cloneServerStatus(status *ServerStatus) *ServerStatus {
	if status == nil {
		return nil
	}
	copyStatus := *status
	copyStatus.Upgradable = append([]string(nil), status.Upgradable...)
	copyStatus.PendingUpdates = clonePendingUpdates(status.PendingUpdates)
	copyStatus.UpgradePlan = serverpkg.CloneUpgradePlan(status.UpgradePlan)
	copyStatus.Tags = append([]string(nil), status.Tags...)
	return &copyStatus
}

func currentStatusSnapshot(name string) *ServerStatus {
	mu.Lock()
	defer mu.Unlock()
	return cloneServerStatus(statusMap[name])
}

func currentStatusLogs(name string) string {
	snapshot := currentStatusSnapshot(name)
	if snapshot == nil {
		return ""
	}
	return snapshot.Logs
}

func activeServerActionNames() []string {
	mu.Lock()
	defer mu.Unlock()

	names := make([]string, 0)
	for name, status := range statusMap {
		if status == nil || !statusInProgress(status.Status) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func activeServerActionNamesForContext(c *gin.Context) []string {
	if state := serverStateForContext(c); state != nil {
		return state.ActiveActionNames()
	}
	return activeServerActionNames()
}

func rejectGlobalKeyMutationIfServerActionsActive(c *gin.Context, action string) bool {
	activeNames := activeServerActionNamesForContext(c)
	if len(activeNames) == 0 {
		return false
	}
	audit(c, action, "global_key", "global", "failure", "Server action already in progress", map[string]any{
		"active_servers": activeNames,
	})
	c.JSON(http.StatusConflict, gin.H{
		"error":          "wait for active server actions to finish before changing the global SSH key",
		"active_servers": activeNames,
	})
	return true
}

func createServerActionJobWithStateAndManager(jm *JobManager, state *serverpkg.State, kind, serverName, actor, clientIP string, policy RetryPolicy) (JobRecord, error) {
	if jm == nil {
		return JobRecord{}, errors.New("job manager is not initialized")
	}
	var snapshot *ServerStatus
	if state != nil {
		snapshot = state.CurrentStatusSnapshot(serverName)
	} else {
		snapshot = currentStatusSnapshot(serverName)
	}
	initialLogs := ""
	if snapshot != nil {
		initialLogs = snapshot.Logs
	}
	return jm.CreateJob(JobCreateParams{
		Kind:            kind,
		ServerName:      serverName,
		Actor:           actor,
		ClientIP:        clientIP,
		Status:          jobStatusQueued,
		LogsText:        initialLogs,
		RetryPolicyJSON: marshalJobJSON(policy),
		MetaJSON:        "{}",
	})
}

func statusInProgress(status string) bool {
	return runtimepkg.StatusInProgress(status)
}

func findServerByNameLocked(name string) (Server, bool) {
	for _, s := range servers {
		if s.Name == name {
			return s, true
		}
	}
	return Server{}, false
}

func beginServerAction(name, newStatus string) (Server, error) {
	mu.Lock()
	defer mu.Unlock()
	status, exists := statusMap[name]
	if !exists || status == nil {
		return Server{}, sql.ErrNoRows
	}
	if statusInProgress(status.Status) {
		return Server{}, errActionInProgress
	}
	server, found := findServerByNameLocked(name)
	if !found {
		return Server{}, sql.ErrNoRows
	}
	status.Status = newStatus
	if strings.TrimSpace(status.Logs) == "" {
		status.Logs = "Starting Linux Updater..."
	}
	return server, nil
}

func approvePendingUpdate(name, scope string) (exists bool, approved bool) {
	return approvePendingUpdateWithOptions(name, scope, serverpkg.ApprovalOptions{})
}

func approvePendingUpdateWithOptions(name, scope string, opts serverpkg.ApprovalOptions) (exists bool, approved bool) {
	normalizedScope := normalizeApprovalScope(scope)
	mu.Lock()
	defer mu.Unlock()
	status, exists := statusMap[name]
	if !exists || status == nil {
		return exists, false
	}
	if status.Status != "pending_approval" {
		return exists, false
	}
	status.ApprovalScope = normalizedScope
	status.ApprovalConfirmRemovals = opts.ConfirmRemovals
	status.Status = "approved"
	return exists, true
}

func readUploadedKeyData(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, maxUploadedKeyBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > maxUploadedKeyBytes {
		return "", errUploadedKeyTooLarge
	}
	key := strings.TrimSpace(string(data))
	if key == "" {
		return "", errUploadedKeyEmpty
	}
	return key, nil
}

func readUploadedPrivateKey(file *multipart.FileHeader) (string, error) {
	if file.Size > maxUploadedKeyBytes {
		return "", errUploadedKeyTooLarge
	}
	src, err := file.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	return readUploadedKeyData(src)
}

func limitUploadedKeyRequest(c *gin.Context) {
	if c != nil && c.Request != nil && c.Writer != nil {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxUploadedKeyRequestBytes)
	}
}

func uploadedKeyFormErrorStatus(err error) int {
	var maxBytesErr *http.MaxBytesError
	if errors.As(err, &maxBytesErr) || strings.Contains(strings.ToLower(err.Error()), "request body too large") {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}

func stringsEqualConstantTime(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func init() {
	loadServers()
	for _, s := range servers {
		statusMap[s.Name] = &ServerStatus{
			Name:           s.Name,
			Host:           s.Host,
			Port:           normalizePort(s.Port),
			User:           s.User,
			Status:         "idle",
			Logs:           "",
			Upgradable:     []string{},
			PendingUpdates: []PendingUpdate{},
			HasPassword:    s.Pass != "",
			HasKey:         s.Key != "",
			Tags:           s.Tags,
		}
	}
}

func runUpdateWithActor(server Server, actor, clientIP string, policy RetryPolicy) {
	runUpdateJobWithActor(server, actor, clientIP, policy, "")
}

func runUpdateJobWithActor(server Server, actor, clientIP string, policy RetryPolicy, jobID string) {
	defaultUpdateService().RunUpdateJob(UpdateRunRequest{
		Server:   server,
		Actor:    actor,
		ClientIP: clientIP,
		Policy:   policy,
		JobID:    jobID,
	})
}

func runSudoersBootstrapWithActor(server Server, sudoPassword, actor, clientIP string, policy RetryPolicy) {
	runSudoersBootstrapJobWithActor(server, sudoPassword, actor, clientIP, policy, "")
}

func runSudoersBootstrapJobWithActor(server Server, sudoPassword, actor, clientIP string, policy RetryPolicy, jobID string) {
	defaultUpdateService().RunSudoersBootstrapJob(SudoersRunRequest{
		Server:       server,
		SudoPassword: sudoPassword,
		Actor:        actor,
		ClientIP:     clientIP,
		Policy:       policy,
		JobID:        jobID,
	})
}

func runSudoersDisableWithActor(server Server, sudoPassword, actor, clientIP string, policy RetryPolicy) {
	runSudoersDisableJobWithActor(server, sudoPassword, actor, clientIP, policy, "")
}

func runSudoersDisableJobWithActor(server Server, sudoPassword, actor, clientIP string, policy RetryPolicy, jobID string) {
	defaultUpdateService().RunSudoersDisableJob(SudoersRunRequest{
		Server:       server,
		SudoPassword: sudoPassword,
		Actor:        actor,
		ClientIP:     clientIP,
		Policy:       policy,
		JobID:        jobID,
	})
}

func runAutoremoveWithActor(server Server, actor, clientIP string, policy RetryPolicy) {
	runAutoremoveJobWithActor(server, actor, clientIP, policy, "")
}

func runAutoremoveJobWithActor(server Server, actor, clientIP string, policy RetryPolicy, jobID string) {
	defaultUpdateService().RunAutoremoveJob(AutoremoveRunRequest{
		Server:   server,
		Actor:    actor,
		ClientIP: clientIP,
		Policy:   policy,
		JobID:    jobID,
	})
}

func getUpgradable(client sshConnection, timeout time.Duration) ([]PendingUpdate, []string, UpgradePlan, error) {
	result, err := updatespkg.DiscoverPackageUpdates(client, timeout, runSSHCommandWithTimeout)
	if err != nil {
		return nil, nil, UpgradePlan{}, err
	}
	return result.PendingUpdates, result.Upgradable, result.UpgradePlan, nil
}

func parseUpgradableEntries(stdout string) ([]PendingUpdate, []string, error) {
	return updatespkg.ParseUpgradableEntries(stdout)
}

func sortPendingUpdates(updates []PendingUpdate) {
	updatespkg.SortPendingUpdates(updates)
}

func normalizeApprovalScope(scope string) string {
	return updatespkg.NormalizeApprovalScope(scope)
}

func buildSelectedUpgradeCmd(packages []string) string {
	return updatespkg.BuildSelectedUpgradeCmd(packages)
}

func preparePendingUpdatesForCVE(updates []PendingUpdate) []PendingUpdate {
	return updatespkg.PreparePendingUpdatesForCVE(updates)
}

func extractCVEsFromText(text string, max int) []string {
	return updatespkg.ExtractCVEsFromText(text, max)
}

func buildPackageCVEQueryCmd(pkg string) string {
	return updatespkg.BuildPackageCVEQueryCmd(pkg)
}

func queryPackageCVEs(client sshConnection, pkg string) ([]string, error) {
	stdout, _, err := runSSHCommandWithTimeout(client, buildPackageCVEQueryCmd(pkg), nil, cveLookupCommandTimeout)
	if err != nil {
		return nil, err
	}
	return extractCVEsFromText(stdout, cveLookupMaxPerPackage), nil
}

func startPendingUpdateCVEEnrichment(server Server, config *ssh.ClientConfig, updates []PendingUpdate, parentJobID, actor, clientIP string) {
	defaultUpdateService().StartPendingCVEEnrichment(server, config, updates, parentJobID, actor, clientIP)
}

func getGlobalKey() string {
	db := getDB()
	getCached := func() string {
		globalKeyMu.RLock()
		cached := globalKey
		globalKeyMu.RUnlock()
		return cached
	}
	for attempt := 1; attempt <= 3; attempt++ {
		var enc string
		err := db.QueryRow("SELECT value FROM settings WHERE key = ?", globalKeySetting).Scan(&enc)
		if err == sql.ErrNoRows {
			globalKeyMu.Lock()
			globalKey = ""
			globalKeyMu.Unlock()
			return ""
		}
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "database is locked") && attempt < 3 {
				time.Sleep(75 * time.Millisecond)
				continue
			}
			cached := getCached()
			log.Printf("Failed to load global SSH key: %v", err)
			if strings.TrimSpace(cached) != "" {
				log.Printf("Using cached global SSH key due to read failure")
			}
			return cached
		}
		// Do not hold runtimeStateMu while decrypting; decrypt may initialize
		// encryption key and require runtimeStateMu write access.
		key, decErr := decryptSecret(enc)
		if decErr != nil {
			cached := getCached()
			log.Printf("Failed to decrypt global SSH key: %v", decErr)
			if strings.TrimSpace(cached) != "" {
				log.Printf("Using cached global SSH key due to decrypt failure")
			}
			return cached
		}
		globalKeyMu.Lock()
		globalKey = key
		globalKeyMu.Unlock()
		return key
	}
	return ""
}

func setGlobalKey(key string) error {
	enc, err := encryptSecret(key)
	if err != nil {
		return err
	}
	db := getDB()
	_, err = db.Exec(
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		globalKeySetting, enc,
	)
	if err != nil {
		return err
	}
	runtimeStateMu.Lock()
	defer runtimeStateMu.Unlock()
	globalKeyMu.Lock()
	defer globalKeyMu.Unlock()
	globalKey = key
	return nil
}

func securityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-Frame-Options", "DENY")
		c.Header("Content-Security-Policy", defaultContentSecurityPolicy)
		if c.Request != nil && c.Request.TLS != nil {
			c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		} else {
			forwardedProto := strings.TrimSpace(c.GetHeader("X-Forwarded-Proto"))
			if forwardedProto != "" {
				if idx := strings.Index(forwardedProto, ","); idx >= 0 {
					forwardedProto = strings.TrimSpace(forwardedProto[:idx])
				}
				if strings.EqualFold(forwardedProto, "https") {
					c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
				}
			}
		}
		c.Next()
	}
}

func trustedProxiesFromEnv() []string {
	return appshell.ParseTrustedProxies(os.Getenv(trustedProxiesEnv))
}

func setupRouter() (*gin.Engine, error) {
	return setupRouterWithDeps(NewDefaultAppDeps())
}

func setupRouterWithDeps(deps AppDeps) (*gin.Engine, error) {
	deps = deps.withDefaults()
	if deps.ServerInventoryService != nil {
		deps.ServerInventoryService.Load()
		initializeServerStateStatuses(deps.ServerState)
	}
	return appshell.NewRouter(appshell.RouterConfig{
		TrustedProxies:        deps.TrustedProxies,
		GlobalMiddleware:      []gin.HandlerFunc{securityHeadersMiddleware(), backupRestoreBarrierMiddleware(deps.BackupBarrier)},
		InitializeMaintenance: deps.InitializeMaintenanceState,
		InitializeJobs:        deps.initializeJobManager,
		InitializeSessions:    deps.initializeSessionManager,
		TemplatesGlob:         "templates/*",
		StaticPath:            "/static",
		StaticRoot:            "./static",
		RegisterRoutes: func(r *gin.Engine) error {
			return registerRoutes(r, deps)
		},
	})
}

func registerRoutes(r *gin.Engine, deps AppDeps) error {
	deps = deps.withDefaults()
	r.Use(authRuntimeMiddleware(deps))
	registerPublicRoutes(r, deps)
	r.Use(authGateMiddleware())
	r.Use(sameOriginWriteMiddleware())
	registerProtectedPageRoutes(r)
	registerProtectedAuthAndSettingsRoutes(r, deps)
	registerPolicyAuditObservabilityRoutes(r, deps)
	registerServerAndActionRoutes(r, deps)
	return nil
}

func registerPublicRoutes(r *gin.Engine, deps AppDeps) {
	r.GET("/setup", handleSetupPage)
	r.GET("/login", handleLoginPage)
	r.POST("/api/auth/setup", sameOriginWriteMiddleware(), handleAuthSetup)
	r.POST("/api/auth/login", sameOriginWriteMiddleware(), handleAuthLogin)
	r.GET("/api/auth/status", handleAuthStatus)
	r.GET("/api/maintenance", handleMaintenanceStatus)
	r.GET("/metrics", metricsBearerMiddlewareWithServiceAndLimiter(deps.MetricsTokenService, deps.MetricsRateLimiter), func(c *gin.Context) {
		handleMetricsWithService(c, deps.ObservabilityService)
	})

}

func registerProtectedPageRoutes(r *gin.Engine) {
	r.GET("/", func(c *gin.Context) {
		setNoStoreHeaders(c)
		c.HTML(http.StatusOK, "index.html", nil)
	})

	r.GET("/manage", func(c *gin.Context) {
		setNoStoreHeaders(c)
		c.HTML(http.StatusOK, "manage.html", nil)
	})

	r.GET("/observability", func(c *gin.Context) {
		setNoStoreHeaders(c)
		c.HTML(http.StatusOK, "observability.html", nil)
	})

	r.GET("/admin", func(c *gin.Context) {
		setNoStoreHeaders(c)
		c.HTML(http.StatusOK, "admin.html", nil)
	})

}

func registerProtectedAuthAndSettingsRoutes(r *gin.Engine, deps AppDeps) {
	deps = deps.withDefaults()
	r.POST("/api/auth/logout", handleAuthLogout)
	r.GET("/api/auth/sessions", handleAuthSessionsStatus)
	r.PUT("/api/auth/password", handleAuthPasswordChange)
	r.DELETE("/api/auth/sessions", handleAuthSessionsClear)
	r.GET("/api/metrics/token", func(c *gin.Context) {
		handleMetricsTokenStatusWithService(c, deps.MetricsTokenService)
	})
	r.POST("/api/metrics/token", func(c *gin.Context) {
		handleMetricsTokenRotateWithService(c, deps.MetricsTokenService)
	})
	r.DELETE("/api/metrics/token", func(c *gin.Context) {
		handleMetricsTokenClearWithService(c, deps.MetricsTokenService)
	})
	r.GET("/api/backup/status", func(c *gin.Context) {
		handleBackupStatusWithService(c, deps.BackupService)
	})
	r.GET("/api/dashboard/events", func(c *gin.Context) {
		handleDashboardEventsWithBroker(c, deps.DashboardEventBroker)
	})
	r.GET("/api/app-settings/timezone", handleAppTimezoneStatus)
	r.PUT("/api/app-settings/timezone", handleAppTimezoneUpdate)
	r.GET("/api/notifications/settings", func(c *gin.Context) {
		handleNotificationSettingsStatus(c, deps.NotificationService)
	})
	r.PUT("/api/notifications/settings", func(c *gin.Context) {
		handleNotificationSettingsUpdate(c, deps.NotificationService)
	})
	r.POST("/api/notifications/test", func(c *gin.Context) {
		handleNotificationTest(c, deps.NotificationService)
	})
	r.POST("/api/backup/export", func(c *gin.Context) {
		handleBackupExportWithDeps(c, deps)
	})
	r.POST("/api/backup/restore", func(c *gin.Context) {
		handleBackupRestoreWithDeps(c, deps)
	})
	r.POST("/api/backup/verify", func(c *gin.Context) {
		handleBackupVerifyWithDeps(c, deps)
	})
}

func registerPolicyAuditObservabilityRoutes(r *gin.Engine, deps AppDeps) {
	deps = deps.withDefaults()
	r.GET("/api/update-policies", func(c *gin.Context) {
		handleUpdatePoliciesListWithDeps(c, deps)
	})
	r.POST("/api/update-policies", func(c *gin.Context) {
		handleUpdatePolicyCreateWithDeps(c, deps)
	})
	r.POST("/api/update-policies/preview", func(c *gin.Context) {
		handleUpdatePolicyPreviewWithDeps(c, deps)
	})
	r.GET("/api/update-policies/runs", func(c *gin.Context) {
		handleUpdatePolicyRunsWithDeps(c, deps)
	})
	r.GET("/api/update-policies/calendar", func(c *gin.Context) {
		handleUpdatePolicyCalendarWithDeps(c, deps)
	})
	r.GET("/api/update-policies/settings", func(c *gin.Context) {
		handleUpdatePolicySettingsStatusWithDeps(c, deps)
	})
	r.PUT("/api/update-policies/settings", func(c *gin.Context) {
		handleUpdatePolicySettingsUpdateWithDeps(c, deps)
	})
	r.GET("/api/update-policies/:id/overrides", func(c *gin.Context) {
		handleUpdatePolicyOverridesWithDeps(c, deps)
	})
	r.PUT("/api/update-policies/:id/overrides/:server", func(c *gin.Context) {
		handleUpdatePolicyOverrideUpsertWithDeps(c, deps)
	})
	r.PUT("/api/update-policies/:id", func(c *gin.Context) {
		handleUpdatePolicyUpdateWithDeps(c, deps)
	})
	r.DELETE("/api/update-policies/:id", func(c *gin.Context) {
		handleUpdatePolicyDeleteWithDeps(c, deps)
	})

	r.GET("/api/audit-events", func(c *gin.Context) {
		handleAuditEventsWithService(c, deps.AuditService)
	})
	r.GET("/api/reports/audit/:id", func(c *gin.Context) {
		handleAuditReportWithService(c, deps.AuditService)
	})
	r.GET("/api/reports/jobs/:id", func(c *gin.Context) {
		handleJobReportWithDeps(c, deps)
	})
	r.GET("/api/jobs/:id", func(c *gin.Context) {
		handleJobDetailWithDeps(c, deps)
	})
	r.GET("/api/observability/summary", func(c *gin.Context) {
		handleObservabilitySummaryWithService(c, deps.ObservabilityService, deps.Now)
	})
	r.GET("/api/observability/health-trends", func(c *gin.Context) {
		handleHealthTrendsWithService(c, deps.ObservabilityService, deps.Now)
	})
	r.GET("/api/dashboard/summary", func(c *gin.Context) {
		handleDashboardSummaryWithService(c, deps.ObservabilityService, deps.Now)
	})
	r.POST("/api/audit-events/prune", func(c *gin.Context) {
		if err := deps.AuditService.Prune(auditRetentionDays); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to prune audit events"})
			return
		}
		auditWithService(deps.AuditService, c, "audit.prune", "system", "audit_events", "success", fmt.Sprintf("Pruned entries older than %d days", auditRetentionDays), map[string]any{"retention_days": auditRetentionDays})
		c.JSON(http.StatusOK, gin.H{"message": "Audit events pruned"})
	})
}

func writeServerInventoryCommandResult(c *gin.Context, result serverpkg.CommandResult, successStatus int, successBody any) {
	if result.Audit.Action != "" {
		audit(c, result.Audit.Action, result.Audit.TargetType, result.Audit.TargetName, result.Audit.Status, result.Audit.Message, result.Audit.Meta)
	}
	if result.Succeeded() {
		if successBody == nil {
			successBody = gin.H{"message": result.Message}
		}
		c.JSON(successStatus, successBody)
		return
	}
	c.JSON(serverInventoryCommandHTTPStatus(result), gin.H{"error": result.Error})
}

func serverInventoryCommandHTTPStatus(result serverpkg.CommandResult) int {
	switch result.Outcome {
	case serverpkg.CommandOutcomeInvalid:
		return http.StatusBadRequest
	case serverpkg.CommandOutcomeConflict:
		return http.StatusConflict
	case serverpkg.CommandOutcomeNotFound:
		return http.StatusNotFound
	case serverpkg.CommandOutcomeRemoteError:
		return http.StatusBadGateway
	default:
		return http.StatusInternalServerError
	}
}

func serverInventoryHostKeyScanBody(result serverpkg.CommandResult) gin.H {
	if result.HostKeyScan == nil {
		return gin.H{}
	}
	scan := result.HostKeyScan
	return gin.H{
		"host":               scan.Host,
		"port":               scan.Port,
		"algorithm":          scan.Algorithm,
		"fingerprint_sha256": scan.FingerprintSHA256,
		"known_hosts_line":   scan.KnownHostsLine,
		"already_trusted":    scan.AlreadyTrusted,
	}
}

func serverInventoryHostKeyTrustBody(result serverpkg.CommandResult) gin.H {
	if result.HostKeyTrust == nil {
		return gin.H{}
	}
	trust := result.HostKeyTrust
	return gin.H{
		"message":            trust.Message,
		"host":               trust.Host,
		"port":               trust.Port,
		"fingerprint_sha256": trust.FingerprintSHA256,
		"known_hosts_line":   trust.KnownHostsLine,
		"already_trusted":    trust.AlreadyTrusted,
	}
}

func serverInventoryHostKeyClearBody(result serverpkg.CommandResult) gin.H {
	if result.HostKeyClear == nil {
		return gin.H{}
	}
	clear := result.HostKeyClear
	return gin.H{
		"message":         clear.Message,
		"host":            clear.Host,
		"port":            clear.Port,
		"removed_entries": clear.RemovedEntries,
	}
}

func registerServerAndActionRoutes(r *gin.Engine, deps AppDeps) {
	deps = deps.withDefaults()
	inventoryService := deps.ServerInventoryService
	inventoryCommands := serverpkg.NewCommandService(inventoryService)
	serverState := func() *serverpkg.State {
		return deps.ServerState
	}

	r.GET("/api/servers", func(c *gin.Context) {
		serverState()
		c.JSON(http.StatusOK, inventoryService.ListStatuses())
	})

	r.POST("/api/servers", func(c *gin.Context) {
		serverState()
		var newServer Server
		if err := c.ShouldBindJSON(&newServer); err != nil {
			audit(c, "server.create", "server", "-", "failure", "Invalid request payload", nil)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := inventoryCommands.CreateServer(newServer)
		writeServerInventoryCommandResult(c, result, http.StatusCreated, result.Server)
	})

	r.PUT("/api/servers/:name", func(c *gin.Context) {
		serverState()
		name := c.Param("name")
		var updatedServer Server
		if err := c.ShouldBindJSON(&updatedServer); err != nil {
			audit(c, "server.update", "server", name, "failure", "Invalid request payload", nil)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := inventoryCommands.UpdateServer(name, updatedServer)
		writeServerInventoryCommandResult(c, result, http.StatusOK, result.Server)
	})

	r.DELETE("/api/servers/:name", func(c *gin.Context) {
		serverState()
		name := c.Param("name")
		result := inventoryCommands.DeleteServer(name)
		writeServerInventoryCommandResult(c, result, http.StatusOK, nil)
	})

	r.DELETE("/api/servers/:name/password", func(c *gin.Context) {
		serverState()
		name := c.Param("name")
		result := inventoryCommands.ClearPassword(name)
		writeServerInventoryCommandResult(c, result, http.StatusOK, nil)
	})

	r.POST("/api/servers/:name/key", func(c *gin.Context) {
		serverState()
		name := c.Param("name")
		limitUploadedKeyRequest(c)
		preflight := inventoryCommands.CheckServerKeyUpload(name)
		if !preflight.Succeeded() {
			writeServerInventoryCommandResult(c, preflight, http.StatusOK, nil)
			return
		}
		file, err := c.FormFile("key")
		if err != nil {
			audit(c, "server.key.upload", "server", name, "failure", "Missing key file", nil)
			if uploadedKeyFormErrorStatus(err) == http.StatusRequestEntityTooLarge {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": errUploadedKeyTooLarge.Error()})
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "missing key file"})
			}
			return
		}
		key, err := readUploadedPrivateKey(file)
		if err != nil {
			if errors.Is(err, errUploadedKeyTooLarge) {
				audit(c, "server.key.upload", "server", name, "failure", "Uploaded key too large", nil)
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
				return
			}
			if errors.Is(err, errUploadedKeyEmpty) {
				audit(c, "server.key.upload", "server", name, "failure", "Uploaded key empty", nil)
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			audit(c, "server.key.upload", "server", name, "failure", "Failed to read key", nil)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read key"})
			return
		}
		result := inventoryCommands.SetServerKey(name, key)
		writeServerInventoryCommandResult(c, result, http.StatusOK, nil)
	})

	r.DELETE("/api/servers/:name/key", func(c *gin.Context) {
		serverState()
		name := c.Param("name")
		result := inventoryCommands.ClearServerKey(name)
		writeServerInventoryCommandResult(c, result, http.StatusOK, nil)
	})

	r.POST("/api/keys/global", func(c *gin.Context) {
		if rejectGlobalKeyMutationIfServerActionsActive(c, "global_key.upload") {
			return
		}
		limitUploadedKeyRequest(c)
		file, err := c.FormFile("key")
		if err != nil {
			audit(c, "global_key.upload", "global_key", "global", "failure", "Missing key file", nil)
			if uploadedKeyFormErrorStatus(err) == http.StatusRequestEntityTooLarge {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": errUploadedKeyTooLarge.Error()})
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "missing key file"})
			}
			return
		}
		key, err := readUploadedPrivateKey(file)
		if err != nil {
			if errors.Is(err, errUploadedKeyTooLarge) {
				audit(c, "global_key.upload", "global_key", "global", "failure", "Uploaded key too large", nil)
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": err.Error()})
				return
			}
			if errors.Is(err, errUploadedKeyEmpty) {
				audit(c, "global_key.upload", "global_key", "global", "failure", "Uploaded key empty", nil)
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			audit(c, "global_key.upload", "global_key", "global", "failure", "Failed to read key", nil)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to read key"})
			return
		}
		if err := deps.SetGlobalKey(key); err != nil {
			audit(c, "global_key.upload", "global_key", "global", "failure", "Failed to save global key", map[string]any{"error": err.Error()})
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		audit(c, "global_key.upload", "global_key", "global", "success", "Global key saved", nil)
		c.JSON(http.StatusOK, gin.H{"message": "Global key saved"})
	})

	r.DELETE("/api/keys/global", func(c *gin.Context) {
		if rejectGlobalKeyMutationIfServerActionsActive(c, "global_key.clear") {
			return
		}
		if err := deps.ClearGlobalKey(); err != nil {
			audit(c, "global_key.clear", "global_key", "global", "failure", "Failed to clear global key", map[string]any{"error": err.Error()})
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		audit(c, "global_key.clear", "global_key", "global", "success", "Global key cleared", nil)
		c.JSON(http.StatusOK, gin.H{"message": "Global key cleared"})
	})

	r.GET("/api/keys/global", func(c *gin.Context) {
		hasKey, err := deps.HasGlobalKey()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read global key"})
			return
		}
		if !hasKey {
			c.JSON(http.StatusOK, gin.H{"has_key": false})
			return
		}
		c.JSON(http.StatusOK, gin.H{"has_key": true})
	})

	r.POST("/api/servers/:name/facts/refresh", func(c *gin.Context) {
		handleServerFactsRefreshWithDeps(c, deps)
	})

	r.POST("/api/hostkeys/scan", func(c *gin.Context) {
		var req struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "hostkey.scan", "hostkey", "-", "failure", "Invalid request payload", nil)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := inventoryCommands.ScanHostKey(req.Host, req.Port)
		writeServerInventoryCommandResult(c, result, http.StatusOK, serverInventoryHostKeyScanBody(result))
	})

	r.POST("/api/hostkeys/trust", func(c *gin.Context) {
		var req struct {
			Host              string `json:"host"`
			Port              int    `json:"port"`
			FingerprintSHA256 string `json:"fingerprint_sha256"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "hostkey.trust", "hostkey", "-", "failure", "Invalid request payload", nil)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := inventoryCommands.TrustHostKey(req.Host, req.Port, req.FingerprintSHA256)
		writeServerInventoryCommandResult(c, result, http.StatusOK, serverInventoryHostKeyTrustBody(result))
	})

	r.POST("/api/hostkeys/clear", func(c *gin.Context) {
		var req struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "hostkey.clear", "hostkey", "-", "failure", "Invalid request payload", nil)
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		result := inventoryCommands.ClearKnownHost(req.Host, req.Port)
		writeServerInventoryCommandResult(c, result, http.StatusOK, serverInventoryHostKeyClearBody(result))
	})

	r.POST("/api/update/:name", func(c *gin.Context) {
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.StartUpdate(c.Param("name"), actorFromContext(c), clientIPFromContext(c)))
	})

	r.POST("/api/autoremove/:name", func(c *gin.Context) {
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.StartAutoremove(c.Param("name"), actorFromContext(c), clientIPFromContext(c)))
	})

	r.POST("/api/sudoers/:name", func(c *gin.Context) {
		name := c.Param("name")
		var req struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "sudoers.enable.start", "server", name, "failure", "Invalid request payload", retryPolicyMeta(loadRetryPolicyFromEnv()))
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.StartSudoersEnable(name, actorFromContext(c), clientIPFromContext(c), req.Password))
	})

	r.POST("/api/sudoers/disable/:name", func(c *gin.Context) {
		name := c.Param("name")
		var req struct {
			Password string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			audit(c, "sudoers.disable.start", "server", name, "failure", "Invalid request payload", retryPolicyMeta(loadRetryPolicyFromEnv()))
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.StartSudoersDisable(name, actorFromContext(c), clientIPFromContext(c), req.Password))
	})

	r.POST("/api/approve/:name", func(c *gin.Context) {
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.ApproveAll(c.Param("name")))
	})

	r.POST("/api/approve-security/:name", func(c *gin.Context) {
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.ApproveSecurity(c.Param("name")))
	})

	r.POST("/api/approve-security-kept-back/:name", func(c *gin.Context) {
		name := c.Param("name")
		var req struct {
			ConfirmRemovals bool `json:"confirm_removals"`
		}
		if c.Request.Body != nil {
			if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
				return
			}
		}
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.ApproveKeptBackSecurity(name, req.ConfirmRemovals))
	})

	r.POST("/api/approve-full/:name", func(c *gin.Context) {
		name := c.Param("name")
		var req struct {
			ConfirmRemovals bool `json:"confirm_removals"`
		}
		if c.Request.Body != nil {
			if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
				c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request payload"})
				return
			}
		}
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.ApproveFullUpgrade(name, req.ConfirmRemovals))
	})

	r.POST("/api/cancel/:name", func(c *gin.Context) {
		lifecycle := newServerActionLifecycle(deps, func(action, targetType, targetName, status, message string, meta map[string]any) {
			audit(c, action, targetType, targetName, status, message, meta)
		})
		writeServerActionLifecycleResult(c, lifecycle.Cancel(c.Param("name")))
	})

}

func writeServerActionLifecycleResult(c *gin.Context, result serverActionLifecycleResult) {
	if result.maintenanceBlocked {
		writeMaintenanceBlockedResponse(c)
		return
	}
	c.JSON(result.statusCode, gin.H(result.body))
}

func main() {
	deps := NewDefaultAppDeps()
	r, err := setupRouterWithDeps(deps)
	if err != nil {
		log.Fatalf("Failed to setup router: %v", err)
	}
	seedVariantCDemoIfRequested(deps)
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	startAuditPruner(shutdownCtx)
	startPolicyScheduler(deps.PolicyService, shutdownCtx, PolicySchedulerOptions{})
	defer StopAuthRateLimiters()
	server := &http.Server{
		Addr:         ":8080",
		Handler:      sessionHandler(r),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	go func() {
		<-shutdownCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Failed to shutdown web server cleanly: %v", err)
		}
	}()
	log.Println("Starting web server on :8080")
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Failed to run web server: %v", err)
	}
}
