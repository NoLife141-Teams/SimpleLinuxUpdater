package main

import (
	"context"
	"errors"
	"log"
	"strings"
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

type policySchedulerStateRevisionRepository interface {
	LoadSchedulerStateRevision() (int64, error)
}

type policySchedulerCheckpointRepository interface {
	LoadSchedulerCheckpoint() (policypkg.SchedulerCheckpoint, bool, error)
	SaveSchedulerCheckpoint(policypkg.SchedulerCheckpoint) error
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

	store := policypkg.SchedulerWatermarkStore{
		Load:                 watermarkRepository.LoadSchedulerWatermark,
		Save:                 watermarkRepository.SaveSchedulerWatermark,
		LoadStateFingerprint: watermarkRepository.LoadSchedulerStateFingerprint,
		SaveStateFingerprint: watermarkRepository.SaveSchedulerStateFingerprint,
		HasRecoveryScope:     watermarkRepository.HasSchedulerRecoveryScope,
		MarkRecoveryScope:    watermarkRepository.MarkSchedulerRecoveryScope,
	}
	if checkpointRepository, supportsAtomicCheckpoint := repository.(policySchedulerCheckpointRepository); supportsAtomicCheckpoint {
		store = atomicPolicySchedulerStore(checkpointRepository, watermarkRepository)
	}
	if revisionRepository, supportsRevision := repository.(policySchedulerStateRevisionRepository); supportsRevision {
		store.LoadStateRevision = revisionRepository.LoadSchedulerStateRevision
	}
	service.StartSchedulerWithRecovery(ctx, options, store)
}

// atomicPolicySchedulerStore adapts the existing two-callback scheduler API to
// a single durable checkpoint transaction. Save() only stages the watermark in
// memory; SaveStateFingerprint() commits watermark+fingerprint together. A
// crash between those callbacks therefore leaves the previous complete
// checkpoint intact rather than advancing only half of it.
func atomicPolicySchedulerStore(checkpointRepository policySchedulerCheckpointRepository, recoveryScopes policySchedulerWatermarkRepository) policypkg.SchedulerWatermarkStore {
	var mu sync.Mutex
	var loaded policypkg.SchedulerCheckpoint
	var loadedFound bool
	var loadedReady bool
	var pendingWatermark time.Time

	return policypkg.SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) {
			checkpoint, found, err := checkpointRepository.LoadSchedulerCheckpoint()
			if err != nil {
				return time.Time{}, false, err
			}
			mu.Lock()
			loaded = checkpoint
			loadedFound = found
			loadedReady = true
			mu.Unlock()
			return checkpoint.Watermark, found, nil
		},
		LoadStateFingerprint: func() (string, bool, error) {
			mu.Lock()
			if loadedReady {
				checkpoint := loaded
				found := loadedFound
				loadedReady = false
				mu.Unlock()
				fingerprint := strings.TrimSpace(checkpoint.StateFingerprint)
				return fingerprint, found && fingerprint != "", nil
			}
			mu.Unlock()
			checkpoint, found, err := checkpointRepository.LoadSchedulerCheckpoint()
			if err != nil {
				return "", false, err
			}
			fingerprint := strings.TrimSpace(checkpoint.StateFingerprint)
			return fingerprint, found && fingerprint != "", nil
		},
		Save: func(value time.Time) error {
			if value.IsZero() {
				return errors.New("policy scheduler watermark is required")
			}
			mu.Lock()
			pendingWatermark = value.UTC().Truncate(time.Minute)
			mu.Unlock()
			return nil
		},
		SaveStateFingerprint: func(value string) error {
			fingerprint := strings.TrimSpace(value)
			if fingerprint == "" {
				return errors.New("policy scheduler state fingerprint is required")
			}
			mu.Lock()
			watermark := pendingWatermark
			mu.Unlock()
			if watermark.IsZero() {
				return errors.New("policy scheduler watermark was not staged")
			}
			if err := checkpointRepository.SaveSchedulerCheckpoint(policypkg.SchedulerCheckpoint{
				Watermark:        watermark,
				StateFingerprint: fingerprint,
			}); err != nil {
				return err
			}
			mu.Lock()
			pendingWatermark = time.Time{}
			mu.Unlock()
			return nil
		},
		HasRecoveryScope:  recoveryScopes.HasSchedulerRecoveryScope,
		MarkRecoveryScope: recoveryScopes.MarkSchedulerRecoveryScope,
	}
}
