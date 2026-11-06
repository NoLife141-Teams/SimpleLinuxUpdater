package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"
)

func TestIdleBackupAutomationDoesNotSkipScheduledRuns(t *testing.T) {
	for _, mode := range []string{"disabled", "future", "retry", "suspended"} {
		t.Run(mode, func(t *testing.T) {
			app, a, now := newBackupAutomationTest(t)
			state := backupAutomationStateForTest(t, a)
			switch mode {
			case "disabled":
				state.Config.Enabled = false
			case "retry":
				state.PendingAt = backupStamp(*now)
				state.RetryAt = backupStamp(now.Add(5 * time.Minute))
			case "suspended":
				state.Approval = ""
			}
			if err := a.save(context.Background(), state); err != nil {
				t.Fatal(err)
			}
			slot := *now
			var handled []PolicyScheduledRunRequest
			deps := testPolicyServiceDeps()
			deps.Maintenance = app.Deps.MaintenanceCoordinator
			deps.ListPolicies = func() ([]UpdatePolicy, error) {
				return []UpdatePolicy{{ID: 1, Name: "daily", Enabled: true, TargetServers: []string{"srv"}, PackageScope: updatePolicyPackageScopeSecurity, ExecutionMode: updatePolicyExecutionScanOnly, CadenceKind: updatePolicyCadenceDaily, TimeLocal: slot.Format("15:04")}}, nil
			}
			deps.SnapshotServers = func() []Server { return []Server{{Name: "srv"}} }
			deps.HandleScheduledRun = func(req PolicyScheduledRunRequest) PolicyScheduledRunResult {
				handled = append(handled, req)
				return PolicyScheduledRunResult{Handled: true, Inserted: true}
			}
			service := NewPolicyService(deps)
			watermark := slot.Add(-time.Minute)
			store := policypkg.SchedulerWatermarkStore{Load: func() (time.Time, bool, error) { return watermark, true, nil }, Save: func(value time.Time) error { watermark = value; return nil }}
			db := a.deps.DB
			probed := false
			a.deps.DB = func() *sql.DB {
				if !probed {
					probed = true
					if err := service.ProcessDueWithRecovery(slot, store); err != nil {
						t.Fatal(err)
					}
				}
				return db()
			}
			if err := a.Tick(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			if !probed {
				t.Fatal("backup tick did not read persisted state")
			}
			if err := service.ProcessDueWithRecovery(slot.Add(time.Minute), store); err != nil {
				t.Fatal(err)
			}
			if len(handled) != 1 || handled[0].Outcome != "" {
				t.Fatalf("idle backup tick caused scheduled run to be skipped: %+v", handled)
			}
		})
	}
}
