package scheduledruns_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"debian-updater/internal/audit"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/scheduledruns"
	"debian-updater/internal/servers"
	"debian-updater/internal/updates"
)

func TestScheduledAutoremoveRunsOnlyCleanupAndReconcilesWithAuditContext(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "command failure"}[failure], func(t *testing.T) {
			db, repository, _, audits := newReconciliationHarness(t)
			server := servers.Server{Name: "cleanup-host", User: "root"}
			inventory := []servers.Server{server}
			statuses := map[string]*servers.ServerStatus{server.Name: {Name: server.Name, Status: "idle"}}
			state := servers.NewState(&sync.Mutex{}, &inventory, &statuses, nil)
			manager := jobs.NewManager(jobs.NewSQLiteRepository(db), jobs.ManagerOptions{NewID: func() string { return "cleanup-job" }})
			commands, admissions, releases := 0, 0, 0
			service := updates.NewService(updates.ServiceDeps{
				ServerState: state, CurrentJobManager: func() *jobs.Manager { return manager },
				AuditWithActor: func(actor, ip, action, targetType, target, status, message string, meta map[string]any) {
					if err := audits.Record(actor, ip, action, targetType, target, status, message, meta); err != nil {
						t.Fatal(err)
					}
				},
				HostMaintenanceSessions: updates.HostMaintenanceSessionFactoryFunc(func(context.Context, updates.HostMaintenanceSessionRequest) (updates.HostMaintenanceSession, error) {
					return &updates.HostMaintenanceSessionFuncs{
						RunUpdatePrechecksFunc: func(context.Context) updates.PrecheckSummary {
							t.Fatal("cleanup must not run update disk-space prechecks")
							return updates.PrecheckSummary{}
						},
						RunCommandFunc: func(_ context.Context, req updates.HostCommandRequest) (updates.HostCommandResult, error) {
							commands++
							if req.Operation != "autoremove.command" || req.Command != updates.AptAutoremoveCmd || req.Effect != updates.HostCommandEffectPackageStateMutation || req.ReplayPolicy != updates.ReplayRetryableOutputErrors {
								t.Fatalf("unexpected scheduled command: %+v", req)
							}
							if failure {
								return updates.HostCommandResult{Attempts: 1}, errors.New("apt cleanup failed")
							}
							return updates.HostCommandResult{Stdout: "Removing unused-package\n", Attempts: 1}, nil
						},
					}, nil
				}),
			})
			var runner func()
			lifecycle := scheduledruns.New(scheduledruns.Deps{
				AuditService: audits, CurrentJobManager: func() *jobs.Manager { return manager },
				LoadRetryPolicy:  func() updates.RetryPolicy { return updates.RetryPolicy{MaxAttempts: 1} },
				PolicyRepository: repository, ServerState: state, UpdateService: service,
				AcquireScheduledAction:          func(string) func() { admissions++; return func() { releases++ } },
				StartJobRunner:                  func(_ string, run func(), _ ...func()) { runner = run },
				StartScheduledRunReconciliation: func(int64, string) {},
			})
			req := policies.ScheduledRunRequest{
				Policy: policies.Policy{ID: 52, Name: "Weekly cleanup", ExecutionMode: policies.ExecutionAutoremove, PackageScope: policies.PackageScopeFull, UpgradeMode: policies.UpgradeModeStandard},
				Server: server, ScheduledForUTC: "2026-08-09T04:30:00Z", Admitted: true,
			}
			result := lifecycle.HandleScheduledRun(req)
			if result.Err != nil || result.Status != policies.RunRunning || runner == nil {
				t.Fatalf("cleanup was not admitted: %+v", result)
			}
			if status := state.CurrentStatusSnapshot(server.Name); status.Status != "autoremove" {
				t.Fatalf("runtime status = %s", status.Status)
			}
			runner()
			job, err := manager.GetJob(result.JobID)
			if err != nil {
				t.Fatal(err)
			}
			wantJob, wantRun := jobs.StatusSucceeded, policies.RunSucceeded
			if failure {
				wantJob, wantRun = jobs.StatusFailed, policies.RunFailed
			}
			if job.Kind != jobs.KindAutoremove || job.Status != wantJob || commands != 1 || admissions != 1 || releases != 1 {
				t.Fatalf("job=%+v commands=%d admission=%d/%d", job, commands, admissions, releases)
			}
			var meta updates.ScheduledJobMeta
			if err := json.Unmarshal([]byte(job.MetaJSON), &meta); err != nil || meta.RunID != result.RunID || meta.PolicyID != req.Policy.ID || meta.AutoApproveScope != "" {
				t.Fatalf("scheduled metadata = %+v, %v", meta, err)
			}
			run, err := repository.GetRun(result.RunID)
			if err != nil || run.Status != wantRun || run.JobID != job.ID || run.ExecutionMode != policies.ExecutionAutoremove {
				t.Fatalf("run = %+v, %v", run, err)
			}
			listed, err := audits.List(audit.ListFilter{Action: "autoremove.complete"})
			if err != nil || len(listed.Items) != 1 {
				t.Fatalf("audit = %+v, %v", listed, err)
			}
			for _, fact := range []string{`"trigger":"scheduled"`, `"policy_id":52`, `"job_kind":"autoremove"`, `"run_id":`, `"scheduled_for_utc":"2026-08-09T04:30:00Z"`} {
				if !strings.Contains(listed.Items[0].MetaJSON, fact) {
					t.Fatalf("audit missing %s: %s", fact, listed.Items[0].MetaJSON)
				}
			}
			action := "schedule.run.completed"
			if failure {
				action = "schedule.run.failed"
			}
			terminal, err := audits.List(audit.ListFilter{Action: action})
			if err != nil || terminal.Total != 1 || !strings.Contains(terminal.Items[0].MetaJSON, `"job_kind":"autoremove"`) {
				t.Fatalf("scheduled terminal audit = %+v, %v", terminal, err)
			}
			duplicate := lifecycle.HandleScheduledRun(req)
			if duplicate.Inserted || commands != 1 {
				t.Fatalf("cleanup occurrence replayed: %+v", duplicate)
			}
		})
	}
}
