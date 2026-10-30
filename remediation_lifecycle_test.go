package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"debian-updater/internal/deferrals"
	"debian-updater/internal/remediation"
	serverpkg "debian-updater/internal/servers"
	updatespkg "debian-updater/internal/updates"
	"debian-updater/internal/vulnerabilities"
)

type remediationTestScanner struct {
	version *atomic.Int32
	fail    *atomic.Bool
	partial bool
	now     func() time.Time
}

func (s remediationTestScanner) Scan(_ context.Context, _ HostMaintenanceSession, p []PendingUpdate) ([]PendingUpdate, error) {
	return p, nil
}
func (s remediationTestScanner) ScanInstalled(ctx context.Context, _ HostMaintenanceSession) (vulnerabilities.Scan, error) {
	if s.fail != nil && s.fail.Load() {
		return vulnerabilities.Scan{}, errors.New("synthetic scan failure")
	}
	name, id := vulnerabilities.ScanIdentity(ctx)
	at := s.now().UTC().Format(time.RFC3339Nano)
	version := fmt.Sprint(s.version.Load())
	ids := []string{"CVE-2026-1001"}
	if version != "1" {
		ids = nil
	}
	scan := vulnerabilities.Scan{ID: id, ServerName: name, Scope: "installed_packages", State: "complete", StartedAt: at, FinishedAt: at, Inventoried: 2, Evaluated: 2, Distro: "debian 12"}
	for _, pkg := range []string{"addressable", "residual"} {
		packageIDs := ids
		if pkg == "residual" {
			packageIDs = []string{"CVE-2026-1002"}
		}
		v := version
		if pkg == "residual" {
			v = "1"
		}
		p := vulnerabilities.Package{Name: pkg, Version: v, SourcePackage: "source-" + pkg, SourceVersion: v, State: "evaluated"}
		p.Proof = &vulnerabilities.EvaluationProof{ScanID: id, ServerName: name, Package: pkg, InstalledVersion: v, SourcePackage: p.SourcePackage, SourceVersion: v, Ecosystem: "Debian:12", Source: "https://synthetic.invalid", ObservedAt: at, VerifiedAt: at, Stable: true, Complete: true, ProvenanceVerified: true, FindingIDs: packageIDs}
		if s.partial && pkg == "residual" {
			p.Proof = nil
			p.State = "unknown"
			scan.State = "partial"
			scan.Unknown = 1
			scan.Evaluated = 1
			packageIDs = nil
		}
		scan.Packages = append(scan.Packages, p)
		for _, cve := range packageIDs {
			scan.Findings = append(scan.Findings, vulnerabilities.Finding{ID: cve, Package: pkg, SourcePackage: p.SourcePackage, InstalledVersion: v, CandidateVersion: "2", Disposition: "fixed_by_candidate", Availability: &vulnerabilities.FixAvailability{CandidateState: "addresses", CandidateBinaryVersion: "2", CandidateSourcePackage: p.SourcePackage, CandidateSourceVersion: "2", QueryObservedAt: at, Scopes: []vulnerabilities.ScopeInstallability{{Scope: "all", State: "simulated"}}}, Assessment: vulnerabilities.Assessment{Ecosystem: "Debian:12", SourcePackage: p.SourcePackage, SourceVersion: v, Source: "https://synthetic.invalid", ObservedAt: at, VendorStatus: "affected", Confidence: "vendor_confirmed", Applicability: "source_match"}})
		}
	}
	for i := range scan.Findings {
		f := &scan.Findings[i]
		if f.Package == "residual" {
			f.Disposition = "still_affected"
			f.Availability.CandidateState = "affected"
			f.Availability.Scopes = []vulnerabilities.ScopeInstallability{{Scope: "all", State: "not_selected"}}
		}
	}
	scan.InventoryProof = &vulnerabilities.InventoryProof{ScanID: id, ServerName: name, ObservedAt: at, VerifiedAt: at, Stable: true, Complete: true, Packages: []string{"addressable", "residual"}}
	return scan, nil
}
func TestRemediationRunnerPreparationApprovalHealthCorrelatedScanAndDrift(t *testing.T) {
	for _, mode := range []string{"success", "late_candidate_drift", "postcheck_failed", "scan_failed", "partial", "security_scope", "kept_back_scope", "full_scope", "cancelled", "configuration_drift"} {
		t.Run(mode, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "example-host", Host: "example-host.invalid", User: "root", Pass: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			var version atomic.Int32
			version.Store(1)
			var fail atomic.Bool
			scanner := &remediationTestScanner{version: &version, fail: &fail, now: app.Deps.Now}
			scope := "all"
			switch mode {
			case "security_scope":
				scope = "security"
			case "kept_back_scope":
				scope = "security_kept_back"
			case "full_scope":
				scope = "full_upgrade"
			}
			discoveries, mutations, approvals := 0, 0, 0
			var auditMeta, verificationMeta map[string]any
			serviceDeps := UpdateServiceDeps{ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager, Now: app.Deps.Now, VulnerabilityScanner: scanner,
				HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						discoveries++
						candidate := "2"
						if mode == "late_candidate_drift" && discoveries > 1 {
							candidate = "3"
						}
						return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"addressable"}, PendingUpdates: []PendingUpdate{{Package: "addressable", CurrentVersion: "1", CandidateVersion: candidate, Source: "stable", CVEState: "done", Security: scope == "security" || scope == "security_kept_back", KeptBack: scope == "security_kept_back", RequiresFull: scope == "security_kept_back"}}, UpgradePlan: UpgradePlan{StandardPackageCount: 1, FullUpgradePlanAvailable: true, KeptBackSecurityPlanAvailable: true, FullUpgradeNewPackages: []string{"new-dependency"}, FullUpgradeRemovedPackages: []string{"old-runtime"}, KeptBackSecurityNewPackages: []string{"new-dependency"}, KeptBackSecurityRemovedPackages: []string{"old-runtime"}}}}, nil
					},
					RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
						if r.Operation == "remediation.installed_inventory" {
							return HostCommandResult{Stdout: fmt.Sprintf("__SLU_OS__\tdebian\t12\naddressable\t%d\tsource-addressable\t%d\nresidual\t1\tsource-residual\t1\n__SLU_INVENTORY_END__\n", version.Load(), version.Load()), Attempts: 1}, nil
						}
						if r.Effect == updatespkg.HostCommandEffectPackageStateMutation {
							mutations++
							version.Store(2)
						}
						return HostCommandResult{Attempts: 1}, nil
					},
					RunPostUpdateHealthChecksFunc: func(context.Context, PostUpdateCheckConfig, map[string]struct{}) updatePostcheckSummary {
						if mode == "postcheck_failed" {
							return updatePostcheckSummary{AllPassed: false, FailedCheck: "post_apt_health", Results: []updatePrecheckResult{{Name: "post_apt_health", Passed: false, Details: "synthetic failed check"}}}
						}
						return updatePostcheckSummary{AllPassed: true}
					},
				}), LoadScheduledJobBehavior: func(string) scheduledJobBehavior {
					return scheduledJobBehavior{AutoApproveScope: "all", ApprovalTimeout: time.Hour}
				},
				LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: true, BlockOnAptHealth: true} }, SaveServerFacts: func(serverFactsRecord) error { return nil },
				AuditWithActor: func(_, _, action, _, _, _, _ string, meta map[string]any) {
					if action == updatespkg.UpdateCompleteAction {
						auditMeta = meta
					}
					if action == "remediation.verify" {
						verificationMeta = meta
					}
				},
			}
			serviceDeps.WaitForApprovalPollContext = func(context.Context) error {
				approvals++
				if approvals > 3 {
					t.Fatal("approval loop did not converge")
				}
				if mutations != 0 {
					t.Fatal("preparation mutated packages before explicit approval")
				}
				status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name)
				if status.RemediationDigest == "" || status.RemediationPlanID == "" {
					t.Fatal("missing plan identity")
				}
				lifecycle := newServerActionLifecycle(app.Deps, nil)
				identity := serverActionApprovalIdentity{JobID: status.JobID, Generation: status.ApprovalGeneration, DeferralRevision: status.UpgradePlan.PackageDeferrals.Revision}
				if result := lifecycle.ApproveAll(server.Name, identity); result.statusCode != 409 {
					t.Fatalf("legacy endpoint bypassed identity: %+v", result)
				}
				identity.RemediationDigest = status.RemediationDigest
				if mode == "cancelled" {
					if result := lifecycle.Cancel(server.Name, identity); result.statusCode != 200 {
						t.Fatal(result)
					}
					return nil
				}
				if scope == "full_upgrade" || scope == "security_kept_back" {
					if result := lifecycle.approve(server.Name, identity, scope, false); result.statusCode != 409 {
						t.Fatal("unconfirmed removals accepted", result)
					}
				}
				if result := lifecycle.approve(server.Name, identity, scope, true); result.statusCode != 200 {
					t.Fatalf("valid approval: %+v", result)
				}
				if mode == "configuration_drift" {
					app.Deps.ServerState.Lock()
					changed := server
					changed.Host = "changed-host.invalid"
					app.Deps.ServerState.SetServers([]Server{changed})
					app.Deps.ServerState.Unlock()
				}
				return nil
			}
			service := NewUpdateService(serviceDeps)
			app.Deps.UpdateService = service
			app.Deps.Remediation.deps.UpdateService = service
			app.Deps.VulnerabilityScans.deps.UpdateService = service
			beforeID, run, err := app.Deps.VulnerabilityScans.AdmitScan(t.Context(), server.Name, "operator", "")
			if err != nil {
				t.Fatal(err)
			}
			run()
			job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "operator", "", RetryPolicy{MaxAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			service.RunUpdateJob(app.Deps.Remediation.request(server, "operator", "", job.ID, RetryPolicy{MaxAttempts: 1}))
			wantMutations := 1
			if mode == "cancelled" || mode == "configuration_drift" {
				wantMutations = 0
			}
			if mutations != wantMutations || approvals < 1 {
				t.Fatalf("mutations=%d approvals=%d", mutations, approvals)
			}
			if mode == "late_candidate_drift" && approvals != 2 {
				t.Fatalf("AutoApprove bypassed renewed approval: %d", approvals)
			}
			if auditMeta["remediation_plan_id"] == "" || auditMeta["remediation_digest"] == "" || auditMeta["before_scan_id"] != beforeID {
				t.Fatal(auditMeta)
			}
			if mode == "scan_failed" {
				fail.Store(true)
			}
			if mode == "partial" {
				scanner.partial = true
			}
			app.Deps.Remediation.deps.StartJobRunner = func(_ string, run func(), _ ...func()) { run() }
			if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			plans, err := app.Deps.Remediation.repo().List(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			p := plans[0]
			if mode == "configuration_drift" {
				if p.State != "queued" || p.ScanID != "" || p.Completion["upgrade_completed"] != false {
					t.Fatal("configuration drift bypassed safety")
				}
				app.Deps.ServerState.Lock()
				app.Deps.ServerState.SetServers([]Server{server})
				app.Deps.ServerState.Unlock()
				if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				p, _ = app.Deps.Remediation.repo().Load(t.Context(), p.ID)
			}
			if p.ScanID == "" || p.ScanID == beforeID {
				t.Fatal("verification reused scan", p)
			}
			scanJob, err := app.Deps.CurrentJobManager().GetJob(p.ScanID)
			if err != nil || scanJob.ParentJobID != job.ID {
				t.Fatalf("missing scan correlation %+v %v", scanJob, err)
			}
			if mode == "scan_failed" {
				if p.State != "verification_failed" || p.Comparison != nil {
					t.Fatal(p)
				}
				fail.Store(false)
				p.State = "queued"
				if err = app.Deps.Remediation.repo().Save(t.Context(), p, "verification_failed"); err != nil {
					t.Fatal(err)
				}
				if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
					t.Fatal(err)
				}
				p, _ = app.Deps.Remediation.repo().Load(t.Context(), p.ID)
				if mutations != 1 {
					t.Fatal("verification replayed update")
				}
			}
			expectedState := "verified"
			if mode == "partial" {
				expectedState = "partially_verified"
			}
			if p.State != expectedState || p.Comparison.Counts["corrected"] != wantMutations {
				t.Fatalf("%+v", p)
			}
			if mode == "partial" && p.Comparison.Counts["not_reverified"] != 1 {
				t.Fatal(p.Comparison)
			}
			if verificationMeta["after_scan_id"] != p.ScanID || verificationMeta["update_job_id"] != job.ID || verificationMeta["remediation_plan_id"] != p.ID {
				t.Fatal("missing verification audit correlation", verificationMeta)
			}
			if mode == "postcheck_failed" && p.Completion["postchecks_passed"] != false {
				t.Fatal("health failure hidden", p.Completion)
			}
		})
	}
}

func TestRemediationRolesReviewAndReadRoutes(t *testing.T) {
	app := newIsolatedTestApp(t)
	app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("remediation-reader", testPasswordStrong, "reader"); err != nil {
		t.Fatal(err)
	}
	reader := identityLogin(t, app, "remediation-reader")
	for _, path := range []string{"/api/servers/example-host/remediation/prepare", "/api/servers/example-host/remediation/plan/retry", "/api/servers/example-host/vulnerabilities/reviews"} {
		if rec := identityRequest(app, "POST", path, reader, "", map[string]any{}); rec.Code != 403 {
			t.Fatalf("%s %d %s", path, rec.Code, rec.Body.String())
		}
	}
	for _, path := range []string{"/vulnerabilities", "/remediation", "/api/vulnerabilities", "/api/vulnerabilities/plan"} {
		if rec := identityRequest(app, "GET", path, reader, "", nil); rec.Code != 200 {
			t.Fatalf("%s %d %s", path, rec.Code, rec.Body.String())
		}
	}
}

func TestRemediationRecoveryBusyRenameAndDeletedReplacement(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "recovery-host", Host: "recovery-host.invalid", User: "root", Pass: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	var version atomic.Int32
	version.Store(2)
	scanner := &remediationTestScanner{version: &version, now: app.Deps.Now}
	service := NewUpdateService(UpdateServiceDeps{Now: app.Deps.Now, VulnerabilityScanner: scanner, HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{})})
	app.Deps.VulnerabilityScans.deps.UpdateService = service
	job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{Kind: jobKindUpdate, ServerName: server.Name, Status: jobStatusInterrupted})
	if err != nil {
		t.Fatal(err)
	}
	repo := app.Deps.Remediation.repo()
	key, err := repo.ServerKey(t.Context(), server.Name)
	if err != nil {
		t.Fatal(err)
	}
	p := remediation.Plan{ID: remediation.ID(), TargetIdentity: remediationTarget(server), ServerKey: key, Server: server.Name, JobID: job.ID, State: "prepared", Actor: "operator"}
	if err = repo.Insert(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	// A fresh app-scoped worker recovers only the verification request.
	recovered := newRemediationLifecycle(app.Deps)
	recovered.deps.StartJobRunner = func(_ string, run func(), _ ...func()) { run() }
	_, previous, err := app.Deps.ServerState.BeginTransientAction(server.Name, "facts_refresh")
	if err != nil {
		t.Fatal(err)
	}
	if err = recovered.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Load(t.Context(), p.ID)
	if p.State != "queued" || p.ScanID != "" {
		t.Fatalf("busy request lost %+v", p)
	}
	app.Deps.ServerState.RestoreStatusSnapshot(server.Name, previous)
	composition := newRuntimeComposition(app.Deps)
	composition.resetCaches = func() {}
	if err = composition.ReloadRestoredState(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Load(t.Context(), p.ID)
	if p.State != "queued" {
		t.Fatal("restore lost verification", p)
	}
	changed := server
	changed.Host = "replacement-target.invalid"
	if _, err = app.Deps.ServerInventoryService.Update(server.Name, changed); err != nil {
		t.Fatal(err)
	}
	if err = recovered.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Load(t.Context(), p.ID)
	if p.ScanID != "" {
		t.Fatal("verification attached to changed target", p)
	}
	if _, err = app.Deps.ServerInventoryService.Update(server.Name, server); err != nil {
		t.Fatal(err)
	}
	renamed := server
	renamed.Name = "renamed-host"
	if _, err = app.Deps.ServerInventoryService.Update(server.Name, renamed); err != nil {
		t.Fatal(err)
	}
	if err = recovered.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, _ = repo.Load(t.Context(), p.ID)
	if p.State != "verified" || p.VerificationServer != "renamed-host" {
		t.Fatalf("rename lost request %+v", p)
	}
	if err = app.Deps.ServerInventoryService.Delete("renamed-host"); err != nil {
		t.Fatal(err)
	}
	if _, err = app.Deps.ServerInventoryService.Create(renamed); err != nil {
		t.Fatal(err)
	}
	if _, err = repo.ServerName(t.Context(), key); err == nil {
		t.Fatal("old plan attached to replacement")
	}
}

// Disposable browser transport: atomic installed state, no SSH/APT/public CVE API.
func syntheticRemediationService(deps AppDeps, failVerification bool) *UpdateService {
	var version atomic.Int32
	version.Store(1)
	var failOnce atomic.Bool
	failOnce.Store(failVerification)
	scanner := remediationBrowserScanner{remediationTestScanner: remediationTestScanner{version: &version, now: deps.Now}, failOnce: &failOnce}
	base := deps.UpdateService.EnsureDeps()
	base.Now = deps.Now
	base.VulnerabilityScanner = scanner
	base.SaveServerFacts = func(serverFactsRecord) error { return nil }
	base.LoadScheduledJobBehavior = func(string) scheduledJobBehavior {
		return scheduledJobBehavior{AutoApproveScope: "all", ApprovalTimeout: time.Hour}
	}
	base.HostMaintenanceSessions = testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
		if r.Operation == "remediation.installed_inventory" {
			return HostCommandResult{Stdout: fmt.Sprintf("__SLU_OS__\tdebian\t12\naddressable\t%d\tsource-addressable\t%d\nresidual\t1\tsource-residual\t1\n__SLU_INVENTORY_END__\n", version.Load(), version.Load()), Attempts: 1}, nil
		}
		if r.Effect == updatespkg.HostCommandEffectPackageStateMutation {
			version.Store(2)
		}
		return HostCommandResult{Attempts: 1}, nil
	}, DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
		return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{PendingPackageCount: 2, Upgradable: []string{"addressable", "extra-package"}, PendingUpdates: []PendingUpdate{{Package: "addressable", CurrentVersion: "1", CandidateVersion: "2", Source: "stable", CVEState: "done"}, {Package: "extra-package", CurrentVersion: "1", CandidateVersion: "2", Source: "stable", CVEState: "done"}}, UpgradePlan: UpgradePlan{StandardPackageCount: 2, FullUpgradePlanAvailable: true, FullUpgradePackageCount: 2, FullUpgradeNewPackages: []string{"new-dependency"}, FullUpgradeRemovedPackages: []string{"old-runtime"}}}}, nil
	}})
	return NewUpdateService(base)
}

type remediationBrowserScanner struct {
	remediationTestScanner
	failOnce *atomic.Bool
}

func (s remediationBrowserScanner) ScanInstalled(ctx context.Context, session HostMaintenanceSession) (vulnerabilities.Scan, error) {
	if s.version.Load() > 1 && s.failOnce.CompareAndSwap(true, false) {
		return vulnerabilities.Scan{}, errors.New("synthetic verification unavailable")
	}
	return s.remediationTestScanner.ScanInstalled(ctx, session)
}

func TestRemediationScopesShowProtectedTransactionsAndPointReleaseBoundary(t *testing.T) {
	status := &ServerStatus{Status: "pending_approval", PendingUpdates: []PendingUpdate{{Package: "ordinary-package", CurrentVersion: "1", CandidateVersion: "2"}, {Package: "security-package", CurrentVersion: "1", CandidateVersion: "2", Security: true}, {Package: "deferred-package", Deferred: true, CurrentVersion: "1", CandidateVersion: "2"}}, UpgradePlan: UpgradePlan{PackageDeferrals: deferrals.Snapshot{Revision: "synthetic-revision", Rules: []deferrals.Rule{{Selector: "deferred-package", Reason: "Investigate compatibility"}}}, DeferralScopes: map[string]serverpkg.DeferralScopePlan{}, FullUpgradePlanAvailable: true, KeptBackSecurityPlanAvailable: true}}
	for _, scope := range []string{"all", "security", "security_kept_back", "full_upgrade"} {
		selected := []string{"ordinary-package", "security-package"}
		if scope == "security" || scope == "security_kept_back" {
			selected = []string{"security-package"}
		}
		status.UpgradePlan.DeferralScopes[scope] = serverpkg.DeferralScopePlan{Available: true, SelectedPackages: selected, Transaction: append(append([]string{}, selected...), "new-dependency"), NewPackages: []string{"new-dependency"}, RemovedPackages: []string{"old-runtime"}, DependencyImpacts: []string{"new-dependency"}}
	}
	tx := buildRemediationTransaction("example-host", status, vulnerabilities.Observation{}, time.Now())
	if len(tx.Scopes) != 4 {
		t.Fatal(tx)
	}
	for _, scope := range tx.Scopes {
		if strings.Contains(scope.Reason, "approved") {
			t.Fatal("preparation claimed approval before consent", scope)
		}
		if len(scope.Added) != 1 || len(scope.Removed) != 1 || len(scope.DependencyImpacts) != 1 || len(scope.Transaction) < 2 {
			t.Fatal("protected transaction facts hidden", scope)
		}
		for _, pkg := range scope.Selected {
			if pkg == "deferred-package" {
				t.Fatal("deferred package selected")
			}
		}
		if scope.Scope == "security" {
			for _, pkg := range scope.Selected {
				if pkg == "ordinary-package" {
					t.Fatal("point release represented as security-only")
				}
			}
		}
	}
}

func TestRemediationRecoveryDoesNotStealLiveCompletionHealth(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "completion-host", Host: "completion-host.invalid", User: "root", Pass: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{Kind: jobKindUpdate, ServerName: server.Name, Status: jobStatusSucceeded})
	if err != nil {
		t.Fatal(err)
	}
	repo := app.Deps.Remediation.repo()
	key, err := repo.ServerKey(t.Context(), server.Name)
	if err != nil {
		t.Fatal(err)
	}
	p := remediation.Plan{ID: remediation.ID(), TargetIdentity: remediationTarget(server), ServerKey: key, Server: server.Name, JobID: job.ID, State: "prepared"}
	if err = repo.Insert(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	app.Deps.ServerState.Lock()
	app.Deps.ServerState.StatusMap()[server.Name].JobID = job.ID
	app.Deps.ServerState.StatusMap()[server.Name].ActionRunning = true
	app.Deps.ServerState.Unlock()
	defer func() {
		app.Deps.ServerState.Lock()
		app.Deps.ServerState.StatusMap()[server.Name].ActionRunning = false
		app.Deps.ServerState.Unlock()
	}()
	if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err := repo.Load(t.Context(), p.ID)
	if err != nil || loaded.State != "prepared" {
		t.Fatal("recovery stole the live completion transition", loaded.State, err)
	}
	p.State = "queued"
	p.Completion = map[string]any{"upgrade_completed": true, "postchecks_enabled": true, "postchecks_passed": false}
	if err = repo.Save(t.Context(), p, "prepared"); err != nil {
		t.Fatal("live owner could not persist health", err)
	}
	if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
		t.Fatal(err)
	}
	loaded, err = repo.Load(t.Context(), p.ID)
	if err != nil || loaded.ScanID != "" || loaded.Completion["postchecks_passed"] != false {
		t.Fatal("verification ran before release or lost failed health", loaded.State, err)
	}
}

func TestRemediationVerificationRevalidatesReservedServerIdentity(t *testing.T) {
	for _, replace := range []bool{false, true} {
		t.Run(fmt.Sprintf("delete_recreate=%t", replace), func(t *testing.T) {
			app := newIsolatedTestApp(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "identity-host", Host: "identity-host.invalid", User: "root", Pass: "synthetic"})
			if err != nil {
				t.Fatal(err)
			}
			job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{Kind: jobKindUpdate, ServerName: server.Name, Status: jobStatusSucceeded})
			if err != nil {
				t.Fatal(err)
			}
			repo := app.Deps.Remediation.repo()
			key, err := repo.ServerKey(t.Context(), server.Name)
			if err != nil {
				t.Fatal(err)
			}
			p := remediation.Plan{ID: remediation.ID(), TargetIdentity: remediationTarget(server), ServerKey: key, Server: server.Name, JobID: job.ID, State: "queued", Actor: "operator"}
			if err = repo.Insert(t.Context(), p); err != nil {
				t.Fatal(err)
			}
			app.Deps.VulnerabilityScans.deps.UpdateService = NewUpdateService(UpdateServiceDeps{
				HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{}),
			})
			// Inventory mutations are legal until scan admission reserves the Server.
			// Change it after the worker's optimistic identity read, before reservation.
			app.Deps.VulnerabilityScans.deps.MaintenanceReadiness = func([]Server) map[string]serverpkg.MaintenanceReadiness {
				if replace {
					if err := app.Deps.ServerInventoryService.Delete(server.Name); err != nil {
						t.Fatal(err)
					}
					if _, err := app.Deps.ServerInventoryService.Create(server); err != nil {
						t.Fatal(err)
					}
				} else {
					changed := server
					changed.Host = "changed-target.invalid"
					if _, err := app.Deps.ServerInventoryService.Update(server.Name, changed); err != nil {
						t.Fatal(err)
					}
				}
				return map[string]serverpkg.MaintenanceReadiness{server.Name: {Ready: true}}
			}
			started := 0
			app.Deps.Remediation.deps.StartJobRunner = func(id string, run func(), _ ...func()) {
				started++
				// Drain without any transport if this regression admits the wrong target.
				_ = app.Deps.VulnerabilityScans.Cancel(server.Name, id)
				run()
			}
			if err = app.Deps.Remediation.Reconcile(t.Context()); err != nil {
				t.Fatal(err)
			}
			loaded, err := repo.Load(t.Context(), p.ID)
			if err != nil || loaded.State != "queued" || loaded.ScanID != "" || started != 0 {
				t.Fatalf("verification admitted a different Server identity: state=%s scan=%s started=%d err=%v", loaded.State, loaded.ScanID, started, err)
			}
			if got := len(app.Deps.VulnerabilityScans.slots); got != 0 {
				t.Fatalf("failed identity guard retained %d scan slots", got)
			}
			if status := app.Deps.ServerState.CurrentStatusSnapshot(server.Name); status.Status == "vulnerability_scan" {
				t.Fatal("failed identity guard retained Server reservation")
			}
		})
	}
}
