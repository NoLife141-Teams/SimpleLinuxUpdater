package main

import (
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	updatespkg "debian-updater/internal/updates"
)

type delayedNewSessionConn struct {
	delay time.Duration
}

func (c *delayedNewSessionConn) NewSession() (sshSessionRunner, error) {
	time.Sleep(c.delay)
	return &noopSession{}, nil
}

func (c *delayedNewSessionConn) Close() error { return nil }

type noopSession struct{}

func (s *noopSession) SetStdin(io.Reader)  {}
func (s *noopSession) SetStdout(io.Writer) {}
func (s *noopSession) SetStderr(io.Writer) {}
func (s *noopSession) Run(string) error    { return nil }
func (s *noopSession) Close() error        { return nil }

type progressingSSHConnection struct {
	interval time.Duration
	writes   int
}

type progressingSSHSession struct {
	conn   *progressingSSHConnection
	stdout io.Writer
}

func (s *progressingSSHSession) SetStdin(io.Reader)    {}
func (s *progressingSSHSession) SetStdout(w io.Writer) { s.stdout = w }
func (s *progressingSSHSession) SetStderr(io.Writer)   {}

func (s *progressingSSHSession) Run(command string) error {
	if strings.Contains(command, "/usr/bin/fuser") {
		return errors.New("no process uses the apt locks")
	}
	for i := 0; i < s.conn.writes; i++ {
		time.Sleep(s.conn.interval)
		_, _ = io.WriteString(s.stdout, "progress\n")
	}
	return nil
}

func (s *progressingSSHSession) Close() error { return nil }

func (c *progressingSSHConnection) NewSession() (sshSessionRunner, error) {
	return &progressingSSHSession{conn: c}, nil
}

func (c *progressingSSHConnection) Close() error { return nil }

type aptLockAwareTestConnection struct {
	mu              sync.Mutex
	lockActive      bool
	extendedAllowed bool
	lockProbeCount  int
	connectionClose bool
	commandDelay    time.Duration
	commandOutputAt time.Duration
	lockProbeDelay  time.Duration
}

type aptLockAwareTestSession struct {
	conn   *aptLockAwareTestConnection
	stdout io.Writer
}

func (s *aptLockAwareTestSession) SetStdin(io.Reader)    {}
func (s *aptLockAwareTestSession) SetStdout(w io.Writer) { s.stdout = w }
func (s *aptLockAwareTestSession) SetStderr(io.Writer)   {}

func (s *aptLockAwareTestSession) Run(command string) error {
	if strings.Contains(command, "/usr/bin/fuser") {
		s.conn.mu.Lock()
		s.conn.lockProbeCount++
		lockActive := s.conn.lockActive
		extendedAllowed := s.conn.extendedAllowed
		lockProbeDelay := s.conn.lockProbeDelay
		s.conn.mu.Unlock()
		if lockProbeDelay > 0 {
			time.Sleep(lockProbeDelay)
		}
		if strings.Contains(command, "/var/lib/apt/lists/lock") && !extendedAllowed {
			return errors.New("sudo: a password is required")
		}
		if !lockActive {
			return errors.New("no process uses the apt locks")
		}
		if s.stdout != nil {
			_, _ = io.WriteString(s.stdout, "4242\n")
		}
		return nil
	}
	delay := s.conn.commandDelay
	if delay <= 0 {
		delay = 95 * time.Millisecond
	}
	if outputAt := s.conn.commandOutputAt; outputAt > 0 {
		time.Sleep(outputAt)
		_, _ = io.WriteString(s.stdout, "progress\n")
		delay -= outputAt
	}
	if delay <= 0 {
		return nil
	}
	time.Sleep(delay)
	return nil
}

func (s *aptLockAwareTestSession) Close() error { return nil }

func (c *aptLockAwareTestConnection) NewSession() (sshSessionRunner, error) {
	return &aptLockAwareTestSession{conn: c}, nil
}

func (c *aptLockAwareTestConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connectionClose = true
	return nil
}

func TestLoadRetryPolicyFromEnvDefaults(t *testing.T) {
	t.Setenv(retryMaxAttemptsEnv, "")
	t.Setenv(retryBaseDelayMSEnv, "")
	t.Setenv(retryMaxDelayMSEnv, "")
	t.Setenv(retryJitterPctEnv, "")

	p := loadRetryPolicyFromEnv()
	if p.MaxAttempts != 3 {
		t.Fatalf("MaxAttempts = %d, want 3", p.MaxAttempts)
	}
	if p.BaseDelay != time.Second {
		t.Fatalf("BaseDelay = %v, want 1s", p.BaseDelay)
	}
	if p.MaxDelay != 8*time.Second {
		t.Fatalf("MaxDelay = %v, want 8s", p.MaxDelay)
	}
	if p.JitterPct != 20 {
		t.Fatalf("JitterPct = %d, want 20", p.JitterPct)
	}
}

func TestRunSSHCommandWithTimeoutTimesOutBlockedSessionOpen(t *testing.T) {
	conn := &delayedNewSessionConn{delay: 2 * time.Second}
	start := time.Now()
	_, _, err := runSSHCommandWithTimeout(conn, "true", updatespkg.HostCommandEffectReadOnly, nil, 200*time.Millisecond)
	if err == nil {
		t.Fatalf("runSSHCommandWithTimeout() error = nil, want timeout error")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want timeout message", err)
	}
	if !updatespkg.IsRetryableError(err) {
		t.Fatalf("timeout error should be retryable, got: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 1500*time.Millisecond {
		t.Fatalf("runSSHCommandWithTimeout() took too long: %v", elapsed)
	}
}

func TestRunSSHCommandWithTimeoutExtendsDeadlineWhileOutputIsActive(t *testing.T) {
	conn := &progressingSSHConnection{
		interval: 10 * time.Millisecond,
		writes:   8,
	}

	stdout, _, err := runSSHCommandWithTimeout(conn, aptUpgradeCmd, updatespkg.HostCommandEffectPackageStateMutation, nil, 25*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want regular output to extend the idle deadline", err)
	}
	if got := strings.Count(stdout, "progress\n"); got != conn.writes {
		t.Fatalf("progress lines = %d, want %d", got, conn.writes)
	}
}

func TestRunSSHCommandWithTimeoutKeepsHardDeadlineForReadOnlyEffectRegardlessOfSyntax(t *testing.T) {
	conn := &progressingSSHConnection{
		interval: 10 * time.Millisecond,
		writes:   8,
	}

	_, _, err := runSSHCommandWithTimeout(conn, "apt-get -y upgrade", updatespkg.HostCommandEffectReadOnly, nil, 25*time.Millisecond)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want read-only effect to retain its hard deadline", err)
	}
}

func TestRunSSHCommandWithTimeoutKeepsWaitingWhileAptLockIsActive(t *testing.T) {
	conn := &aptLockAwareTestConnection{lockActive: true, extendedAllowed: true}

	_, _, err := runSSHCommandWithTimeout(conn, "simplelinuxupdater-apt apply transaction-42", updatespkg.HostCommandEffectPackageStateMutation, nil, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want active apt lock to extend the wait", err)
	}

	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	connectionClosed := conn.connectionClose
	conn.mu.Unlock()
	if lockProbeCount < 2 {
		t.Fatalf("apt lock probes = %d, want at least 2", lockProbeCount)
	}
	if connectionClosed {
		t.Fatal("SSH connection closed while apt lock was active")
	}
}

func TestRunSSHCommandWithTimeoutFallsBackToLegacyAptLockProbe(t *testing.T) {
	conn := &aptLockAwareTestConnection{lockActive: true}

	_, _, err := runSSHCommandWithTimeout(conn, aptUpgradeCmd, updatespkg.HostCommandEffectPackageStateMutation, nil, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want legacy sudoers probe to extend the wait", err)
	}

	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	conn.mu.Unlock()
	if lockProbeCount < 2 {
		t.Fatalf("apt lock probes = %d, want extended probe plus legacy fallback", lockProbeCount)
	}
}

func TestRunSSHCommandWithTimeoutStillTimesOutAptWithoutActiveLock(t *testing.T) {
	conn := &aptLockAwareTestConnection{extendedAllowed: true}

	_, stderr, err := runSSHCommandWithTimeout(conn, "simplelinuxupdater-apt apply transaction-42", updatespkg.HostCommandEffectPackageStateMutation, nil, 30*time.Millisecond)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "timed out") {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want timeout without an active apt lock", err)
	}
	if !strings.Contains(err.Error(), "automatic replay disabled") || !strings.Contains(err.Error(), "total (checkpoint window 30ms)") {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want unknown outcome with cumulative timeout", err)
	}
	if !strings.Contains(stderr, "APT liveness check stopped waiting") {
		t.Fatalf("runSSHCommandWithTimeout() stderr = %q, want observable liveness decision", stderr)
	}
	if updatespkg.IsRetryableError(err) {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want ambiguous mutating APT timeout to stop replay", err)
	}

	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	conn.mu.Unlock()
	if lockProbeCount != 1 {
		t.Fatalf("apt lock probes = %d, want 1", lockProbeCount)
	}
}

func TestRunSSHCommandWithTimeoutReturnsSuccessWhenAptCompletesDuringLockProbe(t *testing.T) {
	conn := &aptLockAwareTestConnection{
		extendedAllowed: true,
		commandDelay:    15 * time.Millisecond,
		lockProbeDelay:  30 * time.Millisecond,
	}

	_, _, err := runSSHCommandWithTimeout(conn, aptUpgradeCmd, updatespkg.HostCommandEffectPackageStateMutation, nil, 10*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want completion during lock probe to succeed", err)
	}
}

func TestRunSSHCommandWithTimeoutHonorsActivityDuringLockProbe(t *testing.T) {
	conn := &aptLockAwareTestConnection{
		extendedAllowed: true,
		commandDelay:    70 * time.Millisecond,
		commandOutputAt: 40 * time.Millisecond,
		lockProbeDelay:  20 * time.Millisecond,
	}

	stdout, _, err := runSSHCommandWithTimeout(conn, aptUpgradeCmd, updatespkg.HostCommandEffectPackageStateMutation, nil, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want output during the lock probe to extend the idle deadline", err)
	}
	if !strings.Contains(stdout, "progress\n") {
		t.Fatalf("runSSHCommandWithTimeout() stdout = %q, want progress emitted during the lock probe", stdout)
	}
	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	conn.mu.Unlock()
	if lockProbeCount != 1 {
		t.Fatalf("apt lock probes = %d, want 1 before output extended the idle deadline", lockProbeCount)
	}
}

func TestRunSSHCommandWithTimeoutAllowsQuietAptFinalizationAfterProgress(t *testing.T) {
	conn := &aptLockAwareTestConnection{
		extendedAllowed: true,
		commandDelay:    70 * time.Millisecond,
		commandOutputAt: 20 * time.Millisecond,
	}

	stdout, _, err := runSSHCommandWithTimeout(conn, aptUpdateCmd, updatespkg.HostCommandEffectMetadataMutation, nil, 30*time.Millisecond)
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeout() error = %v, want recent APT progress to allow one quiet finalization window", err)
	}
	if !strings.Contains(stdout, "progress\n") {
		t.Fatalf("runSSHCommandWithTimeout() stdout = %q, want progress before quiet finalization", stdout)
	}
	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	conn.mu.Unlock()
	if lockProbeCount != 1 {
		t.Fatalf("apt lock probes = %d, want one probe before bounded finalization grace", lockProbeCount)
	}
}

func TestRunSSHCommandWithTimeoutKeepsWaitingBeyondPreviousAptExtensionCap(t *testing.T) {
	conn := &aptLockAwareTestConnection{
		lockActive:      true,
		extendedAllowed: true,
		commandDelay:    150 * time.Millisecond,
	}
	var progress strings.Builder

	_, _, err := runSSHCommandWithTimeoutStreaming(conn, aptUpgradeCmd, updatespkg.HostCommandEffectPackageStateMutation, nil, 20*time.Millisecond, func(output updatespkg.HostCommandOutput) {
		progress.WriteString(output.Data)
	})
	if err != nil {
		t.Fatalf("runSSHCommandWithTimeoutStreaming() error = %v, want active apt command to keep waiting", err)
	}

	conn.mu.Lock()
	lockProbeCount := conn.lockProbeCount
	conn.mu.Unlock()
	if lockProbeCount <= 3 {
		t.Fatalf("apt lock probes = %d, want more than previous three-extension cap", lockProbeCount)
	}
	if got := progress.String(); !strings.Contains(got, "APT command still active") || !strings.Contains(got, "checkpoint 4") {
		t.Fatalf("progress output = %q, want observable checkpoint beyond previous cap", got)
	}
}

func TestLoadRetryPolicyFromEnvOverrideAndInvalidFallback(t *testing.T) {
	t.Setenv(retryMaxAttemptsEnv, "5")
	t.Setenv(retryBaseDelayMSEnv, "250")
	t.Setenv(retryMaxDelayMSEnv, "2000")
	t.Setenv(retryJitterPctEnv, "15")
	p := loadRetryPolicyFromEnv()
	if p.MaxAttempts != 5 || p.BaseDelay != 250*time.Millisecond || p.MaxDelay != 2*time.Second || p.JitterPct != 15 {
		t.Fatalf("unexpected override policy: %+v", p)
	}

	t.Setenv(retryMaxAttemptsEnv, "0")
	t.Setenv(retryBaseDelayMSEnv, "-1")
	t.Setenv(retryMaxDelayMSEnv, "-1")
	t.Setenv(retryJitterPctEnv, "999")
	p = loadRetryPolicyFromEnv()
	if p.MaxAttempts != 3 || p.BaseDelay != time.Second || p.MaxDelay != 8*time.Second || p.JitterPct != 20 {
		t.Fatalf("invalid env should fallback to defaults, got %+v", p)
	}
}

func TestLoadSSHCommandTimeoutFromEnv(t *testing.T) {
	t.Run("default when unset", func(t *testing.T) {
		t.Setenv(sshCommandTimeoutSecondsEnv, "")
		if got := loadSSHCommandTimeoutFromEnv(); got != defaultSSHCommandTimeout {
			t.Fatalf("loadSSHCommandTimeoutFromEnv() = %v, want %v", got, defaultSSHCommandTimeout)
		}
	})

	t.Run("valid override", func(t *testing.T) {
		t.Setenv(sshCommandTimeoutSecondsEnv, "30")
		if got := loadSSHCommandTimeoutFromEnv(); got != 30*time.Second {
			t.Fatalf("loadSSHCommandTimeoutFromEnv() = %v, want %v", got, 30*time.Second)
		}
	})

	t.Run("invalid values fallback to default", func(t *testing.T) {
		for _, value := range []string{"0", "1801", "abc"} {
			t.Setenv(sshCommandTimeoutSecondsEnv, value)
			if got := loadSSHCommandTimeoutFromEnv(); got != defaultSSHCommandTimeout {
				t.Fatalf("loadSSHCommandTimeoutFromEnv(%q) = %v, want %v", value, got, defaultSSHCommandTimeout)
			}
		}
	})
}

func TestIsRetryableError(t *testing.T) {
	retryable := []error{
		errors.New("dial tcp: connection refused"),
		errors.New("read: connection reset by peer"),
		errors.New("i/o timeout"),
		errors.New("E: Could not get lock /var/lib/dpkg/lock-frontend"),
	}
	for _, err := range retryable {
		if !updatespkg.IsRetryableError(err) {
			t.Fatalf("isRetryableError(%q) = false, want true", err.Error())
		}
	}

	nonRetryable := []error{
		errors.New("ssh: handshake failed: unable to authenticate"),
		errors.New("host key verification failed"),
		errors.New("missing password or SSH key"),
	}
	for _, err := range nonRetryable {
		if updatespkg.IsRetryableError(err) {
			t.Fatalf("isRetryableError(%q) = true, want false", err.Error())
		}
	}
}

func TestMarkRetryableFromOutputTagsGenericExitError(t *testing.T) {
	err := errors.New("Process exited with status 100")
	tagged := updatespkg.MarkRetryableFromOutput(err, "E: Could not get lock /var/lib/dpkg/lock-frontend")
	if !updatespkg.IsRetryableError(tagged) {
		t.Fatalf("tagged error should be retryable, got: %v", tagged)
	}
}

func TestRunWithRetrySucceedsAfterTransientFailure(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		JitterPct:   0,
	}
	attempts := 0
	retryCalls := 0
	err := updatespkg.RunWithRetryWithSleep(p, "test.op", func() error {
		attempts++
		if attempts < 2 {
			return errors.New("connection reset by peer")
		}
		return nil
	}, func(_ int, _ time.Duration, _ error) {
		retryCalls++
	}, func(_ time.Duration) {}, func(string, ...any) {})
	if err != nil {
		t.Fatalf("runWithRetryWithSleep() error = %v, want nil", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if retryCalls != 1 {
		t.Fatalf("retryCalls = %d, want 1", retryCalls)
	}
}

func TestRunWithRetryStopsOnPermanentError(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		JitterPct:   0,
	}
	attempts := 0
	retryCalls := 0
	err := updatespkg.RunWithRetryWithSleep(p, "test.op", func() error {
		attempts++
		return errors.New("unable to authenticate")
	}, func(_ int, _ time.Duration, _ error) {
		retryCalls++
	}, func(_ time.Duration) {}, func(string, ...any) {})
	if err == nil {
		t.Fatalf("runWithRetryWithSleep() error = nil, want non-nil")
	}
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1", attempts)
	}
	if retryCalls != 0 {
		t.Fatalf("retryCalls = %d, want 0", retryCalls)
	}
}

func TestRunWithRetryExhaustsAttemptsOnTransientError(t *testing.T) {
	p := RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   10 * time.Millisecond,
		MaxDelay:    20 * time.Millisecond,
		JitterPct:   0,
	}
	attempts := 0
	retryCalls := 0
	var waits []time.Duration
	err := updatespkg.RunWithRetryWithSleep(p, "test.op", func() error {
		attempts++
		return errors.New("connection refused")
	}, func(_ int, wait time.Duration, _ error) {
		retryCalls++
		waits = append(waits, wait)
	}, func(_ time.Duration) {}, func(string, ...any) {})
	if err == nil {
		t.Fatalf("runWithRetryWithSleep() error = nil, want non-nil")
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3", attempts)
	}
	if retryCalls != 2 {
		t.Fatalf("retryCalls = %d, want 2", retryCalls)
	}
	if len(waits) != 2 || waits[0] != 10*time.Millisecond || waits[1] != 20*time.Millisecond {
		t.Fatalf("unexpected waits: %v", waits)
	}
}
