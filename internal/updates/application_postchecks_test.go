package updates

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/postchecks"
	"debian-updater/internal/servers"
)

func applicationTestProfile(kind, mode string) *postchecks.Snapshot {
	c := postchecks.Check{ID: "health", Name: "Application health", Kind: kind, Mode: mode, TimeoutSeconds: 3, IntervalSeconds: 1}
	if kind == "http_status" {
		c.URL = "https://localhost/health"
		c.ExpectedStatus = 200
	} else {
		c.Unit = "nginx.service"
	}
	return &postchecks.Snapshot{ID: "app", Name: "App", Revision: 2, Checks: []postchecks.Check{c}}
}
func TestApplicationPostchecksExecuteBoundedReadOnlyProbes(t *testing.T) {
	for _, kind := range []string{"http_status", "systemd_active"} {
		for _, mode := range []string{"blocking", "warning"} {
			t.Run(kind+"/"+mode, func(t *testing.T) {
				clock := time.Now()
				calls := 0
				session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(ctx context.Context, r HostCommandRequest) (HostCommandResult, error) {
					calls++
					if _, ok := ctx.Deadline(); !ok {
						t.Fatal("no bounded context")
					}
					if r.Effect != HostCommandEffectReadOnly || r.ReplayPolicy != ReplayNever || r.OnOutput != nil {
						t.Fatal("unsafe command request")
					}
					if strings.Contains(r.Command, "sudo") || strings.Contains(r.Command, "--insecure") || strings.Contains(r.Command, "--location") {
						t.Fatal(r.Command)
					}
					if kind == "http_status" {
						if !strings.Contains(r.Command, "--disable") || !strings.Contains(r.Command, "--output /dev/null") || !strings.Contains(r.Command, "--url 'https://localhost/health'") {
							t.Fatal(r.Command)
						}
						code := "500"
						if calls == 2 {
							code = "200"
						}
						return HostCommandResult{Stdout: code, Stderr: "SECRET RESPONSE"}, nil
					}
					if calls == 1 {
						return HostCommandResult{Stderr: "SECRET"}, errors.New("inactive")
					}
					return HostCommandResult{}, nil
				}}
				results := runApplicationChecks(context.Background(), session, applicationTestProfile(kind, mode), func() time.Time { return clock }, func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil })
				if len(results) != 1 || !results[0].Passed || results[0].Attempts != 2 || results[0].DurationMS != 1000 || strings.Contains(results[0].Details, "SECRET") {
					t.Fatalf("results %+v", results)
				}
			})
		}
	}
}
func TestApplicationPostchecksFailuresTimeoutMissingToolAndCancellation(t *testing.T) {
	for _, scenario := range []string{"timeout", "missing tool", "cancel", "invalid", "transport"} {
		t.Run(scenario, func(t *testing.T) {
			clock := time.Now()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			p := applicationTestProfile("http_status", "blocking")
			if scenario == "invalid" {
				p.Checks[0].URL = "file:///etc/passwd"
			}
			calls := 0
			session := &HostMaintenanceSessionFuncs{RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
				calls++
				if scenario == "missing tool" {
					return HostCommandResult{Stdout: "SLU_TOOL_MISSING"}, errors.New("missing curl")
				}
				if scenario == "cancel" {
					cancel()
				}
				if scenario == "transport" {
					return HostCommandResult{}, errors.New("SECRET ssh error")
				}
				return HostCommandResult{Stdout: "500"}, nil
			}}
			results := runApplicationChecks(ctx, session, p, func() time.Time { return clock }, func(_ context.Context, d time.Duration) error { clock = clock.Add(d); return nil })
			if len(results) != 1 || results[0].Passed || strings.Contains(results[0].Details, "SECRET") {
				t.Fatalf("%+v", results)
			}
			if scenario == "invalid" && calls != 0 {
				t.Fatal("invalid check executed")
			}
			if scenario == "missing tool" && calls != 1 {
				t.Fatal("missing tool retried")
			}
			if calls > 3 {
				t.Fatal("unbounded retries")
			}
		})
	}
	if got := runApplicationChecks(context.Background(), &HostMaintenanceSessionFuncs{}, nil, time.Now, nil); got != nil {
		t.Fatal(got)
	}
}
func TestReadOnlyApplicationTestOpensAndClosesSession(t *testing.T) {
	closed := false
	s := NewService(ServiceDeps{HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(_ context.Context, r HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		if r.DialOperation != "postchecks.test.ssh_dial" || r.RetryPolicy.MaxAttempts != 1 {
			t.Fatal(r)
		}
		return &HostMaintenanceSessionFuncs{RunCommandFunc: func(context.Context, HostCommandRequest) (HostCommandResult, error) {
			return HostCommandResult{Stdout: "200"}, nil
		}, CloseFunc: func() error { closed = true; return nil }}, nil
	})})
	if _, err := s.TestApplicationChecks(context.Background(), servers.Server{}, nil); err == nil {
		t.Fatal("missing profile")
	}
	got, err := s.TestApplicationChecks(context.Background(), servers.Server{}, applicationTestProfile("http_status", "warning"))
	if err != nil || !got[0].Passed || !closed {
		t.Fatalf("%+v %v closed %v", got, err, closed)
	}
	failing := NewService(ServiceDeps{HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return nil, errors.New("SECRET")
	})})
	if _, err = failing.TestApplicationChecks(context.Background(), servers.Server{}, applicationTestProfile("http_status", "warning")); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

func TestUpdateRunnerApplicationChecksPersistAuditAndRequireRefreshedApproval(t *testing.T) {
	for _, scenario := range []string{"blocking failure", "warning failure", "success", "profile changed", "profile unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			clock := time.Now()
			loads := 0
			mutations := 0
			polls := 0
			p := applicationTestProfile("http_status", "blocking")
			if scenario == "warning failure" {
				p.Checks[0].Mode = "warning"
			}
			session := &HostMaintenanceSessionFuncs{
				DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
					return HostPackageDiscoveryResult{Outcome: newPackageDiscoveryOutcome([]servers.PendingUpdate{{Package: "openssl", CVEState: "done"}}, []string{"openssl"}, servers.UpgradePlan{StandardPackageCount: 1}), Attempts: 1}, nil
				},
				RunCommandFunc: func(_ context.Context, r HostCommandRequest) (HostCommandResult, error) {
					if r.Effect == HostCommandEffectPackageStateMutation {
						mutations++
					}
					if strings.HasPrefix(r.Operation, "postcheck.application.") {
						code := "200"
						if strings.Contains(scenario, "failure") {
							code = "500"
						}
						return HostCommandResult{Stdout: code, Attempts: 1}, nil
					}
					return HostCommandResult{Attempts: 1}, nil
				},
			}
			var state *servers.State
			service, jm, st, audits := newUpdateShutdownHarness(t, context.Background(), session, func(d *ServiceDeps) {
				d.Now = func() time.Time { return clock }
				d.SleepContext = func(_ context.Context, delay time.Duration) error { clock = clock.Add(delay); return nil }
				d.SaveServerFacts = func(ServerFactsRecord) error { return nil }
				d.LoadApplicationPostchecks = func(servers.Server) (*postchecks.Snapshot, error) {
					loads++
					if scenario == "profile unavailable" {
						return nil, errors.New("conflicting profiles")
					}
					copy := postchecks.CloneSnapshot(p)
					if scenario == "profile changed" && loads > 1 {
						copy.Revision++
						copy.Checks[0].Name = "New expectation"
					}
					return copy, nil
				}
				d.LoadScheduledJobBehavior = func(string) ScheduledJobBehavior {
					return ScheduledJobBehavior{AutoApproveScope: ApprovalScopeAll, ApprovalTimeout: time.Hour}
				}
				d.WaitForApprovalPollContext = func(context.Context) error {
					polls++
					snapshot := state.CurrentStatusSnapshot("srv-shutdown")
					if snapshot.ApplicationPostcheckProfile == nil || snapshot.ApplicationPostcheckProfile.Revision != 3 {
						t.Fatalf("approval missing refreshed profile: %+v", snapshot)
					}
					state.Lock()
					status := state.StatusMap()["srv-shutdown"]
					status.Status = "approved"
					status.ApprovalScope = ApprovalScopeAll
					state.Unlock()
					return nil
				}
			})
			state = st
			if err := jm.MergeActiveMetadata("update-shutdown-job", map[string]any{"foreign": "preserved", "policy_id": 42}); err != nil {
				t.Fatal(err)
			}
			service.RunUpdateJob(UpdateRunRequest{Server: state.CloneServers()[0], JobID: "update-shutdown-job", Actor: "operator", Policy: RetryPolicy{MaxAttempts: 1}})
			job, err := jm.GetJob("update-shutdown-job")
			if err != nil {
				t.Fatal(err)
			}
			expected := jobs.StatusSucceeded
			if scenario == "blocking failure" || scenario == "profile unavailable" {
				expected = jobs.StatusFailed
			}
			if job.Status != expected {
				t.Fatalf("job %+v status %+v", job, state.CurrentStatusSnapshot("srv-shutdown"))
			}
			if scenario == "profile unavailable" {
				if mutations != 0 {
					t.Fatal("mutated despite conflict")
				}
				return
			}
			if mutations != 1 {
				t.Fatalf("package mutations %d", mutations)
			}
			var meta map[string]json.RawMessage
			if err = json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil {
				t.Fatal(err)
			}
			if string(meta["foreign"]) != `"preserved"` || string(meta["policy_id"]) != "42" {
				t.Fatalf("lost foreign metadata %s", job.MetaJSON)
			}
			var captured postchecks.Snapshot
			var results []postchecks.Result
			if err = json.Unmarshal(meta["application_postcheck_profile"], &captured); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(meta["application_postcheck_results"], &results); err != nil {
				t.Fatal(err)
			}
			if len(results) != 1 || results[0].Attempts == 0 {
				t.Fatalf("results %+v", results)
			}
			audit := <-audits
			if audit.meta["application_postcheck_profile"].(*postchecks.Snapshot).Revision != captured.Revision || len(audit.meta["application_postcheck_results"].([]postchecks.Result)) != 1 {
				t.Fatalf("audit missing exact facts %+v", audit)
			}
			if scenario == "warning failure" && (audit.meta["postcheck_warnings"] != 1 || audit.status != "success") {
				t.Fatalf("warning blocked update %+v", audit)
			}
			if scenario == "profile changed" && (captured.Revision != 3 || polls != 1 || state.CurrentStatusSnapshot("srv-shutdown").ApprovalGeneration != 2) {
				t.Fatalf("did not reapprove refreshed expectations %+v polls=%d", captured, polls)
			}
		})
	}
}
