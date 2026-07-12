package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

type HostMaintenanceErrorStage string

const (
	HostMaintenanceStageAuth    HostMaintenanceErrorStage = "auth"
	HostMaintenanceStageHostKey HostMaintenanceErrorStage = "host_key"
	HostMaintenanceStageDial    HostMaintenanceErrorStage = "dial"
	HostMaintenanceStageCommand HostMaintenanceErrorStage = "command"
)

type HostMaintenanceError struct {
	Stage     HostMaintenanceErrorStage
	Operation string
	Attempts  int
	Err       error
}

func (e *HostMaintenanceError) Error() string {
	if e == nil || e.Err == nil {
		return ""
	}
	return e.Err.Error()
}

func (e *HostMaintenanceError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type HostRetryEvent struct {
	Operation   string
	Attempt     int
	MaxAttempts int
	Wait        time.Duration
	Err         error
}

type HostMaintenanceSessionRequest struct {
	Server         servers.Server
	RetryPolicy    RetryPolicy
	DialReplay     HostDialReplayPolicy
	DialOperation  string
	CommandTimeout time.Duration
	OnRetry        func(HostRetryEvent)
}

type HostDialReplayPolicy string

const (
	ReplayRetryableDialErrors HostDialReplayPolicy = "retryable_errors"
	ReplayAllDialErrors       HostDialReplayPolicy = "all_errors"
)

type HostCommandReplayPolicy string

const (
	ReplayRetryableErrors       HostCommandReplayPolicy = "retryable_errors"
	ReplayRetryableOutputErrors HostCommandReplayPolicy = "retryable_output_errors"
)

type HostCommandRequest struct {
	Operation    string
	Command      string
	Stdin        func() io.Reader
	ReplayPolicy HostCommandReplayPolicy
}

type HostCommandResult struct {
	Stdout   string
	Stderr   string
	Attempts int
}

type HostOperationRequest struct {
	Operation string
}

type HostPackageDiscoveryResult struct {
	Outcome  PackageDiscoveryOutcome
	Attempts int
}

type HostMaintenanceSessionStats struct {
	DialAttempts      int
	Reconnects        int
	OperationAttempts map[string]int
}

type HostMaintenanceSessionFactory interface {
	Open(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error)
}

type HostMaintenanceSessionFactoryFunc func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error)

func (f HostMaintenanceSessionFactoryFunc) Open(ctx context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
	return f(ctx, req)
}

type HostMaintenanceSession interface {
	RunCommand(context.Context, HostCommandRequest) (HostCommandResult, error)
	RunUpdatePrechecks(context.Context) PrecheckSummary
	ListFailedSystemdUnits(context.Context) ([]string, string, error)
	RunPostUpdateHealthChecks(context.Context, PostUpdateCheckConfig, map[string]struct{}) PostcheckSummary
	CollectServerFacts(context.Context) ServerFactsRecord
	DiscoverPackages(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error)
	QueryPackageCVEs(context.Context, string) ([]string, error)
	Stats() HostMaintenanceSessionStats
	Close() error
}

type HostMaintenanceSessionFuncs struct {
	RunCommandFunc                func(context.Context, HostCommandRequest) (HostCommandResult, error)
	RunUpdatePrechecksFunc        func(context.Context) PrecheckSummary
	ListFailedSystemdUnitsFunc    func(context.Context) ([]string, string, error)
	RunPostUpdateHealthChecksFunc func(context.Context, PostUpdateCheckConfig, map[string]struct{}) PostcheckSummary
	CollectServerFactsFunc        func(context.Context) ServerFactsRecord
	DiscoverPackagesFunc          func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error)
	QueryPackageCVEsFunc          func(context.Context, string) ([]string, error)
	StatsFunc                     func() HostMaintenanceSessionStats
	CloseFunc                     func() error
}

func (s *HostMaintenanceSessionFuncs) RunCommand(ctx context.Context, req HostCommandRequest) (HostCommandResult, error) {
	if s != nil && s.RunCommandFunc != nil {
		return s.RunCommandFunc(ctx, req)
	}
	return HostCommandResult{Attempts: 1}, nil
}

func (s *HostMaintenanceSessionFuncs) RunUpdatePrechecks(ctx context.Context) PrecheckSummary {
	if s != nil && s.RunUpdatePrechecksFunc != nil {
		return s.RunUpdatePrechecksFunc(ctx)
	}
	return PrecheckSummary{AllPassed: true}
}

func (s *HostMaintenanceSessionFuncs) ListFailedSystemdUnits(ctx context.Context) ([]string, string, error) {
	if s != nil && s.ListFailedSystemdUnitsFunc != nil {
		return s.ListFailedSystemdUnitsFunc(ctx)
	}
	return nil, "", nil
}

func (s *HostMaintenanceSessionFuncs) RunPostUpdateHealthChecks(ctx context.Context, cfg PostUpdateCheckConfig, baseline map[string]struct{}) PostcheckSummary {
	if s != nil && s.RunPostUpdateHealthChecksFunc != nil {
		return s.RunPostUpdateHealthChecksFunc(ctx, cfg, baseline)
	}
	return PostcheckSummary{AllPassed: true}
}

func (s *HostMaintenanceSessionFuncs) CollectServerFacts(ctx context.Context) ServerFactsRecord {
	if s != nil && s.CollectServerFactsFunc != nil {
		return s.CollectServerFactsFunc(ctx)
	}
	return ServerFactsRecord{}
}

func (s *HostMaintenanceSessionFuncs) DiscoverPackages(ctx context.Context, req HostOperationRequest) (HostPackageDiscoveryResult, error) {
	if s != nil && s.DiscoverPackagesFunc != nil {
		return s.DiscoverPackagesFunc(ctx, req)
	}
	return HostPackageDiscoveryResult{Attempts: 1}, nil
}

func (s *HostMaintenanceSessionFuncs) QueryPackageCVEs(ctx context.Context, pkg string) ([]string, error) {
	if s != nil && s.QueryPackageCVEsFunc != nil {
		return s.QueryPackageCVEsFunc(ctx, pkg)
	}
	return []string{}, nil
}

func (s *HostMaintenanceSessionFuncs) Stats() HostMaintenanceSessionStats {
	if s != nil && s.StatsFunc != nil {
		return s.StatsFunc()
	}
	return HostMaintenanceSessionStats{DialAttempts: 1, OperationAttempts: map[string]int{}}
}

func (s *HostMaintenanceSessionFuncs) Close() error {
	if s != nil && s.CloseFunc != nil {
		return s.CloseFunc()
	}
	return nil
}

type ProductionHostMaintenanceSessionDeps struct {
	BuildAuthMethods  func(servers.Server) ([]ssh.AuthMethod, error)
	HostKeyCallback   func() (ssh.HostKeyCallback, error)
	DialSSH           func(servers.Server, *ssh.ClientConfig) (SSHConnection, error)
	RunCommand        func(context.Context, SSHConnection, string, io.Reader, time.Duration) (string, string, error)
	SSHConnectTimeout time.Duration
	Sleep             func(time.Duration)
	Logf              func(string, ...any)
}

type productionHostMaintenanceSessionFactory struct {
	deps ProductionHostMaintenanceSessionDeps
}

func NewProductionHostMaintenanceSessionFactory(deps ProductionHostMaintenanceSessionDeps) HostMaintenanceSessionFactory {
	if deps.SSHConnectTimeout <= 0 {
		deps.SSHConnectTimeout = 15 * time.Second
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return &productionHostMaintenanceSessionFactory{deps: deps}
}

func (f *productionHostMaintenanceSessionFactory) Open(ctx context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil || f.deps.BuildAuthMethods == nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageAuth, Operation: req.DialOperation, Err: errors.New("host maintenance authentication is not configured")}
	}
	authMethods, err := f.deps.BuildAuthMethods(req.Server)
	if err != nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageAuth, Operation: req.DialOperation, Err: err}
	}
	if f.deps.HostKeyCallback == nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageHostKey, Operation: req.DialOperation, Err: errors.New("host key verification is not configured")}
	}
	hostKeyCallback, err := f.deps.HostKeyCallback()
	if err != nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageHostKey, Operation: req.DialOperation, Err: err}
	}
	if f.deps.DialSSH == nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageDial, Operation: req.DialOperation, Err: errors.New("SSH dialer is not configured")}
	}
	config := &ssh.ClientConfig{
		User:            req.Server.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         f.deps.SSHConnectTimeout,
	}
	session := &productionHostMaintenanceSession{
		deps:    f.deps,
		request: req,
		config:  config,
		stats: HostMaintenanceSessionStats{
			OperationAttempts: map[string]int{},
		},
	}
	err = RunWithRetryWithSleep(req.RetryPolicy, req.DialOperation, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		session.stats.DialAttempts++
		conn, dialErr := f.deps.DialSSH(req.Server, config)
		if dialErr == nil {
			session.conn = conn
		}
		if dialErr != nil && req.DialReplay == ReplayAllDialErrors && !IsRetryableError(dialErr) {
			return RetryableTaggedError{Err: dialErr}
		}
		return dialErr
	}, session.notifyRetry(req.DialOperation), func(wait time.Duration) {
		waitForHostRetry(ctx, wait, f.deps.Sleep)
	}, f.deps.Logf)
	if err != nil {
		return nil, &HostMaintenanceError{Stage: HostMaintenanceStageDial, Operation: req.DialOperation, Attempts: session.stats.DialAttempts, Err: err}
	}
	return session, nil
}

type productionHostMaintenanceSession struct {
	deps    ProductionHostMaintenanceSessionDeps
	request HostMaintenanceSessionRequest
	config  *ssh.ClientConfig

	mu       sync.Mutex
	conn     SSHConnection
	closed   bool
	closeErr error
	stats    HostMaintenanceSessionStats
}

func (s *productionHostMaintenanceSession) notifyRetry(operation string) func(int, time.Duration, error) {
	return func(attempt int, wait time.Duration, err error) {
		if s.request.OnRetry != nil {
			s.request.OnRetry(HostRetryEvent{Operation: operation, Attempt: attempt, MaxAttempts: normalizedMaxAttempts(s.request.RetryPolicy), Wait: wait, Err: err})
		}
	}
}

func normalizedMaxAttempts(policy RetryPolicy) int {
	if policy.MaxAttempts < 1 {
		return 1
	}
	return policy.MaxAttempts
}

func (s *productionHostMaintenanceSession) reconnect() error {
	if s.conn != nil {
		_ = s.conn.Close()
		s.conn = nil
	}
	conn, err := s.deps.DialSSH(s.request.Server, s.config)
	if err != nil {
		return err
	}
	s.conn = conn
	s.stats.Reconnects++
	return nil
}

func (s *productionHostMaintenanceSession) runWithRetry(ctx context.Context, operation string, fn func(SSHConnection) error) (int, error) {
	attempts := 0
	err := RunWithRetryWithSleep(s.request.RetryPolicy, operation, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		attempts++
		s.stats.OperationAttempts[operation]++
		if attempts > 1 {
			if err := s.reconnect(); err != nil {
				return err
			}
		}
		return fn(s.conn)
	}, s.notifyRetry(operation), func(wait time.Duration) {
		waitForHostRetry(ctx, wait, s.deps.Sleep)
	}, s.deps.Logf)
	if err != nil {
		return attempts, &HostMaintenanceError{Stage: HostMaintenanceStageCommand, Operation: operation, Attempts: attempts, Err: err}
	}
	return attempts, nil
}

func waitForHostRetry(ctx context.Context, wait time.Duration, sleep func(time.Duration)) {
	if sleep == nil {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
		return
	}
	sleepDone := make(chan struct{})
	go func() {
		sleep(wait)
		close(sleepDone)
	}()
	select {
	case <-ctx.Done():
	case <-sleepDone:
	}
}

func (s *productionHostMaintenanceSession) RunCommand(ctx context.Context, req HostCommandRequest) (HostCommandResult, error) {
	if s.deps.RunCommand == nil {
		return HostCommandResult{}, errors.New("host command runner is not configured")
	}
	var stdout, stderr string
	attempts, err := s.runWithRetry(ctx, req.Operation, func(conn SSHConnection) error {
		var runErr error
		var stdin io.Reader
		if req.Stdin != nil {
			stdin = req.Stdin()
		}
		stdout, stderr, runErr = s.deps.RunCommand(ctx, conn, req.Command, stdin, s.request.CommandTimeout)
		if req.ReplayPolicy == ReplayRetryableOutputErrors {
			return MarkRetryableFromOutput(runErr, stdout+"\n"+stderr)
		}
		return runErr
	})
	return HostCommandResult{Stdout: stdout, Stderr: stderr, Attempts: attempts}, err
}

func (s *productionHostMaintenanceSession) RunUpdatePrechecks(ctx context.Context) PrecheckSummary {
	return s.runUpdatePrechecks(ctx)
}

func (s *productionHostMaintenanceSession) ListFailedSystemdUnits(ctx context.Context) ([]string, string, error) {
	return s.listFailedSystemdUnits(ctx)
}

func (s *productionHostMaintenanceSession) RunPostUpdateHealthChecks(ctx context.Context, cfg PostUpdateCheckConfig, baseline map[string]struct{}) PostcheckSummary {
	return s.runPostUpdateHealthChecks(ctx, cfg, baseline)
}

func (s *productionHostMaintenanceSession) CollectServerFacts(ctx context.Context) ServerFactsRecord {
	return s.collectServerFacts(ctx)
}

func (s *productionHostMaintenanceSession) DiscoverPackages(ctx context.Context, req HostOperationRequest) (HostPackageDiscoveryResult, error) {
	if s.deps.RunCommand == nil {
		return HostPackageDiscoveryResult{}, errors.New("package discovery is not configured")
	}
	var outcome PackageDiscoveryOutcome
	attempts, err := s.runWithRetry(ctx, req.Operation, func(conn SSHConnection) error {
		var discoverErr error
		outcome, discoverErr = DiscoverPackageUpdates(conn, s.request.CommandTimeout, func(conn SSHConnection, command string, stdin io.Reader, timeout time.Duration) (string, string, error) {
			return s.deps.RunCommand(ctx, conn, command, stdin, timeout)
		})
		return discoverErr
	})
	return HostPackageDiscoveryResult{Outcome: outcome, Attempts: attempts}, err
}

func (s *productionHostMaintenanceSession) QueryPackageCVEs(ctx context.Context, pkg string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.deps.RunCommand == nil {
		return nil, errors.New("CVE query command runner is not configured")
	}
	stdout, _, err := s.deps.RunCommand(ctx, s.conn, BuildPackageCVEQueryCmd(pkg), nil, CVELookupCommandTimeout)
	if err != nil {
		return nil, err
	}
	return ExtractCVEsFromText(stdout, CVELookupMaxPerPackage), nil
}

func (s *productionHostMaintenanceSession) Stats() HostMaintenanceSessionStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	stats := s.stats
	stats.OperationAttempts = make(map[string]int, len(s.stats.OperationAttempts))
	for operation, attempts := range s.stats.OperationAttempts {
		stats.OperationAttempts[operation] = attempts
	}
	return stats
}

func (s *productionHostMaintenanceSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	if s.conn != nil {
		s.closeErr = s.conn.Close()
		s.conn = nil
	}
	return s.closeErr
}

func HostMaintenanceErrorStageOf(err error) HostMaintenanceErrorStage {
	var sessionErr *HostMaintenanceError
	if errors.As(err, &sessionErr) {
		return sessionErr.Stage
	}
	return ""
}

func hostMaintenanceUnavailableFactory() HostMaintenanceSessionFactory {
	return HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return nil, fmt.Errorf("host maintenance session factory is not configured")
	})
}
