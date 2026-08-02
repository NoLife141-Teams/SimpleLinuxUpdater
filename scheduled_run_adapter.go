package main

import (
	scheduledrunspkg "debian-updater/internal/scheduledruns"
	serverpkg "debian-updater/internal/servers"
)

func scheduledRunLifecycleDepsFromApp(deps AppDeps) scheduledrunspkg.Deps {
	return scheduledrunspkg.Deps{
		AuditService:           deps.AuditService,
		CurrentJobManager:      deps.CurrentJobManager,
		JobTimestampNow:        deps.JobTimestampNow,
		LoadRetryPolicy:        deps.LoadRetryPolicy,
		MaintenanceCoordinator: deps.MaintenanceCoordinator,
		MaintenanceReadiness: func(server Server) serverpkg.MaintenanceReadiness {
			return deps.MaintenanceReadiness([]Server{server})[server.Name]
		},
		PolicyRepository:                deps.PolicyRepository,
		ServerState:                     deps.ServerState,
		StartJobRunner:                  deps.StartJobRunner,
		StartScheduledRunReconciliation: deps.StartScheduledRunReconciliation,
		UpdateService:                   deps.UpdateService,
	}
}

func scheduledRunLifecycleFromApp(deps AppDeps) *scheduledrunspkg.Lifecycle {
	return scheduledRunLifecycleFromComposedApp(deps.withDefaults())
}

func scheduledRunLifecycleFromComposedApp(deps AppDeps) *scheduledrunspkg.Lifecycle {
	return scheduledrunspkg.New(scheduledRunLifecycleDepsFromApp(deps))
}

func defaultScheduledRunLifecycle() *scheduledrunspkg.Lifecycle {
	return scheduledRunLifecycleFromApp(globalRuntimeAppDeps())
}
