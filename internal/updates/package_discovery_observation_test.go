package updates

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

func TestRunUpdateJobPublishesEmptyPlanOnlyAfterSuccessfulDiscovery(t *testing.T) {
	for _, outcome := range []string{"empty plan", "discovery failure", "metadata failure with held packages"} {
		t.Run(outcome, func(t *testing.T) {
			now := time.Date(2026, 11, 3, 1, 0, 0, 0, time.UTC)
			server := servers.Server{Name: "empty-plan", Host: "synthetic.invalid", User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "idle"}}
			state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
			var auditMeta map[string]any
			service := NewService(ServiceDeps{
				Now:         func() time.Time { return now },
				ServerState: state,
				HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{
					RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} },
					RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
						if req.Operation != "update.apt_update" {
							t.Fatalf("unexpected mutation without a package plan: %s", req.Operation)
						}
						return HostCommandResult{Attempts: 1}, nil
					},
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						if outcome == "discovery failure" {
							return HostPackageDiscoveryResult{Attempts: 1}, errors.New("synthetic discovery failure")
						}
						runner := &packageDiscoveryCommandRunner{}
						if outcome == "metadata failure with held packages" {
							runner.responses = map[string]packageDiscoveryCommandResponse{
								AptListUpgradableCmd: {stdout: "The following packages have been kept back:\n  held-package\n0 upgraded, 0 newly installed, 0 to remove and 1 not upgraded.\n"},
								AptListMetadataCmd:   {err: errors.New("synthetic metadata failure")},
							}
						}
						discovery, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
						return HostPackageDiscoveryResult{Outcome: discovery, Attempts: 1}, err
					},
				}),
				CurrentJobManager: func() *jobs.Manager { return nil },
				SaveServerFacts:   func(ServerFactsRecord) error { return nil },
				AuditWithActor: func(_, _, action, _, _, _, _ string, meta map[string]any) {
					if action == "update.complete" {
						auditMeta = meta
					}
				},
			})
			service.RunUpdateJob(UpdateRunRequest{Server: server, Actor: "tester", Policy: RetryPolicy{MaxAttempts: 1}})
			status := state.CurrentStatusSnapshot(server.Name)
			wantStatus, wantObservation := "done", now.Format(time.RFC3339Nano)
			if outcome == "discovery failure" {
				wantStatus, wantObservation = "error", ""
			} else if outcome == "metadata failure with held packages" {
				wantObservation = ""
			}
			if status.Status != wantStatus || status.APTMetadata.LastRefreshState != "succeeded" || status.EmptyPackageDiscoveryRefreshAt != wantObservation {
				t.Fatalf("discovery outcome lost: %+v", status)
			}
			if auditMeta["status"] != wantStatus || auditMeta["list_upgradable_attempts_used"] != 1 || auditMeta["upgrade_completed"] != false || auditMeta["approval_scope"] != "none" {
				t.Fatalf("unexpected discovery-only audit: %+v", auditMeta)
			}
		})
	}
}
