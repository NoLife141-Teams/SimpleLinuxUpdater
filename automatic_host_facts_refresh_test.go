package main

import (
	"context"
	"sync"
	"testing"
	"time"

	healthpkg "debian-updater/internal/health"
	maintenancepkg "debian-updater/internal/maintenance"
	serverpkg "debian-updater/internal/servers"
)

func TestAutomaticHostFactsRefreshAttemptCoordinatesMaintenanceAndServerState(t *testing.T) {
	server := Server{Name: "stale", Host: "example.org", Port: 22, User: "root"}
	serverList := []Server{server}
	statuses := map[string]*ServerStatus{server.Name: {Name: server.Name, Status: "done", Logs: "previous"}}
	state := serverpkg.NewState(&sync.Mutex{}, &serverList, &statuses, nil)
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	rebootRequired := false
	saved := false
	service := NewUpdateService(UpdateServiceDeps{
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return &HostMaintenanceSessionFuncs{
				CollectServerFactsFunc: func(context.Context) serverFactsRecord {
					return serverFactsRecord{ServerName: server.Name, CollectedAt: "2026-08-31T12:00:00Z", DiskStatus: "ok", AptStatus: "ok", RebootRequired: &rebootRequired}
				},
			}, nil
		}),
		SaveServerFacts: func(record serverFactsRecord) error {
			saved = record.ServerName == server.Name
			return nil
		},
	})
	deps := AppDeps{
		ServerState:            state,
		UpdateService:          service,
		MaintenanceCoordinator: coordinator,
		MaintenanceReadiness: func([]Server) map[string]serverpkg.MaintenanceReadiness {
			return map[string]serverpkg.MaintenanceReadiness{server.Name: {Ready: true, Code: serverpkg.MaintenanceReadinessReady}}
		},
	}

	attempt := automaticHostFactsRefreshAttempt(context.Background(), deps, server)
	if attempt.State != healthpkg.RefreshAttemptSucceeded || !saved {
		t.Fatalf("attempt=%+v saved=%t", attempt, saved)
	}
	if got := state.CurrentStatusSnapshot(server.Name); got == nil || got.Status != "done" || got.Logs != "previous" {
		t.Fatalf("restored status = %+v", got)
	}
	exclusive, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("automatic refresh did not release the shared maintenance lease")
	}
	if err := exclusive.Close(); err != nil {
		t.Fatalf("close exclusive lease: %v", err)
	}
}

func TestAutomaticHostFactsRefreshAttemptDefersBusyServerWithoutSSH(t *testing.T) {
	server := Server{Name: "busy", Host: "example.org", Port: 22, User: "root"}
	serverList := []Server{server}
	statuses := map[string]*ServerStatus{server.Name: {Name: server.Name, Status: "updating"}}
	state := serverpkg.NewState(&sync.Mutex{}, &serverList, &statuses, nil)
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	opened := false
	deps := AppDeps{
		ServerState: state,
		UpdateService: NewUpdateService(UpdateServiceDeps{
			HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
				opened = true
				return &HostMaintenanceSessionFuncs{}, nil
			}),
		}),
		MaintenanceCoordinator: coordinator,
		MaintenanceReadiness: func([]Server) map[string]serverpkg.MaintenanceReadiness {
			return map[string]serverpkg.MaintenanceReadiness{server.Name: {Ready: true, Code: serverpkg.MaintenanceReadinessReady}}
		},
	}

	attempt := automaticHostFactsRefreshAttempt(context.Background(), deps, server)
	if attempt.State != healthpkg.RefreshAttemptDeferred || attempt.ReasonCode != "busy" || opened {
		t.Fatalf("attempt=%+v opened=%t", attempt, opened)
	}
}

func TestAutomaticHostFactsRefreshWorkerStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	worker := healthpkg.NewRefreshWorker(healthpkg.RefreshWorkerDeps{}, healthpkg.RefreshWorkerOptions{SweepInterval: time.Hour})
	worker.Start(ctx)
	cancel()
	done := make(chan struct{})
	go func() {
		worker.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("worker did not stop after context cancellation")
	}
}
