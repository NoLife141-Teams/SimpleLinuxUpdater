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

type policySchedulerWatermarkRepository interface {
	LoadSchedulerWatermark() (time.Time, bool, error)
	SaveSchedulerWatermark(time.Time) error
	LoadSchedulerStateFingerprint() (string, bool, error)
	SaveSchedulerStateFingerprint(string) error
	HasSchedulerRecoveryScope(int64, string) (bool, error)
	MarkSchedulerRecoveryScope(int64, string) error
}

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
	useDefaultPolicyRepository := deps.ListPolicies == nil &&
		deps.LoadOverrides == nil &&
		deps.LoadGlobalBlackouts == nil &&
		deps.ListRuns == nil
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
	if deps.ListRolloutRuns == nil && useDefaultPolicyRepository {
		deps.ListRolloutRuns = defaultPolicyRepository().ListRolloutRuns
	}
	if deps.ReconcileRun == nil {
		deps.ReconcileRun = func(run policypkg.Run) (policypkg.Run, error) {
			return defaultScheduledRunLifecycle().ReconcileRun(context.Background(), run)
		}
	}
	if deps.SnapshotServers == nil {
		deps.SnapshotServers = snapshotServers
	}
	if deps.HandleScheduledRun == nil {
		deps.HandleScheduledRun = func(req policypkg.ScheduledRunRequest) policypkg.ScheduledRunResult {
			return defaultScheduledRunLifecycle().HandleScheduledRun(req)
		}
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

func startPolicyScheduler(service *PolicyService, repository policypkg.Repository, ctx context.Context, options PolicySchedulerOptions) {
	if service == nil {
		service = defaultPolicyService()
	}
	if repository == nil {
		repository = defaultPolicyRepository()
	}
	watermarkRepository, ok := repository.(policySchedulerWatermarkRepository)
	if !ok {
		// A custom repository that does not expose recovery checkpoint
		// persistence keeps the legacy scheduler rather than reading or writing
		// another app's DB.
		service.StartScheduler(ctx, options)
		return
	}
	service.StartSchedulerWithRecovery(ctx, options, policypkg.SchedulerWatermarkStore{
		Load:                 watermarkRepository.LoadSchedulerWatermark,
		Save:                 watermarkRepository.SaveSchedulerWatermark,
		LoadStateFingerprint: watermarkRepository.LoadSchedulerStateFingerprint,
		SaveStateFingerprint: watermarkRepository.SaveSchedulerStateFingerprint,
		HasRecoveryScope:     watermarkRepository.HasSchedulerRecoveryScope,
		MarkRecoveryScope:    watermarkRepository.MarkSchedulerRecoveryScope,
	})
}
