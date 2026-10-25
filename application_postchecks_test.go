package main

import (
	"bytes"
	"context"
	policypkg "debian-updater/internal/policies"
	scheduledrunspkg "debian-updater/internal/scheduledruns"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/postchecks"
	updatespkg "debian-updater/internal/updates"
)

func TestApplicationPostcheckAPIRevisionResolutionTestAndMaintenanceGate(t *testing.T) {
	state := newServerState()
	commands := 0
	closed := false
	service := NewUpdateService(UpdateServiceDeps{ServerState: state, HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
		RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
			commands++
			if r.Effect != updatespkg.HostCommandEffectReadOnly {
				t.Fatal("test attempted mutation")
			}
			return HostCommandResult{Stdout: "200"}, nil
		}, CloseFunc: func() error { closed = true; return nil },
	})})
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "profiles-api.db"), Deps: AppDeps{ServerState: state, UpdateService: service}})
	cookie := app.authenticate(t)
	state.Lock()
	state.SetServers([]Server{{Name: "web", Host: "example.invalid", Port: 22, User: "operator", Tags: []string{"web"}}})
	state.SetStatusMap(map[string]*ServerStatus{"web": {Name: "web", Status: "idle"}})
	state.Unlock()
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(method, path, bytes.NewReader(raw))
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	check := postchecks.Check{ID: "http", Name: "Health", Kind: "http_status", URL: "http://localhost/health", ExpectedStatus: 200, TimeoutSeconds: 1, IntervalSeconds: 1, Mode: "blocking"}
	config := postchecks.Config{Profiles: []postchecks.Profile{{ID: "web", Name: "Web", TargetTags: []string{"web"}, Checks: []postchecks.Check{check}}}}
	saved := request("PUT", "/api/postcheck-profiles", config)
	if saved.Code != 200 {
		t.Fatal(saved.Code, saved.Body.String())
	}
	if got := request("PUT", "/api/postcheck-profiles", config); got.Code != 409 {
		t.Fatal(got.Code, got.Body.String())
	}
	got := request("GET", "/api/servers/web/postchecks", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"revision":1`) {
		t.Fatal(got.Code, got.Body.String())
	}
	got = request("POST", "/api/servers/web/postchecks/test", nil)
	if got.Code != 200 || !strings.Contains(got.Body.String(), `"passed":true`) || commands != 1 || !closed {
		t.Fatal(got.Code, got.Body.String(), commands, closed)
	}
	for _, path := range []string{"/api/servers/absent/postchecks", "/api/servers/absent/postchecks/test"} {
		method := "GET"
		if strings.HasSuffix(path, "/test") {
			method = "POST"
		}
		if got = request(method, path, nil); got.Code != 404 {
			t.Fatal(got.Code)
		}
	}
	if err := json.Unmarshal(saved.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	config.Profiles[0].Checks[0].URL = "file:///etc/passwd"
	if got = request("PUT", "/api/postcheck-profiles", config); got.Code != 400 {
		t.Fatal(got.Code)
	}
	// Read-only tests hold the same request lease as configuration updates and cannot cross restore.
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("exclusive denied")
	}
	defer lease.Close()
	got = request("POST", "/api/servers/web/postchecks/test", nil)
	if got.Code != http.StatusServiceUnavailable || commands != 1 {
		t.Fatalf("maintenance gate %d commands %d", got.Code, commands)
	}
}

func TestApplicationPostcheckFailureControlsCanaryWave(t *testing.T) {
	for _, mode := range []string{"blocking", "warning"} {
		t.Run(mode, func(t *testing.T) {
			canary := Server{Name: "a-canary", Host: "canary.invalid", Port: 22, User: "root", Pass: "test", Tags: []string{"prod"}}
			deps, policy, _, jm := newScheduledRunLifecycleTestDeps(t, "postcheck-canary.db", canary, "idle")
			wave := Server{Name: "z-wave", Host: "wave.invalid", Port: 22, User: "root", Pass: "test", Tags: []string{"prod"}}
			deps.ServerState.Lock()
			deps.ServerState.SetServers([]Server{canary, wave})
			deps.ServerState.StatusMap()[wave.Name] = &ServerStatus{Name: wave.Name, Status: "idle"}
			deps.ServerState.Unlock()
			origin := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
			policy.Enabled = true
			policy.TargetTag = "prod"
			policy.ExecutionMode = policypkg.ExecutionAutoApply
			policy.PackageScope = policypkg.PackageScopeSecurity
			policy.CadenceKind = policypkg.CadenceDaily
			policy.TimeLocal = "03:00"
			policy.RolloutMode = policypkg.RolloutCanaryWaves
			policy.CanaryCount = 1
			policy.WaveSize = 1
			policy.WaveDelayMinutes = 1
			policy.CreatedAt = origin.Add(-time.Minute).Format(jobTimestampLayout)
			config := postchecks.Config{Profiles: []postchecks.Profile{{ID: "app", Name: "Web app", TargetTags: []string{"prod"}, Checks: []postchecks.Check{{ID: "web", Name: "Web", Kind: "http_status", URL: "http://localhost/health", ExpectedStatus: 200, Mode: mode, TimeoutSeconds: 1, IntervalSeconds: 1}}}}}
			if _, err := postchecks.Save(context.Background(), getDB(), config); err != nil {
				t.Fatal(err)
			}
			calls := 0
			deps.StartJobRunner = func(_ string, run func(), _ ...func()) { calls++; run() }
			deps.UpdateService = NewUpdateService(UpdateServiceDeps{
				ServerState: deps.ServerState, CurrentJobManager: func() *JobManager { return jm },
				HostMaintenanceSessions: testHostMaintenanceFactory(&HostMaintenanceSessionFuncs{
					DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
						return HostPackageDiscoveryResult{Outcome: PackageDiscoveryOutcome{PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{{Package: "openssl", Security: true, CVEState: "done"}}, UpgradePlan: UpgradePlan{StandardPackageCount: 1, StandardSecurityCount: 1}}, Attempts: 1}, nil
					},
					RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
						if strings.HasPrefix(r.Operation, "postcheck.application.") {
							return HostCommandResult{Stdout: "SLU_TOOL_MISSING"}, errors.New("missing curl")
						}
						return HostCommandResult{Attempts: 1}, nil
					},
				}),
				LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
				LoadScheduledJobBehavior: func(string) updatespkg.ScheduledJobBehavior {
					return updatespkg.ScheduledJobBehavior{AutoApproveScope: updatespkg.ApprovalScopeSecurity}
				},
				SaveServerFacts:              func(updatespkg.ServerFactsRecord) error { return nil },
				UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {},
				AuditWithActor: func(actor, ip, action, targetType, target, status, message string, meta map[string]any) {
					if err := deps.AuditService.Record(actor, ip, action, targetType, target, status, message, meta); err != nil {
						t.Error(err)
					}
				},
			})
			repo := defaultPolicyRepository()
			lifecycle := scheduledrunspkg.New(deps)
			scheduler := NewPolicyService(PolicyServiceDeps{ListPolicies: func() ([]UpdatePolicy, error) { return []UpdatePolicy{policy}, nil }, LoadOverrides: repo.LoadAllOverrides, LoadGlobalBlackouts: repo.LoadGlobalBlackouts, ListRolloutRuns: repo.ListRolloutRuns, ListRolloutOrigins: repo.ListRolloutOrigins, SnapshotServers: func() []Server { return []Server{canary, wave} }, CurrentLocation: func() *time.Location { return time.UTC }, HandleScheduledRun: lifecycle.HandleScheduledRun})
			if err := scheduler.ProcessDue(origin.Add(-time.Minute)); err != nil {
				t.Fatal(err)
			}
			if err := scheduler.ProcessDue(origin); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatalf("canary calls %d", calls)
			}
			run, err := repo.FindRun(policy.ID, canary.Name, origin.Format(jobTimestampLayout))
			if err != nil {
				t.Fatal(err)
			}
			expected := policypkg.RunFailed
			if mode == "warning" {
				expected = policypkg.RunSucceeded
			}
			if run.Status != expected {
				t.Fatalf("canary %+v", run)
			}
			job, err := jm.GetJob(run.JobID)
			if err != nil || !strings.Contains(job.MetaJSON, `"application_postcheck_results"`) {
				t.Fatalf("job %+v %v", job, err)
			}
			events, err := deps.AuditService.List(AuditListFilter{Action: updatespkg.UpdateCompleteAction, TargetName: canary.Name})
			if err != nil || events.Total != 1 || !strings.Contains(events.Items[0].MetaJSON, `"profile"`) && !strings.Contains(events.Items[0].MetaJSON, `"application_postcheck_profile"`) {
				t.Fatalf("audit %+v %v", events, err)
			}
			if err = scheduler.ProcessDue(origin.Add(2 * time.Minute)); err != nil {
				t.Fatal(err)
			}
			want := 1
			if mode == "warning" {
				want = 2
			}
			if calls != want {
				t.Fatalf("%s let %d runners start, want %d", mode, calls, want)
			}
		})
	}
}
