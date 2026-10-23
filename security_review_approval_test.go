package main

import (
	"context"
	"testing"
	"time"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

func TestSecurityReviewApprovalRequiresNewConsentForCandidateVersion(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "candidate-drift", Host: "192.0.2.91", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	var service *UpdateService
	discoveries, approvals, mutations := 0, 0, 0
	var auditMeta map[string]any
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			discoveries++
			candidate := "1.0"
			if discoveries > 1 {
				candidate = "2.0"
			}
			return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{
				PendingPackageCount: 1,
				Upgradable:          []string{"openssl"},
				PendingUpdates:      []PendingUpdate{{Package: "openssl", CurrentVersion: "0.9", CandidateVersion: candidate, CVEState: "done"}},
				UpgradePlan:         UpgradePlan{StandardPackageCount: 1},
			}}, nil
		},
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			if req.Effect == updatespkg.HostCommandEffectPackageStateMutation {
				mutations++
				if approvals != 2 || discoveries != 3 {
					t.Errorf("package mutation before approval of new candidate: approvals=%d discoveries=%d", approvals, discoveries)
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
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
		LoadScheduledJobBehavior:  func(string) scheduledJobBehavior { return scheduledJobBehavior{ApprovalTimeout: time.Minute} },
		WaitForApprovalPollContext: func(context.Context) error {
			approvals++
			if approvals > 2 {
				t.Fatal("approval did not converge")
			}
			if approvals == 2 {
				status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
				if status.ApprovalGeneration != 2 || status.PendingUpdates[0].CandidateVersion != "2.0" {
					t.Fatalf("new candidate not published for renewed approval: %+v", status)
				}
			}
			if exists, approved := service.ApprovePendingUpdateWithOptions(server.Name, updatespkg.ApprovalScopeAll, serverpkg.ApprovalOptions{}); !exists || !approved {
				t.Fatalf("approval %d failed: exists=%t approved=%t", approvals, exists, approved)
			}
			return nil
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
		SaveServerFacts:              func(serverFactsRecord) error { return nil },
		AuditWithActor:               func(_, _, _, _, _, _, _ string, meta map[string]any) { auditMeta = meta },
	})
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "127.0.0.1", RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Actor: "review", Policy: RetryPolicy{MaxAttempts: 1}})
	if approvals != 2 || discoveries != 3 || mutations != 1 {
		t.Fatalf("approvals=%d discoveries=%d mutations=%d", approvals, discoveries, mutations)
	}
	if auditMeta["approval_scope"] != updatespkg.ApprovalScopeAll || auditMeta["approved_package_count"] != 1 {
		t.Fatalf("audit does not describe approved execution: %+v", auditMeta)
	}
}
