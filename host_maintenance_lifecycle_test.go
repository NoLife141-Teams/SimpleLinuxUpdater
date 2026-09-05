package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

func TestLifecycleHostMaintenanceSessionCancelsReadOnlyCommand(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	inner := updatespkg.HostMaintenanceSessionFactoryFunc(func(context.Context, updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
		return &updatespkg.HostMaintenanceSessionFuncs{
			RunCommandFunc: func(ctx context.Context, _ updatespkg.HostCommandRequest) (updatespkg.HostCommandResult, error) {
				close(started)
				<-ctx.Done()
				return updatespkg.HostCommandResult{}, ctx.Err()
			},
		}, nil
	})
	factory := newLifecycleHostMaintenanceSessionFactory(lifecycle, inner)
	session, err := factory.Open(context.Background(), updatespkg.HostMaintenanceSessionRequest{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer session.Close()

	resultCh := make(chan error, 1)
	go func() {
		_, err := session.RunCommand(context.Background(), updatespkg.HostCommandRequest{
			Operation: "facts.read",
			Effect:    updatespkg.HostCommandEffectReadOnly,
		})
		resultCh <- err
	}()
	<-started
	stop()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCommand() error = %v, want context canceled", err)
		}
		var reconciliation interface{ RequiresReconciliation() bool }
		if errors.As(err, &reconciliation) && reconciliation.RequiresReconciliation() {
			t.Fatalf("read-only cancellation unexpectedly requires reconciliation: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("RunCommand() did not stop after lifecycle cancellation")
	}
}

func TestLifecycleHostMaintenanceSessionMarksPackageMutationForReconciliationAndPreservesStreamedOutput(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	inner := updatespkg.HostMaintenanceSessionFactoryFunc(func(context.Context, updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
		return &updatespkg.HostMaintenanceSessionFuncs{
			RunCommandFunc: func(ctx context.Context, req updatespkg.HostCommandRequest) (updatespkg.HostCommandResult, error) {
				close(started)
				if req.OnOutput == nil {
					t.Fatal("package mutation did not enable output capture")
				}
				req.OnOutput(updatespkg.HostCommandOutput{Stream: updatespkg.HostCommandStdout, Data: "Setting up packages...\n"})
				req.OnOutput(updatespkg.HostCommandOutput{Stream: updatespkg.HostCommandStderr, Data: "dpkg: processing triggers\n"})
				<-ctx.Done()
				// Match the production context wrapper: cancellation may return empty
				// aggregate buffers even though output already streamed.
				return updatespkg.HostCommandResult{}, ctx.Err()
			},
		}, nil
	})
	factory := newLifecycleHostMaintenanceSessionFactory(lifecycle, inner)
	session, err := factory.Open(context.Background(), updatespkg.HostMaintenanceSessionRequest{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer session.Close()

	type result struct {
		command updatespkg.HostCommandResult
		err     error
	}
	resultCh := make(chan result, 1)
	go func() {
		command, err := session.RunCommand(context.Background(), updatespkg.HostCommandRequest{
			Operation: "update.apt_upgrade",
			Effect:    updatespkg.HostCommandEffectPackageStateMutation,
		})
		resultCh <- result{command: command, err: err}
	}()
	<-started
	stop()

	select {
	case got := <-resultCh:
		if got.command.Stdout != "Setting up packages...\n" {
			t.Fatalf("RunCommand() stdout = %q, want preserved streamed output", got.command.Stdout)
		}
		if got.command.Stderr != "dpkg: processing triggers\n" {
			t.Fatalf("RunCommand() stderr = %q, want preserved streamed output", got.command.Stderr)
		}
		var reconciliation interface{ RequiresReconciliation() bool }
		if !errors.As(got.err, &reconciliation) || !reconciliation.RequiresReconciliation() {
			t.Fatalf("RunCommand() error = %v, want reconciliation-required error", got.err)
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("RunCommand() error = %v, want wrapped context cancellation", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("package mutation did not stop after lifecycle cancellation")
	}
}

func TestLifecycleHostMaintenanceSessionPreservesCallerCancellation(t *testing.T) {
	lifecycle := context.Background()
	inner := updatespkg.HostMaintenanceSessionFactoryFunc(func(context.Context, updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
		return &updatespkg.HostMaintenanceSessionFuncs{
			DiscoverPackagesFunc: func(ctx context.Context, _ updatespkg.HostOperationRequest) (updatespkg.HostPackageDiscoveryResult, error) {
				<-ctx.Done()
				return updatespkg.HostPackageDiscoveryResult{}, ctx.Err()
			},
		}, nil
	})
	factory := newLifecycleHostMaintenanceSessionFactory(lifecycle, inner)
	session, err := factory.Open(context.Background(), updatespkg.HostMaintenanceSessionRequest{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer session.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = session.DiscoverPackages(ctx, updatespkg.HostOperationRequest{Operation: "list"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("DiscoverPackages() error = %v, want caller context cancellation", err)
	}
}

func TestLifecycleHostMaintenanceSessionCloseCancelsInFlightOperation(t *testing.T) {
	started := make(chan struct{})
	var closes atomic.Int32
	inner := updatespkg.HostMaintenanceSessionFactoryFunc(func(context.Context, updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
		return &updatespkg.HostMaintenanceSessionFuncs{
			RunCommandFunc: func(ctx context.Context, _ updatespkg.HostCommandRequest) (updatespkg.HostCommandResult, error) {
				close(started)
				<-ctx.Done()
				return updatespkg.HostCommandResult{}, ctx.Err()
			},
			CloseFunc: func() error {
				closes.Add(1)
				return nil
			},
		}, nil
	})
	factory := newLifecycleHostMaintenanceSessionFactory(context.Background(), inner)
	session, err := factory.Open(context.Background(), updatespkg.HostMaintenanceSessionRequest{})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	resultCh := make(chan error, 1)
	go func() {
		_, err := session.RunCommand(context.Background(), updatespkg.HostCommandRequest{
			Operation: "probe",
			Effect:    updatespkg.HostCommandEffectReadOnly,
		})
		resultCh <- err
	}()
	<-started
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if closes.Load() != 1 {
		t.Fatalf("inner Close() calls = %d, want 1", closes.Load())
	}
	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("RunCommand() error after Close = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close() did not cancel in-flight operation")
	}
}

func TestLifecycleVulnerabilityScannerCancelsInFlightLookup(t *testing.T) {
	lifecycle, stop := context.WithCancel(context.Background())
	started := make(chan struct{})
	inner := updatespkg.VulnerabilityScannerFunc(func(ctx context.Context, _ updatespkg.HostMaintenanceSession, _ []serverpkg.PendingUpdate) ([]serverpkg.PendingUpdate, error) {
		close(started)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	scanner := newLifecycleVulnerabilityScanner(lifecycle, inner)

	resultCh := make(chan error, 1)
	go func() {
		_, err := scanner.Scan(context.Background(), &updatespkg.HostMaintenanceSessionFuncs{}, []serverpkg.PendingUpdate{{Package: "openssl"}})
		resultCh <- err
	}()
	<-started
	stop()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Scan() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("vulnerability scanner did not stop after lifecycle cancellation")
	}
}
