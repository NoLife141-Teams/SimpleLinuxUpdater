package main

import (
	"context"
	"errors"
	"log"
	"strings"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

type backupOperationKind string

const (
	backupOperationSucceeded                 backupOperationKind = "succeeded"
	backupOperationActiveServerActions       backupOperationKind = "active_server_actions"
	backupOperationSnapshotFailed            backupOperationKind = "snapshot_failed"
	backupOperationJobManagerUnavailable     backupOperationKind = "job_manager_unavailable"
	backupOperationJobCreateFailed           backupOperationKind = "job_create_failed"
	backupOperationMaintenanceActivateFailed backupOperationKind = "maintenance_activate_failed"
	backupOperationMaintenanceReleaseFailed  backupOperationKind = "maintenance_release_failed"
	backupOperationExportFailed              backupOperationKind = "export_failed"
	backupOperationRestoreDecryptFailed      backupOperationKind = "restore_decrypt_failed"
	backupOperationRestoreArchiveFailed      backupOperationKind = "restore_archive_failed"
	backupOperationRestoreApplyFailed        backupOperationKind = "restore_apply_failed"
)

type backupArchiveRunner interface {
	CreateDBSnapshot() ([]byte, error)
	ExportArchive(context.Context, backupExportRequest) (internalbackup.ExportResult, error)
	RestoreArchiveWithOptions(context.Context, []byte, string, internalbackup.RestoreOptions) (internalbackup.RestoreResult, error)
}

type backupOperationAuditRecord struct {
	Actor      string
	ClientIP   string
	Action     string
	TargetType string
	TargetName string
	Status     string
	Message    string
	Meta       map[string]any
}

type backupOperationLifecycleDeps struct {
	Archive                 backupArchiveRunner
	CurrentJobManager       func() *JobManager
	ActiveServerActionNames func() []string
	ActivateMaintenance     func(kind, jobID, actor, message string) error
	DeactivateMaintenance   func() error
	RecordAudit             func(backupOperationAuditRecord)
	EnsureEncryptionKey     func() []byte
	JobTimestampNow         func() string
	MarshalJobJSON          func(any) string
	Logf                    func(string, ...any)
}

type backupOperationLifecycle struct {
	deps backupOperationLifecycleDeps
}

type backupExportCommand struct {
	Actor    string
	ClientIP string
	Request  backupExportRequest
	Lease    *maintenancepkg.ExclusiveLease
}

type backupRestoreCommand struct {
	Actor      string
	ClientIP   string
	Blob       []byte
	Passphrase string
	Lease      *maintenancepkg.ExclusiveLease
}

type backupOperationOutcome struct {
	Kind                backupOperationKind
	JobID               string
	PublicError         string
	ActiveServers       []string
	ExportBytes         []byte
	KnownHostsIncluded  bool
	GlobalKeyPresent    bool
	KnownHostsRestored  bool
	SessionsInvalidated bool
	Err                 error
}

func newBackupOperationLifecycle(deps AppDeps) *backupOperationLifecycle {
	deps = deps.withDefaults()
	return newBackupOperationLifecycleWithDeps(backupOperationLifecycleDeps{
		Archive:           deps.BackupService,
		CurrentJobManager: deps.CurrentJobManager,
		ActiveServerActionNames: func() []string {
			if deps.ServerState != nil {
				return deps.ServerState.ActiveActionNames()
			}
			return activeServerActionNames()
		},
		RecordAudit: func(record backupOperationAuditRecord) {
			if deps.AuditService != nil {
				if err := deps.AuditService.Record(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta); err != nil {
					log.Printf("audit write failed: action=%s target=%s err=%v", record.Action, record.TargetName, err)
				}
				return
			}
			auditWithActor(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
		},
		EnsureEncryptionKey: getEncryptionKey,
		JobTimestampNow:     jobTimestampNow,
		MarshalJobJSON:      marshalJobJSON,
		Logf:                log.Printf,
	})
}

func newBackupOperationLifecycleWithDeps(deps backupOperationLifecycleDeps) *backupOperationLifecycle {
	return &backupOperationLifecycle{deps: deps.withDefaults()}
}

func (deps backupOperationLifecycleDeps) withDefaults() backupOperationLifecycleDeps {
	if deps.CurrentJobManager == nil {
		deps.CurrentJobManager = currentJobManager
	}
	if deps.ActiveServerActionNames == nil {
		deps.ActiveServerActionNames = activeServerActionNames
	}
	if deps.ActivateMaintenance == nil {
		deps.ActivateMaintenance = func(string, string, string, string) error { return errors.New("maintenance lease is not configured") }
	}
	if deps.DeactivateMaintenance == nil {
		deps.DeactivateMaintenance = func() error { return nil }
	}
	if deps.RecordAudit == nil {
		deps.RecordAudit = func(record backupOperationAuditRecord) {
			auditWithActor(record.Actor, record.ClientIP, record.Action, record.TargetType, record.TargetName, record.Status, record.Message, record.Meta)
		}
	}
	if deps.EnsureEncryptionKey == nil {
		deps.EnsureEncryptionKey = getEncryptionKey
	}
	if deps.JobTimestampNow == nil {
		deps.JobTimestampNow = jobTimestampNow
	}
	if deps.MarshalJobJSON == nil {
		deps.MarshalJobJSON = marshalJobJSON
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return deps
}

func (l *backupOperationLifecycle) Export(ctx context.Context, cmd backupExportCommand) (outcome backupOperationOutcome) {
	deps := l.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	cmd.Request.Passphrase = strings.TrimSpace(cmd.Request.Passphrase)

	if activeServers := cloneStrings(deps.ActiveServerActionNames()); len(activeServers) > 0 {
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.export",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Active server actions must finish before export",
			Meta:       map[string]any{"active_servers": activeServers},
		})
		return backupOperationOutcome{
			Kind:          backupOperationActiveServerActions,
			PublicError:   "wait for active server actions to finish before starting backup export",
			ActiveServers: activeServers,
		}
	}

	dbSnapshot, err := deps.Archive.CreateDBSnapshot()
	if err != nil {
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.export",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Failed to snapshot database",
			Meta:       map[string]any{"error": err.Error()},
		})
		return backupOperationOutcome{Kind: backupOperationSnapshotFailed, PublicError: "failed to snapshot database", Err: err}
	}
	cmd.Request.DBSnapshot = dbSnapshot

	jm := deps.CurrentJobManager()
	if jm == nil {
		return backupOperationOutcome{Kind: backupOperationJobManagerUnavailable, PublicError: "job manager unavailable"}
	}
	job, err := jm.CreateJob(JobCreateParams{
		Kind:     jobKindBackupExport,
		Actor:    cmd.Actor,
		ClientIP: cmd.ClientIP,
		Status:   jobStatusRunning,
		Phase:    jobPhaseSnapshot,
		Summary:  "Preparing backup export",
	})
	if err != nil {
		return backupOperationOutcome{Kind: backupOperationJobCreateFailed, PublicError: "failed to create backup export job", Err: err}
	}
	activateErr := error(nil)
	if cmd.Lease != nil {
		activateErr = cmd.Lease.Activate(ctx, maintenancepkg.OperationFacts{JobID: job.ID, Actor: cmd.Actor, Message: "Backup export in progress. The application will reopen when the encrypted archive is ready."})
	} else {
		activateErr = deps.ActivateMaintenance(jobKindBackupExport, job.ID, cmd.Actor, "Backup export in progress. The application will reopen when the encrypted archive is ready.")
	}
	if activateErr != nil {
		err := activateErr
		deps.Logf("backup export lifecycle: activateMaintenance failed for job %q: %v", job.ID, err)
		status := jobStatusFailed
		summary := "Failed to activate maintenance mode"
		errorClass := "maintenance"
		_ = jm.Transition(job.ID, JobTransitionIntent{
			Status:     &status,
			Summary:    &summary,
			ErrorClass: &errorClass,
		})
		return backupOperationOutcome{Kind: backupOperationMaintenanceActivateFailed, JobID: job.ID, PublicError: "failed to activate maintenance mode", Err: err}
	}
	var completion *JobTransitionIntent
	defer func() {
		var err error
		if cmd.Lease != nil {
			err = cmd.Lease.Close()
		} else {
			err = deps.DeactivateMaintenance()
		}
		if err != nil {
			deps.Logf("backup export lifecycle: failed to clear maintenance mode: %v", err)
			outcome = l.failMaintenanceRelease(job.ID, cmd.Actor, cmd.ClientIP, "backup.export", err)
			return
		}
		if completion != nil {
			_ = jm.Transition(job.ID, *completion)
		}
	}()

	_ = deps.EnsureEncryptionKey()
	phase := jobPhaseEncrypt
	summary := "Encrypting backup payload"
	_ = jm.Transition(job.ID, JobTransitionIntent{Phase: &phase, Summary: &summary})
	result, err := deps.Archive.ExportArchive(ctx, cmd.Request)
	if err != nil {
		return l.failExportArchive(jm, job.ID, cmd, err)
	}

	status := jobStatusSucceeded
	phase = jobPhaseComplete
	summary = "Backup export completed"
	meta := deps.MarshalJobJSON(map[string]any{
		"bytes":                len(result.Bytes),
		"known_hosts_included": result.KnownHostsIncluded,
	})
	completion = &JobTransitionIntent{
		Status:   &status,
		Phase:    &phase,
		Summary:  &summary,
		MetaJSON: &meta,
	}
	deps.RecordAudit(backupOperationAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "backup.export",
		TargetType: "backup",
		TargetName: "state",
		Status:     "success",
		Message:    "Backup exported",
		Meta: map[string]any{
			"bytes":                len(result.Bytes),
			"known_hosts_included": result.KnownHostsIncluded,
		},
	})
	return backupOperationOutcome{
		Kind:               backupOperationSucceeded,
		JobID:              job.ID,
		ExportBytes:        append([]byte(nil), result.Bytes...),
		KnownHostsIncluded: result.KnownHostsIncluded,
	}
}

func (l *backupOperationLifecycle) failExportArchive(jm *JobManager, jobID string, cmd backupExportCommand, err error) backupOperationOutcome {
	deps := l.deps.withDefaults()
	summary := "Failed to build backup payload"
	errorClass := "archive"
	publicError := "failed to build backup"
	auditMessage := "Failed to build backup payload"
	var exportErr *internalbackup.ExportError
	if errors.As(err, &exportErr) {
		switch exportErr.Stage {
		case internalbackup.ExportStageSnapshot:
			summary = "Failed to snapshot database"
			errorClass = "snapshot"
			publicError = "failed to snapshot database"
			auditMessage = "Failed to snapshot database"
		case internalbackup.ExportStageConfig:
			summary = "Failed to read config"
			errorClass = "config"
			publicError = "failed to read config"
			auditMessage = "Failed to read config"
		case internalbackup.ExportStageEncrypt:
			summary = "Failed to encrypt backup payload"
			errorClass = "encrypt"
			publicError = "failed to encrypt backup"
			auditMessage = "Failed to encrypt backup"
		}
	}
	status := jobStatusFailed
	_ = jm.Transition(jobID, JobTransitionIntent{
		Status:     &status,
		Summary:    &summary,
		ErrorClass: &errorClass,
	})
	deps.RecordAudit(backupOperationAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "backup.export",
		TargetType: "backup",
		TargetName: "state",
		Status:     "failure",
		Message:    auditMessage,
		Meta:       map[string]any{"error": err.Error()},
	})
	return backupOperationOutcome{Kind: backupOperationExportFailed, JobID: jobID, PublicError: publicError, Err: err}
}

func (l *backupOperationLifecycle) Restore(ctx context.Context, cmd backupRestoreCommand) (outcome backupOperationOutcome) {
	deps := l.deps.withDefaults()
	if ctx == nil {
		ctx = context.Background()
	}
	cmd.Actor = strings.TrimSpace(cmd.Actor)
	cmd.ClientIP = strings.TrimSpace(cmd.ClientIP)
	cmd.Passphrase = strings.TrimSpace(cmd.Passphrase)

	if activeServers := cloneStrings(deps.ActiveServerActionNames()); len(activeServers) > 0 {
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.restore",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Active server actions must finish before restore",
			Meta:       map[string]any{"active_servers": activeServers},
		})
		return backupOperationOutcome{
			Kind:          backupOperationActiveServerActions,
			PublicError:   "wait for active server actions to finish before starting backup restore",
			ActiveServers: activeServers,
		}
	}

	jm := deps.CurrentJobManager()
	if jm == nil {
		return backupOperationOutcome{Kind: backupOperationJobManagerUnavailable, PublicError: "job manager unavailable"}
	}
	job, err := jm.CreateJob(JobCreateParams{
		Kind:     jobKindBackupRestore,
		Actor:    cmd.Actor,
		ClientIP: cmd.ClientIP,
		Status:   jobStatusRunning,
		Phase:    jobPhaseDecrypt,
		Summary:  "Restoring backup archive",
	})
	if err != nil {
		return backupOperationOutcome{Kind: backupOperationJobCreateFailed, PublicError: "failed to create backup restore job", Err: err}
	}
	activateErr := error(nil)
	if cmd.Lease != nil {
		activateErr = cmd.Lease.Activate(ctx, maintenancepkg.OperationFacts{JobID: job.ID, Actor: cmd.Actor, Message: "Backup restore in progress. Requests are paused until the restored state is ready."})
	} else {
		activateErr = deps.ActivateMaintenance(jobKindBackupRestore, job.ID, cmd.Actor, "Backup restore in progress. Requests are paused until the restored state is ready.")
	}
	if activateErr != nil {
		err := activateErr
		status := jobStatusFailed
		summary := "Failed to activate maintenance mode"
		errorClass := "maintenance"
		_ = jm.Transition(job.ID, JobTransitionIntent{
			Status:     &status,
			Summary:    &summary,
			ErrorClass: &errorClass,
		})
		return backupOperationOutcome{Kind: backupOperationMaintenanceActivateFailed, JobID: job.ID, PublicError: "failed to activate maintenance mode", Err: err}
	}
	var restoredCompletion *JobRecord
	defer func() {
		var err error
		if cmd.Lease != nil {
			err = cmd.Lease.Close()
		} else {
			err = deps.DeactivateMaintenance()
		}
		if err != nil {
			deps.Logf("backup restore lifecycle: failed to clear maintenance mode: %v", err)
			failed := job
			failed.Status = jobStatusFailed
			failed.Phase = jobPhaseComplete
			failed.Summary = "Maintenance mode could not be cleared"
			failed.ErrorClass = "maintenance_coordination"
			failed.FinishedAt = deps.JobTimestampNow()
			if manager := deps.CurrentJobManager(); manager != nil {
				_ = manager.ImportJobRecord(failed)
			}
			deps.RecordAudit(backupOperationAuditRecord{Actor: cmd.Actor, ClientIP: cmd.ClientIP, Action: "backup.restore", TargetType: "backup", TargetName: "state", Status: "failure", Message: failed.Summary, Meta: map[string]any{"error": err.Error()}})
			outcome = backupOperationOutcome{Kind: backupOperationMaintenanceReleaseFailed, JobID: job.ID, PublicError: "maintenance mode could not be cleared", Err: err}
			return
		}
		if restoredCompletion != nil {
			if manager := deps.CurrentJobManager(); manager != nil {
				_ = manager.ImportJobRecord(*restoredCompletion)
			}
		}
	}()

	result, err := deps.Archive.RestoreArchiveWithOptions(ctx, cmd.Blob, cmd.Passphrase, internalbackup.RestoreOptions{
		BeforeApply: func() {
			phase := jobPhaseApply
			summary := "Applying restored backup files"
			_ = jm.Transition(job.ID, JobTransitionIntent{Phase: &phase, Summary: &summary})
		},
		RestoreHandoff: func(ctx context.Context) error {
			if cmd.Lease == nil {
				return errors.New("maintenance lease is not configured")
			}
			return cmd.Lease.Handoff(ctx)
		},
	})
	if err != nil {
		return l.failRestoreArchive(job, cmd, err)
	}
	jm = deps.CurrentJobManager()
	deps.RecordAudit(backupOperationAuditRecord{
		Actor:      cmd.Actor,
		ClientIP:   cmd.ClientIP,
		Action:     "backup.restore",
		TargetType: "backup",
		TargetName: "state",
		Status:     "success",
		Message:    "Backup restored",
		Meta: map[string]any{
			"manifest_files":       len(result.Manifest.Files),
			"global_key_present":   result.GlobalKeyPresent,
			"known_hosts_restored": result.KnownHostsRestored,
		},
	})
	status := jobStatusSucceeded
	phase := jobPhaseComplete
	summary := "Backup restore completed"
	finishedAt := deps.JobTimestampNow()
	meta := deps.MarshalJobJSON(map[string]any{
		"manifest_files":       len(result.Manifest.Files),
		"global_key_present":   result.GlobalKeyPresent,
		"known_hosts_restored": result.KnownHostsRestored,
		"sessions_invalidated": true,
	})
	if jm != nil {
		job.Status = status
		job.Phase = phase
		job.Summary = summary
		job.MetaJSON = meta
		job.FinishedAt = finishedAt
		restoredCompletion = &job
	}
	return backupOperationOutcome{
		Kind:                backupOperationSucceeded,
		JobID:               job.ID,
		GlobalKeyPresent:    result.GlobalKeyPresent,
		KnownHostsRestored:  result.KnownHostsRestored,
		SessionsInvalidated: true,
	}
}

func (l *backupOperationLifecycle) failMaintenanceRelease(jobID, actor, clientIP, action string, err error) backupOperationOutcome {
	deps := l.deps.withDefaults()
	status := jobStatusFailed
	summary := "Maintenance mode could not be cleared"
	errorClass := "maintenance_coordination"
	if jm := deps.CurrentJobManager(); jm != nil {
		_ = jm.Transition(jobID, JobTransitionIntent{Status: &status, Summary: &summary, ErrorClass: &errorClass})
	}
	deps.RecordAudit(backupOperationAuditRecord{
		Actor: actor, ClientIP: clientIP, Action: action, TargetType: "backup", TargetName: "state",
		Status: "failure", Message: summary, Meta: map[string]any{"error": err.Error()},
	})
	return backupOperationOutcome{Kind: backupOperationMaintenanceReleaseFailed, JobID: jobID, PublicError: "maintenance mode could not be cleared", Err: err}
}

func (l *backupOperationLifecycle) failRestoreArchive(job JobRecord, cmd backupRestoreCommand, err error) backupOperationOutcome {
	deps := l.deps.withDefaults()
	stage := internalbackup.RestoreStageApply
	var restoreErr *internalbackup.RestoreError
	if errors.As(err, &restoreErr) {
		stage = restoreErr.Stage
	}
	switch stage {
	case internalbackup.RestoreStageDecrypt:
		status := jobStatusFailed
		summary := "Failed to decrypt backup archive"
		errorClass := "decrypt"
		if jm := deps.CurrentJobManager(); jm != nil {
			_ = jm.Transition(job.ID, JobTransitionIntent{
				Status:     &status,
				Summary:    &summary,
				ErrorClass: &errorClass,
			})
		}
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.restore",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Failed to decrypt backup",
			Meta:       map[string]any{"error": err.Error()},
		})
		return backupOperationOutcome{Kind: backupOperationRestoreDecryptFailed, JobID: job.ID, PublicError: "failed to decrypt backup", Err: err}
	case internalbackup.RestoreStageArchive:
		status := jobStatusFailed
		summary := "Invalid backup payload"
		errorClass := "archive"
		if jm := deps.CurrentJobManager(); jm != nil {
			_ = jm.Transition(job.ID, JobTransitionIntent{
				Status:     &status,
				Summary:    &summary,
				ErrorClass: &errorClass,
			})
		}
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.restore",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Invalid backup payload",
			Meta:       map[string]any{"error": err.Error()},
		})
		return backupOperationOutcome{Kind: backupOperationRestoreArchiveFailed, JobID: job.ID, PublicError: "invalid backup payload", Err: err}
	default:
		jm := deps.CurrentJobManager()
		status := jobStatusFailed
		summary := "Failed to apply backup files"
		errorClass := "apply"
		finishedAt := deps.JobTimestampNow()
		if jm != nil {
			job.Status = status
			job.Phase = jobPhaseComplete
			job.Summary = summary
			job.ErrorClass = errorClass
			job.FinishedAt = finishedAt
			_ = jm.ImportJobRecord(job)
		}
		deps.RecordAudit(backupOperationAuditRecord{
			Actor:      cmd.Actor,
			ClientIP:   cmd.ClientIP,
			Action:     "backup.restore",
			TargetType: "backup",
			TargetName: "state",
			Status:     "failure",
			Message:    "Failed to apply backup",
			Meta:       map[string]any{"error": err.Error()},
		})
		return backupOperationOutcome{Kind: backupOperationRestoreApplyFailed, JobID: job.ID, PublicError: "failed to apply backup", Err: err}
	}
}

func cloneStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	return append([]string(nil), values...)
}
