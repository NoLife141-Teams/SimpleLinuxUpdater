package updates

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/deferrals"
	"debian-updater/internal/jobs"
	"debian-updater/internal/servers"
)

func testDeferralSnapshot(now time.Time) deferrals.Snapshot {
	s, err := deferrals.Resolve([]deferrals.Rule{{ID: "rule", ServerName: "srv", Selector: "deferred:amd64", Reason: "Compatibility investigation", Author: "operator", CreatedAt: now.Add(-time.Hour).Format(time.RFC3339), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339)}}, "srv", nil, now)
	if err != nil {
		panic(err)
	}
	return s
}

func deferralFixtureOutcome() PackageDiscoveryOutcome {
	return newPackageDiscoveryOutcome([]servers.PendingUpdate{
		{Package: "deferred:amd64", CurrentVersion: "1", CandidateVersion: "2", Security: true, CVEState: "ready", CVEs: []string{"CVE-2026-1000"}},
		{Package: "independent:amd64", CurrentVersion: "1", CandidateVersion: "2", Security: true},
		{Package: "kept:amd64", InstallPackage: "kept:amd64", CurrentVersion: "1", CandidateVersion: "2", Security: true, KeptBack: true, RequiresFull: true},
	}, []string{"deferred", "independent", "kept"}, servers.UpgradePlan{FullUpgradePlanAvailable: true, KeptBackSecurityPlanAvailable: true, StandardPackageCount: 2, StandardSecurityCount: 2, TotalSecurityCount: 3, KeptBackPackageCount: 1})
}

func testDeferralSimulation(req HostCommandRequest) string {
	var output string
	if req.Operation != "update.deferral_plan.security_kept_back" {
		output += "Inst independent:amd64 [1] (2 fixture [amd64])\n"
	}
	if req.Operation == "update.deferral_plan.security_kept_back" || req.Operation == "update.deferral_plan.full_upgrade" {
		output += "Inst kept:amd64 [1] (2 fixture [amd64])\n"
	}
	return output
}

func TestPackageDeferralPlansRetainCVEAndExactExecutableScopes(t *testing.T) {
	now := time.Now().UTC()
	snapshot := testDeferralSnapshot(now)
	outcome := deferralFixtureOutcome()
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "independent transaction", true: "dependency conflict"}[conflict], func(t *testing.T) {
			session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Effect != HostCommandEffectReadOnly || strings.Contains(req.Command, "'deferred:amd64'") {
					t.Fatal(req)
				}
				output := testDeferralSimulation(req)
				if conflict {
					output += "Inst deferred:amd64 [1] (2 fixture [amd64])\n"
				}
				return HostCommandResult{Stdout: output}, nil
			}}
			got, err := PlanPackageDeferrals(context.Background(), session, outcome, snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.PendingUpdates) != 3 || !got.PendingUpdates[0].Deferred || len(got.PendingUpdates[0].CVEs) != 1 || got.SecurityPackageCount != 3 || len(got.UpgradePlan.DeferredSecurityPackages) != 1 {
				t.Fatal(got)
			}
			if outcome.PendingUpdates[0].Deferred {
				t.Fatal("mutated original discovery")
			}
			for _, scope := range []string{ApprovalScopeAll, ApprovalScopeSecurity, ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade} {
				p := got.UpgradePlan.DeferralScopes[scope]
				if p.Available == conflict {
					t.Fatal(scope, p)
				}
				interpreted := InterpretApprovedScope(scope, got.PendingUpdates, got.UpgradePlan, ApprovalScopeOptions{})
				if interpreted.Allowed == conflict {
					t.Fatal(scope, interpreted)
				}
				if !conflict && (!strings.Contains(interpreted.Command, "guarded-install") || strings.Contains(interpreted.Command, "apt-get")) {
					t.Fatal(interpreted.Command)
				}
				if conflict && (!strings.Contains(p.BlockReason, "Dependency conflict") || len(p.DependencyImpacts) != 1) {
					t.Fatal(p)
				}
			}
			clone := got.Clone()
			clone.UpgradePlan.DeferralScopes[ApprovalScopeAll].Transaction[0] = "changed"
			if reflect.DeepEqual(clone.UpgradePlan, got.UpgradePlan) {
				t.Fatal("shared plan transaction")
			}
		})
	}
}

func TestDeferralPlanFailsClosedOnSimulationAndIdentityErrors(t *testing.T) {
	now := time.Now().UTC()
	for _, tc := range []struct {
		name, output string
		err          error
	}{{"APT failure", "", errors.New("unmet dependency")}, {"unknown architecture", "Inst independent [1] (2 fixture)\n", nil}, {"invalid removal", "Remv independent\n", nil}, {"missing removal arch", "Remv independent [1]\n", nil}, {"invalid version", "Inst independent [1] (2! fixture [amd64])\n", nil}} {
		t.Run(tc.name, func(t *testing.T) {
			session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				return HostCommandResult{Stdout: tc.output}, tc.err
			}}
			got, err := PlanPackageDeferrals(context.Background(), session, deferralFixtureOutcome(), testDeferralSnapshot(now))
			if err != nil {
				t.Fatal(err)
			}
			for _, p := range got.UpgradePlan.DeferralScopes {
				if p.Available || p.BlockReason == "" {
					t.Fatal(p)
				}
			}
		})
	}
	changes, added, removed, err := parseDeferralTransaction("SLU_INSTALLED_ARCH old amd64\nInst new (2 fixture [amd64])\nRemv old [1]\n")
	if err != nil || !reflect.DeepEqual(changes, []string{"new:amd64=2", "old:amd64=-"}) || len(added) != 1 || len(removed) != 1 {
		t.Fatal(changes, added, removed, err)
	}
	out := deferralFixtureOutcome()
	out.PendingUpdates[1].Package = "--bad"
	session := &HostMaintenanceSessionFuncs{}
	if _, err = PlanPackageDeferrals(context.Background(), session, out, testDeferralSnapshot(now)); err == nil {
		t.Fatal("accepted invalid pending selector")
	}
}

func TestRunUpdatePackageDeferralsAllScopesAndScheduledAudit(t *testing.T) {
	for _, scope := range []string{ApprovalScopeAll, ApprovalScopeSecurity, ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade} {
		for _, automatic := range []bool{false, true} {
			if automatic && scope == ApprovalScopeSecurityKeptBack {
				continue
			}
			t.Run(scope+map[bool]string{false: " manual", true: " scheduled"}[automatic], func(t *testing.T) {
				now := time.Now().UTC()
				server := servers.Server{Name: "srv", User: "root"}
				inventory := []servers.Server{server}
				statuses := map[string]*servers.ServerStatus{"srv": {Name: "srv", Status: "idle"}}
				state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
				repository := newPendingApprovalTestRepository(t)
				manager := jobs.NewManager(repository, jobs.ManagerOptions{})
				metadata := "{}"
				if automatic {
					metadata = jobs.MarshalJSON(ScheduledJobMeta{Trigger: "scheduled", PolicyID: 7, PolicyRevision: 3, ExecutionMode: "auto_apply", AutoApproveScope: scope})
				}
				job, createErr := manager.CreateJob(jobs.CreateParams{Kind: jobs.KindUpdate, ServerName: server.Name, Status: jobs.StatusRunning, MetaJSON: metadata})
				if createErr != nil {
					t.Fatal(createErr)
				}
				statuses[server.Name].JobID = job.ID
				mutation := 0
				var meta map[string]any
				var service *Service
				session := &HostMaintenanceSessionFuncs{RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} }, DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
					return HostPackageDiscoveryResult{Outcome: deferralFixtureOutcome(), Attempts: 1}, nil
				}, RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
					if req.Effect == HostCommandEffectReadOnly {
						return HostCommandResult{Stdout: testDeferralSimulation(req)}, nil
					}
					if req.Effect == HostCommandEffectPackageStateMutation {
						mutation++
						if !strings.Contains(req.Command, "guarded-install") || !strings.Contains(req.Command, "'deferred:amd64'") {
							t.Fatal(req)
						}
					}
					return HostCommandResult{Attempts: 1}, nil
				}}
				service = NewService(ServiceDeps{ServerState: state, HostMaintenanceSessions: testHostMaintenanceSessionFactory(session), CurrentJobManager: func() *jobs.Manager { return manager }, StartJobRunner: func(string, func()) {}, LoadPackageDeferrals: func(servers.Server, string) (deferrals.Snapshot, error) { return testDeferralSnapshot(now), nil }, PackageDeferralGate: &sync.RWMutex{}, Now: func() time.Time { return now }, LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{} }, LoadScheduledJobBehavior: func(string) ScheduledJobBehavior {
					auto := ""
					if automatic {
						auto = scope
					}
					return ScheduledJobBehavior{ApprovalTimeout: time.Minute, AutoApproveScope: auto}
				}, WaitForApprovalPoll: func() {
					pending, pendingErr := manager.GetJob(job.ID)
					if pendingErr != nil || !strings.Contains(pending.MetaJSON, testDeferralSnapshot(now).Revision) {
						t.Fatal(pending, pendingErr)
					}
					if _, decisionErr := manager.ResolvePendingApproval(job.ID, pending.Revision, false, "Approved"); decisionErr != nil {
						t.Fatal(decisionErr)
					}
					service.ApprovePendingUpdate("srv", scope)
				}, SaveServerFacts: func(ServerFactsRecord) error { return nil }, UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {}, AuditWithActor: func(_, _, action, _, _, _, _ string, m map[string]any) {
					if action == UpdateCompleteAction {
						meta = m
					}
				}})
				service.RunUpdateJob(UpdateRunRequest{Server: server, Actor: "operator", JobID: job.ID, Policy: RetryPolicy{MaxAttempts: 1}})
				if status := state.CurrentStatusSnapshot("srv"); status.Status != "done" || mutation != 1 {
					t.Fatal(status, mutation)
				}
				saved, savedErr := manager.GetJob(job.ID)
				if savedErr != nil || saved.Status != jobs.StatusSucceeded {
					t.Fatal(saved, savedErr)
				}
				var persisted struct {
					UpgradePlan      servers.UpgradePlan `json:"upgrade_plan"`
					DeferralRevision string              `json:"deferral_revision"`
					PolicyRevision   int64               `json:"policy_revision"`
				}
				if savedErr = json.Unmarshal([]byte(saved.MetaJSON), &persisted); savedErr != nil || persisted.DeferralRevision != testDeferralSnapshot(now).Revision || len(persisted.UpgradePlan.DeferralScopes) != 4 || automatic && persisted.PolicyRevision != 3 {
					t.Fatal(saved.MetaJSON, savedErr)
				}
				if meta["deferral_revision"] == "" || meta["approval_scope"] != scope || meta["upgrade_completed"] != true || len(meta["deferred_packages"].([]string)) != 1 {
					t.Fatal(meta)
				}
			})
		}
	}
}

func TestPendingApprovalDeferralEditAndExpirationRequireAnotherGeneration(t *testing.T) {
	for _, mode := range []string{"changed reason", "expired", "cancelled during drift"} {
		expire := mode == "expired"
		t.Run(mode, func(t *testing.T) {
			now := time.Now().UTC()
			ruleTime := now
			rules := testDeferralSnapshot(now)
			server := servers.Server{Name: "srv", User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{"srv": {Name: "srv", Status: "idle"}}
			state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
			polls := 0
			mutations := 0
			var generations []uint64
			var service *Service
			session := &HostMaintenanceSessionFuncs{RunUpdatePrechecksFunc: func(context.Context) PrecheckSummary { return PrecheckSummary{AllPassed: true} }, DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
				return HostPackageDiscoveryResult{Outcome: deferralFixtureOutcome()}, nil
			}, RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
				if req.Effect == HostCommandEffectReadOnly {
					return HostCommandResult{Stdout: testDeferralSimulation(req)}, nil
				}
				if req.Effect == HostCommandEffectPackageStateMutation {
					mutations++
				}
				return HostCommandResult{}, nil
			}}
			service = NewService(ServiceDeps{ServerState: state, HostMaintenanceSessions: testHostMaintenanceSessionFactory(session), CurrentJobManager: func() *jobs.Manager { return nil }, StartJobRunner: func(string, func()) {}, Now: func() time.Time { return now }, LoadScheduledJobBehavior: func(string) ScheduledJobBehavior { return ScheduledJobBehavior{ApprovalTimeout: 4 * time.Hour} }, LoadPackageDeferrals: func(servers.Server, string) (deferrals.Snapshot, error) { return rules, nil }, LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{} }, WaitForApprovalPoll: func() {
				polls++
				generations = append(generations, state.CurrentStatusSnapshot("srv").ApprovalGeneration)
				if polls == 1 {
					if expire {
						now = now.Add(2 * time.Hour)
						rules, _ = deferrals.Resolve(rules.Rules, "srv", nil, now)
					} else {
						rules.Rules[0].Reason = "Changed investigation"
						rules, _ = deferrals.Resolve(rules.Rules, "srv", nil, ruleTime)
					}
					if mode == "cancelled during drift" {
						service.CancelPendingUpdate("srv")
					}
					return
				}
				service.ApprovePendingUpdate("srv", ApprovalScopeAll)
			}, SaveServerFacts: func(ServerFactsRecord) error { return nil }, UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {}, AuditWithActor: func(string, string, string, string, string, string, string, map[string]any) {}})
			service.RunUpdateJob(UpdateRunRequest{Server: server, Policy: RetryPolicy{MaxAttempts: 1}})
			if mode == "cancelled during drift" {
				if mutations != 0 || polls != 1 || state.CurrentStatusSnapshot("srv").Status != "idle" {
					t.Fatal(mutations, polls, state.CurrentStatusSnapshot("srv"))
				}
				return
			}
			if mutations != 1 || polls != 2 || generations[1] <= generations[0] {
				t.Fatal(mutations, polls, generations, state.CurrentStatusSnapshot("srv"))
			}
		})
	}
}

func TestDeferralRemovalConfirmationAndFinalMutationGate(t *testing.T) {
	now := time.Now().UTC()
	session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
		return HostCommandResult{Stdout: "SLU_INSTALLED_ARCH obsolete amd64\n" + testDeferralSimulation(req) + "Remv obsolete [1]\n"}, nil
	}}
	out, err := PlanPackageDeferrals(context.Background(), session, deferralFixtureOutcome(), testDeferralSnapshot(now))
	if err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{ApprovalScopeSecurityKeptBack, ApprovalScopeFullUpgrade} {
		status := &servers.ServerStatus{Status: "pending_approval", PendingUpdates: out.PendingUpdates, UpgradePlan: out.UpgradePlan}
		if EvaluateManualApproval(status, scope, ApprovalScopeOptions{}).Allowed || InterpretApprovedScope(scope, out.PendingUpdates, out.UpgradePlan, ApprovalScopeOptions{}).Allowed {
			t.Fatal("unconfirmed removal accepted", scope)
		}
		if got := InterpretApprovedScope(scope, out.PendingUpdates, out.UpgradePlan, ApprovalScopeOptions{ConfirmRemovals: true}); !got.Allowed || !strings.Contains(got.Command, "'allow-removals'") {
			t.Fatal(got)
		}
	}
	changed := false
	gate := &sync.RWMutex{}
	r := &withActorRunner{server: servers.Server{Name: "srv"}, upgradePlan: out.UpgradePlan, service: NewService(ServiceDeps{PackageDeferralGate: gate, LoadPackageDeferrals: func(servers.Server, string) (deferrals.Snapshot, error) {
		if changed {
			return deferrals.Snapshot{}, nil
		}
		return testDeferralSnapshot(now), nil
	}})}
	release, err := r.guardPackageDeferrals()
	if err != nil {
		t.Fatal(err)
	}
	if gate.TryLock() {
		t.Fatal("mutation did not exclude concurrent edits")
	}
	release()
	release()
	if !gate.TryLock() {
		t.Fatal("release leaked lock")
	}
	gate.Unlock()
	changed = true
	if _, err = r.guardPackageDeferrals(); err == nil {
		t.Fatal("accepted last-moment drift")
	}
}

func TestPackageDeferralsBlockUnplannedRepairAndAutoremove(t *testing.T) {
	for _, kind := range []string{"repair", "autoremove"} {
		t.Run(kind, func(t *testing.T) {
			state, statuses := testState()
			statuses["srv"].Status = "idle"
			commands := 0
			var meta map[string]any
			now := time.Now().UTC()
			service := NewService(ServiceDeps{ServerState: state, CurrentJobManager: func() *jobs.Manager { return nil }, HostMaintenanceSessions: testHostMaintenanceSessionFactory(&HostMaintenanceSessionFuncs{RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				commands++
				return HostCommandResult{}, nil
			}}), LoadPackageDeferrals: func(servers.Server, string) (deferrals.Snapshot, error) { return testDeferralSnapshot(now), nil }, PackageDeferralGate: &sync.RWMutex{}, AuditWithActor: func(_, _, _, _, _, _, _ string, m map[string]any) { meta = m }})
			if kind == "repair" {
				service.RunAptRepairJob(AptRepairRunRequest{Server: servers.Server{Name: "srv"}})
			} else {
				service.RunAutoremoveJob(AutoremoveRunRequest{Server: servers.Server{Name: "srv"}})
			}
			if commands != 0 || state.CurrentStatusSnapshot("srv").Status != "error" || meta["deferral_decision"] != "blocked" {
				t.Fatal(commands, statuses["srv"], meta)
			}
		})
	}
}
