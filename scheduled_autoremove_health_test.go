package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	healthpkg "debian-updater/internal/health"
	"debian-updater/internal/policies"
	"debian-updater/internal/scheduledruns"
	updatespkg "debian-updater/internal/updates"
)

func TestScheduledAutoremovePreservesScanHealthHistory(t *testing.T) {
	for _, outcome := range []string{"success", "command failure", "missing server", "job creation failure", "run persistence failure"} {
		t.Run(outcome, func(t *testing.T) {
			server := Server{Name: "cleanup-health", Host: "example.test", User: "root"}
			deps, policy, _, manager := newScheduledRunLifecycleTestDeps(t, "cleanup-health.db", server, "idle")
			policy.ExecutionMode = policies.ExecutionAutoremove
			policy.PackageScope, policy.UpgradeMode = policies.PackageScopeFull, policies.UpgradeModeStandard
			observation := healthpkg.SQLiteObservation{DB: getDB}
			if err := observation.AcceptMaintenance(healthpkg.MaintenanceOutcome{
				ServerName: server.Name, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano),
				Kind: healthpkg.MaintenanceKindScheduledRun, Status: "success",
				RawJSON: `{"pending_package_count":8,"security_package_count":3}`,
			}); err != nil {
				t.Fatal(err)
			}
			commands := 0
			deps.UpdateService = updatespkg.NewService(updatespkg.ServiceDeps{
				ServerState: deps.ServerState, CurrentJobManager: func() *JobManager { return manager },
				AuditWithActor: func(actor, ip, action, targetType, target, status, message string, meta map[string]any) {
					if err := deps.AuditService.Record(actor, ip, action, targetType, target, status, message, meta); err != nil {
						t.Fatal(err)
					}
				},
				HostMaintenanceSessions: updatespkg.HostMaintenanceSessionFactoryFunc(func(context.Context, updatespkg.HostMaintenanceSessionRequest) (updatespkg.HostMaintenanceSession, error) {
					return &updatespkg.HostMaintenanceSessionFuncs{
						RunCommandFunc: func(_ context.Context, req updatespkg.HostCommandRequest) (updatespkg.HostCommandResult, error) {
							commands++
							if req.Command != updatespkg.AptAutoremoveCmd {
								t.Fatalf("unexpected command: %+v", req)
							}
							if outcome == "command failure" {
								return updatespkg.HostCommandResult{Attempts: 1}, errors.New("cleanup failed")
							}
							return updatespkg.HostCommandResult{Attempts: 1}, nil
						},
					}, nil
				}),
			})
			var runner func()
			deps.StartJobRunner = func(_ string, run func(), _ ...func()) { runner = run }
			switch outcome {
			case "missing server":
				deps.ServerState.Lock()
				deps.ServerState.SetServers(nil)
				deps.ServerState.Unlock()
			case "job creation failure":
				deps.CurrentJobManager = func() *JobManager { return nil }
			case "run persistence failure":
				deps.PolicyRepository = cleanupRunningPersistenceFailure{RunRepository: deps.PolicyRepository}
			}
			result := scheduledruns.New(deps).HandleScheduledRun(policies.ScheduledRunRequest{
				Policy: policy, Server: server, ScheduledForUTC: "2026-10-25T04:30:00Z",
			})
			if !result.Inserted {
				t.Fatalf("cleanup run was not inserted: %+v", result)
			}
			if runner != nil {
				runner()
			}
			wantCommands, action := 0, "schedule.run.failed"
			if outcome == "success" || outcome == "command failure" {
				wantCommands = 1
			}
			if outcome == "success" {
				action = "schedule.run.completed"
			}
			if commands != wantCommands {
				t.Fatalf("commands = %d, want %d", commands, wantCommands)
			}
			audits, err := deps.AuditService.List(AuditListFilter{Action: action, TargetName: server.Name})
			if err != nil || audits.Total != 1 {
				t.Fatalf("cleanup terminal audit = %+v, %v", audits, err)
			}
			var auditMeta struct {
				ExecutionMode string `json:"execution_mode"`
				JobKind       string `json:"job_kind"`
			}
			if err := json.Unmarshal([]byte(audits.Items[0].MetaJSON), &auditMeta); err != nil || auditMeta.ExecutionMode != policies.ExecutionAutoremove || auditMeta.JobKind != jobKindAutoremove {
				t.Fatalf("cleanup audit identity = %+v, %v", auditMeta, err)
			}
			snapshots, err := observation.History("0000", "9999", server.Name)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshots) != 1 || snapshots[0].PackageCount != 8 || snapshots[0].SecurityCount != 3 || snapshots[0].LastScanStatus != "success" {
				t.Fatalf("cleanup changed scan health history: %+v", snapshots)
			}
		})
	}
}

type cleanupRunningPersistenceFailure struct{ scheduledruns.RunRepository }

func (r cleanupRunningPersistenceFailure) UpdateRun(id int64, update policies.RunUpdate) error {
	if update.Status != nil && *update.Status == policies.RunRunning {
		return errors.New("running state persistence failed")
	}
	return r.RunRepository.UpdateRun(id, update)
}
