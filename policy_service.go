package main

import (
	"context"
	"log"
	"sync"
	"time"

	policypkg "debian-updater/internal/policies"
)

type PolicyServiceDeps = policypkg.ServiceDeps
type PolicyScheduleRequest = policypkg.ScheduleRequest
type PolicyScheduleProjectionRequest = policypkg.ScheduleProjectionRequest
type PolicyScheduleProjection = policypkg.ScheduleProjection
type PolicyMatchContext = policypkg.MatchContext
type PolicySchedulerOptions = policypkg.SchedulerOptions
type PolicyScheduledRunRequest = policypkg.ScheduledRunRequest
type PolicyScheduledRunResult = policypkg.ScheduledRunResult
type PolicyService = policypkg.Service

var (
	defaultPolicyServiceOnce sync.Once
	defaultPolicyServiceInst *PolicyService
)

func NewPolicyService(deps PolicyServiceDeps) *PolicyService {
	return policypkg.NewService(policyServiceDepsWithDefaults(deps))
}

func defaultPolicyService() *PolicyService {
	defaultPolicyServiceOnce.Do(func() {
		defaultPolicyServiceInst = NewPolicyService(PolicyServiceDeps{})
	})
	return defaultPolicyServiceInst
}

func policyServiceDepsWithDefaults(deps PolicyServiceDeps) PolicyServiceDeps {
	if deps.ListPolicies == nil {
		deps.ListPolicies = listUpdatePolicies
	}
	if deps.LoadOverrides == nil {
		deps.LoadOverrides = loadAllUpdatePolicyOverrides
	}
	if deps.LoadGlobalBlackouts == nil {
		deps.LoadGlobalBlackouts = loadGlobalUpdatePolicyBlackouts
	}
	if deps.ListRuns == nil {
		deps.ListRuns = listUpdatePolicyRuns
	}
	if deps.SnapshotServers == nil {
		deps.SnapshotServers = snapshotServers
	}
	if deps.HandleScheduledRun == nil {
		deps.HandleScheduledRun = handleScheduledRunRequest
	}
	if deps.CurrentLocation == nil {
		deps.CurrentLocation = currentAppLocation
	}
	if deps.MarkInterruptedRuns == nil {
		deps.MarkInterruptedRuns = markInterruptedUpdatePolicyRuns
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	if deps.TimestampLayout == "" {
		deps.TimestampLayout = jobTimestampLayout
	}
	return deps
}

func startPolicyScheduler(service *PolicyService, ctx context.Context, options PolicySchedulerOptions) {
	if service == nil {
		service = defaultPolicyService()
	}
	service.StartScheduler(ctx, options)
}
