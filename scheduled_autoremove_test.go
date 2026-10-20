package main

import (
	"testing"

	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/policies"
	"debian-updater/internal/scheduledruns"
	serverpkg "debian-updater/internal/servers"
)

func TestScheduledAutoremoveHonorsMaintenanceAndServerAdmission(t *testing.T) {
	for _, tc := range []struct{ name, status, reason string }{
		{"busy", "updating", policies.RunReasonBusy},
		{"approval pending", "pending_approval", policies.RunReasonBusy},
		{"untrusted", "idle", policies.RunReasonReadiness},
		{"backup", "idle", policies.RunReasonMaintenance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := Server{Name: "cleanup-gate", Host: "example.test", User: "root"}
			deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "cleanup-gates.db", server, tc.status)
			policy.ExecutionMode = policies.ExecutionAutoremove
			deps.StartJobRunner = func(string, func(), ...func()) { t.Fatal("blocked cleanup was dispatched") }
			if tc.name == "untrusted" {
				deps.MaintenanceReadiness = func(Server) serverpkg.MaintenanceReadiness {
					return serverpkg.MaintenanceReadiness{Code: serverpkg.MaintenanceReadinessHostKeyNotTrusted, Message: "SSH host key is not trusted"}
				}
			}
			if tc.name == "backup" {
				lease, decision := deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
				if !decision.Allowed {
					t.Fatalf("backup not admitted: %+v", decision)
				}
				defer lease.Close()
			}
			scheduledruns.New(deps).ExecuteRun(run, policy, server)
			stored := getScheduledLifecycleRun(t, deps, run.ID)
			if stored.Status != policies.RunSkipped || stored.Reason != tc.reason || stored.JobID != "" {
				t.Fatalf("blocked cleanup = %+v", stored)
			}
		})
	}
}
