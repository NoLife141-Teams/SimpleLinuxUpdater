package main

import (
	"context"
	"slices"
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

func TestApprovalRevalidationRequiresNewDependencyApproval(t *testing.T) {
	for _, scope := range []string{"full_upgrade", "security_kept_back"} {
		t.Run(scope, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "new-dependency", Host: "192.0.2.93", User: "root"})
			if err != nil {
				t.Fatal(err)
			}
			discoveries, approvals, mutations := 0, 0, 0
			var auditMeta map[string]any
			var service *UpdateService
			session := &HostMaintenanceSessionFuncs{
				DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
					discoveries++
					plan := UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradePackageCount: 1, KeptBackSecurityPlanAvailable: true, KeptBackSecurityPackageCount: 1}
					if discoveries > 1 {
						if scope == "full_upgrade" {
							plan.FullUpgradeNewPackages = []string{"new-runtime"}
						} else {
							plan.KeptBackSecurityNewPackages = []string{"new-runtime"}
						}
					}
					return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{{Package: "openssl", CVEState: "done", Security: true, KeptBack: scope == "security_kept_back", RequiresFull: scope == "security_kept_back"}}, UpgradePlan: plan}}, nil
				},
				RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
					if req.Effect == updatespkg.HostCommandEffectPackageStateMutation {
						mutations++
						if approvals != 2 {
							t.Errorf("dependency installed after %d approvals; want renewed approval", approvals)
						}
					}
					return HostCommandResult{Attempts: 1}, nil
				},
			}
			service = NewUpdateService(UpdateServiceDeps{
				ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager,
				HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
					return session, nil
				}),
				WaitForApprovalPollContext: func(context.Context) error {
					approvals++
					if approvals > 2 {
						t.Fatal("approval did not converge")
					}
					if approvals == 2 {
						plan := app.Deps.ServerState.CurrentStatusSnapshot(server.Name).UpgradePlan
						added := plan.FullUpgradeNewPackages
						if scope == "security_kept_back" {
							added = plan.KeptBackSecurityNewPackages
						}
						if !slices.Equal(added, []string{"new-runtime"}) {
							t.Fatalf("renewed approval omitted new dependencies: %+v", plan)
						}
					}
					if _, ok := service.ApprovePendingUpdate(server.Name, scope); !ok {
						t.Fatal("approval rejected")
					}
					return nil
				},
				LoadPostUpdateCheckConfig:    func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				SaveServerFacts:              func(serverFactsRecord) error { return nil },
				AuditWithActor:               func(_, _, _, _, _, _, _ string, meta map[string]any) { auditMeta = meta },
			})
			job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "", RetryPolicy{MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Policy: RetryPolicy{MaxAttempts: 1}})
			if approvals != 2 || discoveries != 3 || mutations != 1 {
				t.Fatalf("approvals=%d discoveries=%d mutations=%d", approvals, discoveries, mutations)
			}
			plan := auditMeta["upgrade_plan"].(UpgradePlan)
			added := plan.FullUpgradeNewPackages
			if scope == "security_kept_back" {
				added = plan.KeptBackSecurityNewPackages
			}
			if !slices.Equal(added, []string{"new-runtime"}) || auditMeta["approval_scope"] != scope || auditMeta["status"] != "done" {
				t.Fatalf("audit omitted approved dependency plan: %+v", auditMeta)
			}
		})
	}
}
