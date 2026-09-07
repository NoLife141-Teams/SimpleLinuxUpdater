package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
)

func TestApprovalRevalidationRejectsStaleCVEEnrichment(t *testing.T) {
	for _, tc := range []struct {
		name                              string
		sameVersion, scanFails, dialFails bool
	}{
		{name: "assessment"},
		{name: "same_version_plan", sameVersion: true},
		{name: "lookup_failure", scanFails: true},
		{name: "dial_failure", dialFails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "cve-revalidation", Host: "192.0.2.95", User: "root"})
			if err != nil {
				t.Fatal(err)
			}
			oldEntered, releaseOld := make(chan struct{}), make(chan struct{})
			oldDone, freshDone := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseOld) }) }
			defer release()
			await := func(done <-chan struct{}) {
				t.Helper()
				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("CVE regression gate timed out")
				}
			}
			discoveries, approvals, workers := 0, 0, 0
			var cveDials atomic.Int32
			freshVersion := "2.0"
			if tc.sameVersion {
				freshVersion = "1.0"
			}
			t.Cleanup(func() {
				release()
				if workers > 0 {
					await(oldDone)
				}
				if workers > 1 {
					await(freshDone)
				}
			})
			var oldJobID string
			var auditMeta map[string]any
			var service *UpdateService
			session := &HostMaintenanceSessionFuncs{
				DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
					discoveries++
					pkg := PendingUpdate{Package: "openssl", CandidateVersion: "1.0", CVEState: "pending"}
					plan := UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradePackageCount: 1}
					if discoveries > 1 {
						pkg.CandidateVersion, pkg.Security, pkg.KeptBack, pkg.RequiresFull = freshVersion, true, true, true
						plan.FullUpgradeRemovedPackages = []string{"old-runtime"}
					}
					return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{pkg}, UpgradePlan: plan}}, nil
				},
			}
			service = NewUpdateService(UpdateServiceDeps{
				ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager,
				HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
					if tc.dialFails && req.DialOperation == "cve_enrichment.ssh_dial" && cveDials.Add(1) == 1 {
						close(oldEntered)
						<-releaseOld
						return nil, errors.New("old CVE dial failed")
					}
					return session, nil
				}),
				VulnerabilityScanner: updatespkg.VulnerabilityScannerFunc(func(_ context.Context, _ HostMaintenanceSession, pending []PendingUpdate) ([]PendingUpdate, error) {
					result := serverpkg.ClonePendingUpdates(pending)
					if !result[0].Security {
						close(oldEntered)
						<-releaseOld
						if tc.scanFails {
							return nil, errors.New("old vulnerability lookup failed")
						}
					}
					result[0].CVEState = "ready"
					result[0].CVEs = []string{"CVE-old"}
					if result[0].Security {
						result[0].CVEs = []string{"CVE-current"}
					}
					return result, nil
				}),
				StartJobRunner: func(id string, run func()) {
					workers++
					done := freshDone
					if workers == 1 {
						done, oldJobID = oldDone, id
					}
					go func() { defer close(done); run() }()
				},
				WaitForApprovalPollContext: func(context.Context) error {
					approvals++
					if approvals == 1 {
						await(oldEntered)
					} else if approvals == 2 {
						await(freshDone)
						release()
						await(oldDone)
						pkg := app.Deps.ServerState.CurrentStatusSnapshot(server.Name).PendingUpdates[0]
						if pkg.CandidateVersion != freshVersion || !pkg.Security || !pkg.KeptBack || !pkg.RequiresFull || pkg.CVEState != "ready" || len(pkg.CVEs) != 1 || pkg.CVEs[0] != "CVE-current" {
							t.Errorf("stale CVE worker replaced refreshed package: %+v", pkg)
						}
						old, err := app.Deps.CurrentJobManager().GetJob(oldJobID)
						if err != nil || old.Status != jobStatusCancelled {
							t.Errorf("superseded CVE job = %+v, %v; want cancelled", old, err)
						}
					} else {
						t.Fatal("approval did not converge")
					}
					if _, ok := service.ApprovePendingUpdateWithOptions(server.Name, "full_upgrade", serverpkg.ApprovalOptions{ConfirmRemovals: approvals == 2}); !ok {
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
			if approvals != 2 || workers != 2 || auditMeta["status"] != "done" || auditMeta["approval_scope"] != "full_upgrade" {
				t.Fatalf("approvals=%d workers=%d audit=%+v", approvals, workers, auditMeta)
			}
		})
	}
}
