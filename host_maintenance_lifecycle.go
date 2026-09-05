package main

import (
	"context"
	"fmt"
	"strings"
	"sync"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

// lifecycleHostMaintenanceSessionFactory binds every SSH-backed maintenance
// session to the application lifecycle. Callers still retain their own request
// cancellation, but process shutdown can no longer leave background maintenance
// detached from the server lifecycle.
type lifecycleHostMaintenanceSessionFactory struct {
	lifecycle context.Context
	inner     updatespkg.HostMaintenanceSessionFactory
}

func newLifecycleHostMaintenanceSessionFactory(lifecycle context.Context, inner updatespkg.HostMaintenanceSessionFactory) updatespkg.HostMaintenanceSessionFactory {
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	return &lifecycleHostMaintenanceSessionFactory{lifecycle: lifecycle, inner: inner}
}

func (f *lifecycleHostMaintenanceSessionFactory) Open(ctx context.Context, req updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
	if f == nil || f.inner == nil {
		return nil, fmt.Errorf("host maintenance session factory is unavailable")
	}
	sessionCtx, cancel := mergeMaintenanceContexts(ctx, f.lifecycle)
	if err := sessionCtx.Err(); err != nil {
		cancel()
		return nil, err
	}
	inner, err := f.inner.Open(sessionCtx, req)
	if err != nil {
		cancel()
		return nil, err
	}
	return &lifecycleHostMaintenanceSession{
		lifecycle: sessionCtx,
		cancel:    cancel,
		inner:     inner,
	}, nil
}

type lifecycleHostMaintenanceSession struct {
	lifecycle context.Context
	cancel    context.CancelFunc
	inner     updatespkg.HostMaintenanceSession

	closeOnce sync.Once
	closeErr  error
}

// MaintenanceContext exposes the session lifecycle to maintenance collaborators
// that are not SSH operations themselves (for example, OSV HTTP lookups and
// final persistence checks). The updates package consumes it through a small
// structural interface so the core HostMaintenanceSession contract stays stable.
func (s *lifecycleHostMaintenanceSession) MaintenanceContext() context.Context {
	if s == nil || s.lifecycle == nil {
		return context.Background()
	}
	return s.lifecycle
}

func (s *lifecycleHostMaintenanceSession) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if s == nil {
		return mergeMaintenanceContexts(ctx, context.Background())
	}
	return mergeMaintenanceContexts(ctx, s.lifecycle)
}

type lifecycleCommandCapture struct {
	mu      sync.Mutex
	stdout  strings.Builder
	stderr  strings.Builder
	forward updatespkg.HostCommandOutputHandler
}

func (c *lifecycleCommandCapture) handle(output updatespkg.HostCommandOutput) {
	if c == nil {
		return
	}
	c.mu.Lock()
	switch output.Stream {
	case updatespkg.HostCommandStdout:
		c.stdout.WriteString(output.Data)
	case updatespkg.HostCommandStderr:
		c.stderr.WriteString(output.Data)
	}
	forward := c.forward
	c.mu.Unlock()
	if forward != nil {
		forward(output)
	}
}

func (c *lifecycleCommandCapture) values() (string, string) {
	if c == nil {
		return "", ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stdout.String(), c.stderr.String()
}

func (s *lifecycleHostMaintenanceSession) RunCommand(ctx context.Context, req updatespkg.HostCommandRequest) (updatespkg.HostCommandResult, error) {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	if err := opCtx.Err(); err != nil {
		return updatespkg.HostCommandResult{}, err
	}

	// Package-state mutations need their already-produced output if shutdown
	// closes the SSH connection. Force the production streaming path so output is
	// captured before the context-aware command wrapper returns cancellation.
	var capture *lifecycleCommandCapture
	wrappedReq := req
	if req.Effect.RequiresReconciliationOnUnknownOutcome() {
		capture = &lifecycleCommandCapture{forward: req.OnOutput}
		wrappedReq.OnOutput = capture.handle
	}

	result, err := s.inner.RunCommand(opCtx, wrappedReq)
	if err == nil || s.lifecycle == nil || s.lifecycle.Err() == nil {
		return result, err
	}
	if req.Effect.RequiresReconciliationOnUnknownOutcome() {
		capturedStdout, capturedStderr := capture.values()
		if result.Stdout == "" {
			result.Stdout = capturedStdout
		}
		if result.Stderr == "" {
			result.Stderr = capturedStderr
		}
		return result, updatespkg.NonRetryableTaggedError{
			Err:                    fmt.Errorf("APT command outcome is unknown; application shutdown interrupted command: %w", err),
			ReconciliationRequired: true,
		}
	}
	return result, err
}

func (s *lifecycleHostMaintenanceSession) RunUpdatePrechecks(ctx context.Context) updatespkg.PrecheckSummary {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	if err := opCtx.Err(); err != nil {
		return shutdownPrecheckSummary(err)
	}
	summary := s.inner.RunUpdatePrechecks(opCtx)
	if s.lifecycle != nil && s.lifecycle.Err() != nil {
		return shutdownPrecheckSummary(s.lifecycle.Err())
	}
	return summary
}

func shutdownPrecheckSummary(err error) updatespkg.PrecheckSummary {
	detail := "application shutdown interrupted maintenance pre-checks"
	if err != nil {
		detail += ": " + err.Error()
	}
	return updatespkg.PrecheckSummary{
		AllPassed:   false,
		FailedCheck: "application_shutdown",
		Results: []updatespkg.PrecheckResult{{
			Name:    "application_shutdown",
			Passed:  false,
			Details: detail,
		}},
	}
}

func (s *lifecycleHostMaintenanceSession) RunPlanDiskPrecheck(ctx context.Context, plan serverpkg.UpgradePlan) updatespkg.PrecheckResult {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.RunPlanDiskPrecheck(opCtx, plan)
}

func (s *lifecycleHostMaintenanceSession) ListFailedSystemdUnits(ctx context.Context) ([]string, string, error) {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.ListFailedSystemdUnits(opCtx)
}

func (s *lifecycleHostMaintenanceSession) RunPostUpdateHealthChecks(ctx context.Context, cfg updatespkg.PostUpdateCheckConfig, baseline map[string]struct{}) updatespkg.PostcheckSummary {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.RunPostUpdateHealthChecks(opCtx, cfg, baseline)
}

func (s *lifecycleHostMaintenanceSession) CollectServerFacts(ctx context.Context) updatespkg.ServerFactsRecord {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.CollectServerFacts(opCtx)
}

func (s *lifecycleHostMaintenanceSession) DiscoverPackages(ctx context.Context, req updatespkg.HostOperationRequest) (updatespkg.HostPackageDiscoveryResult, error) {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.DiscoverPackages(opCtx, req)
}

func (s *lifecycleHostMaintenanceSession) QueryPackageCVEs(ctx context.Context, pkg string) ([]string, error) {
	opCtx, cancel := s.operationContext(ctx)
	defer cancel()
	return s.inner.QueryPackageCVEs(opCtx, pkg)
}

func (s *lifecycleHostMaintenanceSession) Stats() updatespkg.HostMaintenanceSessionStats {
	if s == nil || s.inner == nil {
		return updatespkg.HostMaintenanceSessionStats{}
	}
	return s.inner.Stats()
}

func (s *lifecycleHostMaintenanceSession) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.inner != nil {
			s.closeErr = s.inner.Close()
		}
	})
	return s.closeErr
}

// lifecycleVulnerabilityScanner binds non-SSH vulnerability work, notably OSV
// HTTP requests, to the same maintenance lifecycle used by the SSH session.
type lifecycleVulnerabilityScanner struct {
	lifecycle context.Context
	inner     updatespkg.VulnerabilityScanner
}

func newLifecycleVulnerabilityScanner(lifecycle context.Context, inner updatespkg.VulnerabilityScanner) updatespkg.VulnerabilityScanner {
	if inner == nil {
		return nil
	}
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	if existing, ok := inner.(*lifecycleVulnerabilityScanner); ok && existing.lifecycle == lifecycle {
		return inner
	}
	return &lifecycleVulnerabilityScanner{lifecycle: lifecycle, inner: inner}
}

func (s *lifecycleVulnerabilityScanner) Scan(ctx context.Context, session updatespkg.HostMaintenanceSession, pending []serverpkg.PendingUpdate) ([]serverpkg.PendingUpdate, error) {
	if s == nil || s.inner == nil {
		return nil, fmt.Errorf("vulnerability scanner is unavailable")
	}
	opCtx, cancel := mergeMaintenanceContexts(ctx, s.lifecycle)
	defer cancel()
	return s.inner.Scan(opCtx, session, pending)
}

func mergeMaintenanceContexts(operation, lifecycle context.Context) (context.Context, context.CancelFunc) {
	if operation == nil {
		operation = context.Background()
	}
	if lifecycle == nil {
		lifecycle = context.Background()
	}
	ctx, cancel := context.WithCancel(operation)
	stopLifecycle := context.AfterFunc(lifecycle, cancel)
	if lifecycle.Err() != nil {
		cancel()
	}
	return ctx, func() {
		stopLifecycle()
		cancel()
	}
}
