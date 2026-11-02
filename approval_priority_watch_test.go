package main

import (
	"context"
	"testing"
	"time"

	"debian-updater/internal/observability"
	"debian-updater/internal/securityintel"
	"debian-updater/internal/vulnerabilities"
)

func TestRunUpdateRetainsApprovalOnlyCriticalWatchAfterCancellation(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "approval-priority", Host: "192.0.2.96", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	now := app.Deps.Now().UTC()
	at := now.Add(-time.Hour).Format(time.RFC3339)
	f := VulnerabilityFinding{ID: "CVE-2026-9999", SourcePackage: "example", Severity: "critical", SeveritySource: "synthetic", SeverityObservedAt: at, PublishedAt: at, Assessment: vulnerabilities.Assessment{Ecosystem: "Debian:12", SourcePackage: "example", SourceVersion: "1", VendorStatus: "affected", Confidence: "vendor_confirmed", Applicability: "source_match", Source: "synthetic", ObservedAt: at}, Availability: &vulnerabilities.FixAvailability{Published: vulnerabilities.PublishedFix{State: "not_established", Source: "synthetic", ObservedAt: at, Ranges: []vulnerabilities.FixRange{{Type: "ECOSYSTEM", Events: []vulnerabilities.VersionEvent{{Introduced: "0"}}}}}, CandidateState: "affected"}}
	deps := testUpdateServiceDeps(t)
	deps.ServerState = app.Deps.ServerState
	deps.CurrentJobManager = app.Deps.CurrentJobManager
	deps.RecordApprovalFindings = app.Deps.UpdateService.EnsureDeps().RecordApprovalFindings
	if deps.RecordApprovalFindings == nil {
		t.Fatal("composed runtime has no positive-history admission hook")
	}
	deps.Now = func() time.Time { return now }
	deps.StartJobRunner = func(_ string, run func()) { run() }
	deps.HostMaintenanceSessions = testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
		return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"example"}, PendingUpdates: []PendingUpdate{{Package: "example", InstallPackage: "example:amd64", CurrentVersion: "1", CandidateVersion: "2", CVEState: "pending"}}, UpgradePlan: UpgradePlan{StandardPackageCount: 1}}}, nil
	}})
	deps.VulnerabilityScanner = VulnerabilityScannerFunc(func(_ context.Context, _ HostMaintenanceSession, pending []PendingUpdate) ([]PendingUpdate, error) {
		pending = clonePendingUpdates(pending)
		pending[0].CVEScannedAt = at
		pending[0].CVEState = "ready"
		pending[0].CVEFindings = []VulnerabilityFinding{f}
		return pending, nil
	})
	var service *UpdateService
	deps.WaitForApprovalPollContext = func(context.Context) error {
		if _, ok := service.CancelPendingUpdate(server.Name); !ok {
			t.Fatal("pending cancellation rejected")
		}
		return nil
	}
	var audit map[string]any
	deps.AuditWithActor = func(_, _, action, _, _, _, _ string, meta map[string]any) {
		if action == "update.complete" {
			audit = meta
		}
	}
	service = NewUpdateService(deps)
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "watch-review", "", RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Actor: "watch-review", Policy: RetryPolicy{MaxAttempts: 1}})
	if audit["status"] != "idle" || audit["approval_scope"] != "none" || audit["upgrade_completed"] != false {
		t.Fatalf("runner audit=%+v", audit)
	}
	repo := vulnerabilities.Repository{DB: app.Deps.DB}
	observation, err := repo.Load(context.Background(), server.Name)
	if err != nil {
		t.Fatal(err)
	}
	if observation.Accepted != nil || observation.LastAttempt.State != "" || len(observation.Findings) != 1 || observation.Findings[0].PrioritySince != at {
		t.Fatalf("approval history changed installed coverage or missed watch: %+v", observation)
	}
	build := func(clock time.Time) observability.SecurityActions {
		t.Helper()
		summary, err := app.Deps.ObservabilityService.BuildDashboardSummaryContextWithOptions(context.Background(), "7d", clock, observability.DashboardSummaryOptions{OmitCommandHistory: true})
		if err != nil {
			t.Fatal(err)
		}
		return observability.BuildSecurityActions(summary, securityintel.Snapshot{}, observability.SecurityActionQuery{}, clock)
	}
	if initial := build(now); initial.AlertTotal != 1 {
		t.Fatalf("approval-only alert was not displayed: %+v", initial)
	}
	if stale := build(now.Add(48 * time.Hour)); stale.AlertTotal != 0 || stale.ReviewPriority != 1 {
		t.Fatalf("unresolved alert lost its priority on expiry: %+v", stale)
	}
	later := now.Add(60 * 24 * time.Hour)
	next := observation.Findings[0]
	next.LastSeen = later.Format(time.RFC3339)
	next.SeverityObservedAt = next.LastSeen
	next.Assessment.ObservedAt = next.LastSeen
	next.Availability.Published.ObservedAt = next.LastSeen
	if err := repo.RecordPositiveFindings(context.Background(), server.Name, []vulnerabilities.Finding{next}); err != nil {
		t.Fatal(err)
	}
	if refreshed := build(later); refreshed.AlertTotal != 1 {
		t.Fatalf("refreshed watch aged out after disclosure window: %+v", refreshed)
	}
}
