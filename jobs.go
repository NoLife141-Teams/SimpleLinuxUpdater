package main

import (
	"context"
	"database/sql"
	"log"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	internaljobs "debian-updater/internal/jobs"
	runtimepkg "debian-updater/internal/runtime"
	serverpkg "debian-updater/internal/servers"
)

const (
	jobKindUpdate         = internaljobs.KindUpdate
	jobKindAutoremove     = internaljobs.KindAutoremove
	jobKindSudoersEnable  = internaljobs.KindSudoersEnable
	jobKindSudoersDisable = internaljobs.KindSudoersDisable
	jobKindCVEEnrichment  = internaljobs.KindCVEEnrichment
	jobKindBackupExport   = internaljobs.KindBackupExport
	jobKindBackupRestore  = internaljobs.KindBackupRestore
	jobKindScheduledScan  = internaljobs.KindScheduledScan

	jobStatusQueued          = internaljobs.StatusQueued
	jobStatusRunning         = internaljobs.StatusRunning
	jobStatusWaitingApproval = internaljobs.StatusWaitingApproval
	jobStatusSucceeded       = internaljobs.StatusSucceeded
	jobStatusFailed          = internaljobs.StatusFailed
	jobStatusCancelled       = internaljobs.StatusCancelled
	jobStatusInterrupted     = internaljobs.StatusInterrupted
	jobIntentResumeApproval  = internaljobs.IntentResumeAfterApproval
	jobIntentWaitApproval    = internaljobs.IntentWaitForApproval
	jobIntentCancel          = internaljobs.IntentCancel

	jobPhaseDial         = internaljobs.PhaseDial
	jobPhasePrechecks    = internaljobs.PhasePrechecks
	jobPhaseAptUpdate    = internaljobs.PhaseAptUpdate
	jobPhaseApprovalWait = internaljobs.PhaseApprovalWait
	jobPhaseAptUpgrade   = internaljobs.PhaseAptUpgrade
	jobPhasePostchecks   = internaljobs.PhasePostchecks
	jobPhaseAutoremove   = internaljobs.PhaseAutoremove
	jobPhaseApply        = internaljobs.PhaseApply
	jobPhaseSnapshot     = internaljobs.PhaseSnapshot
	jobPhaseEncrypt      = internaljobs.PhaseEncrypt
	jobPhaseDecrypt      = internaljobs.PhaseDecrypt
	jobPhaseLookup       = internaljobs.PhaseLookup
	jobPhaseComplete     = internaljobs.PhaseComplete

	jobTimestampLayout = internaljobs.TimestampLayout

	jobLogRetentionDaysEnv = "DEBIAN_UPDATER_JOB_LOG_RETENTION_DAYS"
	jobLogMaxBytesEnv      = "DEBIAN_UPDATER_JOB_LOG_MAX_BYTES"
	jobLogPruneInterval    = 24 * time.Hour
)

type JobRecord = internaljobs.Record
type JobTransitionIntent = internaljobs.Intent
type JobCreateParams = internaljobs.CreateParams
type JobManager = internaljobs.Manager

var (
	jobManagerMu sync.RWMutex
	jobManager   *JobManager
)

func currentJobManager() *JobManager {
	jobManagerMu.RLock()
	defer jobManagerMu.RUnlock()
	return jobManager
}

func setCurrentJobManager(jm *JobManager) {
	jobManagerMu.Lock()
	defer jobManagerMu.Unlock()
	jobManager = jm
}

func newJobManager(db *sql.DB) *JobManager {
	return newJobManagerWithNotify(db, notifyDashboardEvent)
}

func newJobManagerWithNotify(db *sql.DB, notify func(string)) *JobManager {
	return newJobManagerWithRuntime(db, notify, globalServerState(), nil)
}

func newJobManagerWithRuntime(db *sql.DB, notify func(string), state *serverpkg.State, _ func() bool) *JobManager {
	return newJobManagerWithRuntimeConfig(db, notify, state, loadJobLogConfigFromEnv(), time.Now)
}

func newJobManagerWithRuntimeConfig(db *sql.DB, notify func(string), state *serverpkg.State, config internaljobs.LogConfig, now func() time.Time) *JobManager {
	if notify == nil {
		notify = notifyDashboardEvent
	}
	if state == nil {
		state = globalServerState()
	}
	return internaljobs.NewManager(internaljobs.NewSQLiteRepositoryWithLogConfig(db, config), internaljobs.ManagerOptions{
		Notify: notify,
		SyncRuntime: func(record JobRecord) {
			syncServerStateFromJobRecord(state, record)
		},
		SyncInterruptedServer: func(serverNames []string) {
			markInterruptedServerStateIdle(state, serverNames)
		},
		Now: now,
	})
}

func initializeJobManager() error {
	jm := newJobManager(getDB())
	if err := jm.MarkUnfinishedJobsInterrupted(); err != nil {
		return err
	}
	if _, err := jm.PurgeExpiredLogs(); err != nil {
		return err
	}
	setCurrentJobManager(jm)
	return nil
}

func ensureJobSchema(db *sql.DB) error {
	return internaljobs.EnsureSchemaConfigured(db, loadJobLogConfigFromEnv())
}

func loadJobLogConfigFromEnv() internaljobs.LogConfig {
	config := internaljobs.DefaultLogConfig()
	if raw := strings.TrimSpace(os.Getenv(jobLogRetentionDaysEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 3650 {
			log.Printf("Invalid %s=%q, must be an integer in [1,3650], using default %d", jobLogRetentionDaysEnv, raw, config.RetentionDays)
		} else {
			config.RetentionDays = value
		}
	}
	if raw := strings.TrimSpace(os.Getenv(jobLogMaxBytesEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < internaljobs.MinLogMaxBytes || value > internaljobs.MaxLogMaxBytes {
			log.Printf("Invalid %s=%q, must be an integer in [%d,%d], using default %d", jobLogMaxBytesEnv, raw, internaljobs.MinLogMaxBytes, internaljobs.MaxLogMaxBytes, config.MaxBytes)
		} else {
			config.MaxBytes = value
		}
	}
	return config
}

func startJobLogPruner(ctx context.Context, current func() *JobManager) {
	if current == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(jobLogPruneInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				manager := current()
				if manager == nil {
					continue
				}
				if _, err := manager.PurgeExpiredLogs(); err != nil {
					log.Printf("job log prune failed: %v", err)
				}
			case <-ctx.Done():
				return
			}
		}
	}()
}

func marshalJobJSON(v any) string {
	return internaljobs.MarshalJSON(v)
}

func formatJobTimestamp(t time.Time) string {
	return internaljobs.FormatTimestamp(t)
}

func jobTimestampNow() string {
	return formatJobTimestamp(time.Now())
}

func runtimeStatusFromJob(record JobRecord) string {
	return runtimepkg.RuntimeStatusFromJob(record)
}

func syncServerStateFromJobRecord(state *serverpkg.State, record JobRecord) {
	if strings.TrimSpace(record.ServerName) == "" {
		return
	}
	statusValue := runtimeStatusFromJob(record)
	if statusValue == "" {
		return
	}
	if state == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	status := state.StatusMap()[record.ServerName]
	if status == nil {
		return
	}
	status.Status = statusValue
	if record.LogsText != "" {
		status.Logs = record.LogsText
	}
	if record.Status == jobStatusInterrupted {
		status.ApprovalScope = ""
		status.ApprovalConfirmRemovals = false
		status.Upgradable = nil
		status.PendingUpdates = nil
		status.UpgradePlan = serverpkg.UpgradePlan{}
	}
}

func markInterruptedServerStateIdle(state *serverpkg.State, serverNames []string) {
	if state == nil {
		return
	}
	state.Lock()
	defer state.Unlock()
	for _, serverName := range serverNames {
		status := state.StatusMap()[serverName]
		if status == nil {
			continue
		}
		status.Status = "idle"
		status.ApprovalScope = ""
		status.ApprovalConfirmRemovals = false
		status.Upgradable = nil
		status.PendingUpdates = nil
		status.UpgradePlan = serverpkg.UpgradePlan{}
	}
}

func startJobRunner(jobID string, run func(), onAdmissionFailure ...func()) {
	startJobRunnerWithManager(currentJobManager, jobID, run, onAdmissionFailure...)
}

func startJobRunnerWithManager(current func() *JobManager, jobID string, run func(), onAdmissionFailure ...func()) {
	if current == nil {
		current = currentJobManager
	}
	startTrackedActionRunner(func() {
		restoreRuntime := func() {
			for _, restore := range onAdmissionFailure {
				if restore != nil {
					restore()
				}
			}
		}
		jm := current()
		if strings.TrimSpace(jobID) != "" {
			if jm == nil {
				log.Printf("failed to start job %q: job manager is unavailable", jobID)
				restoreRuntime()
				return
			}
			status := jobStatusRunning
			if err := jm.Transition(jobID, JobTransitionIntent{
				Status: &status,
			}); err != nil {
				log.Printf("failed to mark job %q running: %v", jobID, err)
				failedStatus := jobStatusFailed
				phase := jobPhaseComplete
				summary := "Runner admission failed"
				errorClass := "persistence"
				if terminalErr := jm.Transition(jobID, JobTransitionIntent{
					Status:     &failedStatus,
					Phase:      &phase,
					Summary:    &summary,
					ErrorClass: &errorClass,
				}); terminalErr != nil {
					log.Printf("failed to terminalize job %q after runner admission failure: %v", jobID, terminalErr)
				}
				restoreRuntime()
				return
			}
		}
		defer func() {
			if recovered := recover(); recovered != nil {
				log.Printf("job runner panic for job %q: %v", jobID, recovered)
				if jm := current(); jm != nil && strings.TrimSpace(jobID) != "" {
					status := jobStatusFailed
					phase := jobPhaseComplete
					summary := "Runner panicked"
					errorClass := "panic"
					_ = jm.Transition(jobID, JobTransitionIntent{
						Status:     &status,
						Phase:      &phase,
						Summary:    &summary,
						ErrorClass: &errorClass,
					})
				}
			}
		}()
		run()
	})
}
