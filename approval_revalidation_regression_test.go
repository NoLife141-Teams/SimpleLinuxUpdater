package main

import (
	"context"
	"testing"

	updatespkg "debian-updater/internal/updates"
)

func TestApprovalRevalidationCompletesWhenAnotherUpdaterFinished(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "already-updated", Host: "192.0.2.92", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	discoveries, mutations, approvals := 0, 0, 0
	var service *UpdateService
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			discoveries++
			if discoveries > 1 {
				return HostPackageDiscoveryResult{Attempts: 1}, nil
			}
			return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{{Package: "openssl", CVEState: "done"}}}}, nil
		},
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			if req.Effect == updatespkg.HostCommandEffectPackageStateMutation {
				mutations++
			}
			return HostCommandResult{Attempts: 1}, nil
		},
	}
	service = NewUpdateService(UpdateServiceDeps{
		ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return session, nil
		}),
		WaitForApprovalPollContext:   func(context.Context) error { approvals++; service.ApprovePendingUpdate(server.Name, "all"); return nil },
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		SaveServerFacts:              func(serverFactsRecord) error { return nil },
		AuditWithActor:               func(_, _, _, _, _, _, _ string, _ map[string]any) {},
	})
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "", RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Policy: RetryPolicy{MaxAttempts: 1}})
	if status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name).Status; status != "done" || mutations != 0 || approvals != 1 || discoveries != 2 {
		t.Fatalf("status=%s mutations=%d approvals=%d discoveries=%d", status, mutations, approvals, discoveries)
	}
}
