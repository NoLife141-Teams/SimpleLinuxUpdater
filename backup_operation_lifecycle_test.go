package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"

	internalbackup "debian-updater/internal/backup"
)

type fakeBackupArchive struct {
	snapshotData  []byte
	snapshotErr   error
	exportResult  internalbackup.ExportResult
	exportErr     error
	restoreResult internalbackup.RestoreResult
	restoreErr    error

	snapshotCalled bool
	exportRequest  backupExportRequest
	restoreBlob    []byte
	beforeApply    bool
}

func (f *fakeBackupArchive) CreateDBSnapshot() ([]byte, error) {
	f.snapshotCalled = true
	if f.snapshotErr != nil {
		return nil, f.snapshotErr
	}
	return append([]byte(nil), f.snapshotData...), nil
}

func (f *fakeBackupArchive) ExportArchive(_ context.Context, req backupExportRequest) (internalbackup.ExportResult, error) {
	f.exportRequest = req
	if f.exportErr != nil {
		return internalbackup.ExportResult{}, f.exportErr
	}
	result := f.exportResult
	result.Bytes = append([]byte(nil), result.Bytes...)
	return result, nil
}

func (f *fakeBackupArchive) RestoreArchiveWithOptions(_ context.Context, encrypted []byte, _ string, opts internalbackup.RestoreOptions) (internalbackup.RestoreResult, error) {
	f.restoreBlob = append([]byte(nil), encrypted...)
	if opts.BeforeApply != nil {
		opts.BeforeApply()
		f.beforeApply = true
	}
	if f.restoreErr != nil {
		return internalbackup.RestoreResult{}, f.restoreErr
	}
	return f.restoreResult, nil
}

type backupLifecycleHarness struct {
	lifecycle   *backupOperationLifecycle
	archive     *fakeBackupArchive
	db          *sql.DB
	jobManager  *JobManager
	audits      []backupOperationAuditRecord
	activated   []string
	deactivated int
	persisted   []MaintenanceState
}

func newBackupLifecycleHarness(t *testing.T) *backupLifecycleHarness {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/jobs.db")
	if err != nil {
		t.Fatalf("open lifecycle test db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
	})
	if err := ensureJobSchema(db); err != nil {
		t.Fatalf("ensureJobSchema: %v", err)
	}
	h := &backupLifecycleHarness{
		archive: &fakeBackupArchive{snapshotData: []byte("db-snapshot")},
		db:      db,
	}
	h.jobManager = newJobManagerWithRuntime(db, nil, newServerState(), func() bool { return false })
	h.lifecycle = newBackupOperationLifecycleWithDeps(backupOperationLifecycleDeps{
		Archive: h.archive,
		CurrentJobManager: func() *JobManager {
			return h.jobManager
		},
		ActiveServerActionNames: func() []string {
			return nil
		},
		ActivateMaintenance: func(kind, jobID, actor, message string) error {
			h.activated = append(h.activated, strings.Join([]string{kind, jobID, actor, message}, "|"))
			return nil
		},
		DeactivateMaintenance: func() error {
			h.deactivated++
			return nil
		},
		CurrentMaintenanceState: func() MaintenanceState {
			return MaintenanceState{Active: true, Kind: jobKindBackupRestore, JobID: "restore-job"}
		},
		PersistMaintenanceState: func(state MaintenanceState) error {
			h.persisted = append(h.persisted, state)
			return nil
		},
		RecordAudit: func(record backupOperationAuditRecord) {
			h.audits = append(h.audits, record)
		},
		EnsureEncryptionKey: func() []byte {
			return []byte("key")
		},
		JobTimestampNow: func() string {
			return "2026-07-09T00:00:00Z"
		},
	})
	return h
}

func TestBackupOperationLifecycleExportSuccess(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	h.archive.exportResult = internalbackup.ExportResult{
		Bytes:              []byte("encrypted-archive"),
		KnownHostsIncluded: true,
	}

	outcome := h.lifecycle.Export(context.Background(), backupExportCommand{
		Actor:    "admin",
		ClientIP: "127.0.0.1",
		Request:  backupExportRequest{Passphrase: "very-strong-passphrase"},
	})

	if outcome.Kind != backupOperationSucceeded {
		t.Fatalf("Export outcome kind = %q, want %q (err=%v)", outcome.Kind, backupOperationSucceeded, outcome.Err)
	}
	if outcome.JobID == "" || string(outcome.ExportBytes) != "encrypted-archive" || !outcome.KnownHostsIncluded {
		t.Fatalf("Export outcome = %+v, want job id, archive bytes, known_hosts included", outcome)
	}
	if string(h.archive.exportRequest.DBSnapshot) != "db-snapshot" {
		t.Fatalf("export DBSnapshot = %q, want db-snapshot", string(h.archive.exportRequest.DBSnapshot))
	}
	if len(h.activated) != 1 || !strings.HasPrefix(h.activated[0], jobKindBackupExport+"|"+outcome.JobID+"|admin|") {
		t.Fatalf("maintenance activations = %+v, want backup export activation for job", h.activated)
	}
	if h.deactivated != 1 {
		t.Fatalf("maintenance deactivated %d times, want 1", h.deactivated)
	}
	if len(h.audits) != 1 || h.audits[0].Action != "backup.export" || h.audits[0].Status != "success" || h.audits[0].Message != "Backup exported" {
		t.Fatalf("audits = %+v, want backup export success", h.audits)
	}
	job, err := h.jobManager.GetJob(outcome.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", outcome.JobID, err)
	}
	if job.Status != jobStatusSucceeded || job.Phase != jobPhaseComplete || job.Summary != "Backup export completed" {
		t.Fatalf("job status/phase/summary = %q/%q/%q, want succeeded/complete/export completed", job.Status, job.Phase, job.Summary)
	}
	if !strings.Contains(job.MetaJSON, `"bytes":17`) || !strings.Contains(job.MetaJSON, `"known_hosts_included":true`) {
		t.Fatalf("job meta = %s, want bytes and known_hosts_included", job.MetaJSON)
	}
}

func TestBackupOperationLifecycleExportRejectsActiveServerActionsBeforeJob(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	h.lifecycle.deps.ActiveServerActionNames = func() []string {
		return []string{"srv-busy"}
	}

	outcome := h.lifecycle.Export(context.Background(), backupExportCommand{
		Actor:    "admin",
		ClientIP: "127.0.0.1",
		Request:  backupExportRequest{Passphrase: "very-strong-passphrase"},
	})

	if outcome.Kind != backupOperationActiveServerActions {
		t.Fatalf("Export outcome kind = %q, want active server actions", outcome.Kind)
	}
	if h.archive.snapshotCalled {
		t.Fatalf("snapshot was called despite active server actions")
	}
	if len(h.audits) != 1 || h.audits[0].Status != "failure" || h.audits[0].Message != "Active server actions must finish before export" {
		t.Fatalf("audits = %+v, want active action failure audit", h.audits)
	}
	if outcome.ActiveServers[0] != "srv-busy" {
		t.Fatalf("active servers = %+v, want srv-busy", outcome.ActiveServers)
	}
}

func TestBackupOperationLifecycleExportSnapshotFailureDoesNotCreateJob(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	h.archive.snapshotErr = errors.New("snapshot failed")

	outcome := h.lifecycle.Export(context.Background(), backupExportCommand{
		Actor:    "admin",
		ClientIP: "127.0.0.1",
		Request:  backupExportRequest{Passphrase: "very-strong-passphrase"},
	})

	if outcome.Kind != backupOperationSnapshotFailed || outcome.PublicError != "failed to snapshot database" {
		t.Fatalf("Export outcome = %+v, want snapshot failure", outcome)
	}
	if len(h.audits) != 1 || h.audits[0].Message != "Failed to snapshot database" {
		t.Fatalf("audits = %+v, want snapshot failure audit", h.audits)
	}
	var jobCount int
	if err := h.db.QueryRow("SELECT COUNT(1) FROM jobs").Scan(&jobCount); err != nil {
		t.Fatalf("query job count: %v", err)
	}
	if jobCount != 0 {
		t.Fatalf("job count = %d, want 0 when snapshot fails before job creation", jobCount)
	}
}

func TestBackupOperationLifecycleExportMapsArchiveStages(t *testing.T) {
	tests := []struct {
		name        string
		stage       internalbackup.ExportStage
		wantSummary string
		wantClass   string
		wantPublic  string
		wantAudit   string
	}{
		{
			name:        "config",
			stage:       internalbackup.ExportStageConfig,
			wantSummary: "Failed to read config",
			wantClass:   "config",
			wantPublic:  "failed to read config",
			wantAudit:   "Failed to read config",
		},
		{
			name:        "encrypt",
			stage:       internalbackup.ExportStageEncrypt,
			wantSummary: "Failed to encrypt backup payload",
			wantClass:   "encrypt",
			wantPublic:  "failed to encrypt backup",
			wantAudit:   "Failed to encrypt backup",
		},
		{
			name:        "archive",
			stage:       internalbackup.ExportStageArchive,
			wantSummary: "Failed to build backup payload",
			wantClass:   "archive",
			wantPublic:  "failed to build backup",
			wantAudit:   "Failed to build backup payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBackupLifecycleHarness(t)
			h.archive.exportErr = &internalbackup.ExportError{Stage: tt.stage, Err: errors.New("stage failed")}

			outcome := h.lifecycle.Export(context.Background(), backupExportCommand{
				Actor:    "admin",
				ClientIP: "127.0.0.1",
				Request:  backupExportRequest{Passphrase: "very-strong-passphrase"},
			})

			if outcome.Kind != backupOperationExportFailed || outcome.PublicError != tt.wantPublic {
				t.Fatalf("Export outcome = %+v, want export failed with public error %q", outcome, tt.wantPublic)
			}
			job, err := h.jobManager.GetJob(outcome.JobID)
			if err != nil {
				t.Fatalf("GetJob(%q): %v", outcome.JobID, err)
			}
			if job.Status != jobStatusFailed || job.Summary != tt.wantSummary || job.ErrorClass != tt.wantClass {
				t.Fatalf("job = status %q summary %q class %q, want failed/%q/%q", job.Status, job.Summary, job.ErrorClass, tt.wantSummary, tt.wantClass)
			}
			if len(h.audits) != 1 || h.audits[0].Message != tt.wantAudit {
				t.Fatalf("audits = %+v, want message %q", h.audits, tt.wantAudit)
			}
			if h.deactivated != 1 {
				t.Fatalf("maintenance deactivated %d times, want 1", h.deactivated)
			}
		})
	}
}

func TestBackupOperationLifecycleRestoreMapsDecryptAndArchiveFailures(t *testing.T) {
	tests := []struct {
		name        string
		stage       internalbackup.RestoreStage
		wantKind    backupOperationKind
		wantSummary string
		wantClass   string
		wantPublic  string
		wantAudit   string
	}{
		{
			name:        "decrypt",
			stage:       internalbackup.RestoreStageDecrypt,
			wantKind:    backupOperationRestoreDecryptFailed,
			wantSummary: "Failed to decrypt backup archive",
			wantClass:   "decrypt",
			wantPublic:  "failed to decrypt backup",
			wantAudit:   "Failed to decrypt backup",
		},
		{
			name:        "archive",
			stage:       internalbackup.RestoreStageArchive,
			wantKind:    backupOperationRestoreArchiveFailed,
			wantSummary: "Invalid backup payload",
			wantClass:   "archive",
			wantPublic:  "invalid backup payload",
			wantAudit:   "Invalid backup payload",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newBackupLifecycleHarness(t)
			h.archive.restoreErr = &internalbackup.RestoreError{Stage: tt.stage, Err: errors.New("restore failed")}

			outcome := h.lifecycle.Restore(context.Background(), backupRestoreCommand{
				Actor:      "admin",
				ClientIP:   "127.0.0.1",
				Blob:       []byte("encrypted-restore"),
				Passphrase: "very-strong-passphrase",
			})

			if outcome.Kind != tt.wantKind || outcome.PublicError != tt.wantPublic {
				t.Fatalf("Restore outcome = %+v, want %q with public error %q", outcome, tt.wantKind, tt.wantPublic)
			}
			job, err := h.jobManager.GetJob(outcome.JobID)
			if err != nil {
				t.Fatalf("GetJob(%q): %v", outcome.JobID, err)
			}
			if job.Status != jobStatusFailed || job.Summary != tt.wantSummary || job.ErrorClass != tt.wantClass {
				t.Fatalf("job = status %q summary %q class %q, want failed/%q/%q", job.Status, job.Summary, job.ErrorClass, tt.wantSummary, tt.wantClass)
			}
			if len(h.audits) != 1 || h.audits[0].Message != tt.wantAudit {
				t.Fatalf("audits = %+v, want message %q", h.audits, tt.wantAudit)
			}
			if h.deactivated != 1 {
				t.Fatalf("maintenance deactivated %d times, want 1", h.deactivated)
			}
		})
	}
}

func TestBackupOperationLifecycleRestoreSuccess(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	h.archive.restoreResult = internalbackup.RestoreResult{
		Manifest: internalbackup.Manifest{Files: map[string]internalbackup.ManifestFile{
			"servers.db":  {},
			"config.json": {},
		}},
		GlobalKeyPresent:   true,
		KnownHostsRestored: true,
	}

	outcome := h.lifecycle.Restore(context.Background(), backupRestoreCommand{
		Actor:      "admin",
		ClientIP:   "127.0.0.1",
		Blob:       []byte("encrypted-restore"),
		Passphrase: "very-strong-passphrase",
	})

	if outcome.Kind != backupOperationSucceeded {
		t.Fatalf("Restore outcome kind = %q, want %q (err=%v)", outcome.Kind, backupOperationSucceeded, outcome.Err)
	}
	if outcome.JobID == "" || !outcome.SessionsInvalidated || !outcome.GlobalKeyPresent || !outcome.KnownHostsRestored {
		t.Fatalf("Restore outcome = %+v, want job id and restored facts", outcome)
	}
	if !h.archive.beforeApply {
		t.Fatalf("restore did not invoke BeforeApply")
	}
	if len(h.persisted) != 1 || h.persisted[0].JobID != "restore-job" {
		t.Fatalf("persisted maintenance states = %+v, want current restore state persisted", h.persisted)
	}
	if len(h.audits) != 1 || h.audits[0].Action != "backup.restore" || h.audits[0].Status != "success" || h.audits[0].Message != "Backup restored" {
		t.Fatalf("audits = %+v, want backup restore success", h.audits)
	}
	job, err := h.jobManager.GetJob(outcome.JobID)
	if err != nil {
		t.Fatalf("GetJob(%q): %v", outcome.JobID, err)
	}
	if job.Status != jobStatusSucceeded || job.Phase != jobPhaseComplete || job.Summary != "Backup restore completed" {
		t.Fatalf("job status/phase/summary = %q/%q/%q, want succeeded/complete/restore completed", job.Status, job.Phase, job.Summary)
	}
	if !strings.Contains(job.MetaJSON, `"sessions_invalidated":true`) || !strings.Contains(job.MetaJSON, `"manifest_files":2`) {
		t.Fatalf("job meta = %s, want sessions_invalidated and manifest_files", job.MetaJSON)
	}
}

func TestBackupOperationLifecycleRestoreApplyFailureUpsertsIntoCurrentJobManager(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	replacementDB, err := sql.Open("sqlite", t.TempDir()+"/replacement-jobs.db")
	if err != nil {
		t.Fatalf("open replacement db: %v", err)
	}
	t.Cleanup(func() {
		_ = replacementDB.Close()
	})
	if err := ensureJobSchema(replacementDB); err != nil {
		t.Fatalf("ensure replacement job schema: %v", err)
	}
	replacementJobs := newJobManagerWithRuntime(replacementDB, nil, newServerState(), func() bool { return false })
	calls := 0
	h.lifecycle.deps.CurrentJobManager = func() *JobManager {
		calls++
		if calls == 1 {
			return h.jobManager
		}
		return replacementJobs
	}
	h.archive.restoreErr = &internalbackup.RestoreError{Stage: internalbackup.RestoreStageApply, Err: errors.New("apply failed")}

	outcome := h.lifecycle.Restore(context.Background(), backupRestoreCommand{
		Actor:      "admin",
		ClientIP:   "127.0.0.1",
		Blob:       []byte("encrypted-restore"),
		Passphrase: "very-strong-passphrase",
	})

	if outcome.Kind != backupOperationRestoreApplyFailed || outcome.PublicError != "failed to apply backup" {
		t.Fatalf("Restore outcome = %+v, want apply failure", outcome)
	}
	if calls < 2 {
		t.Fatalf("CurrentJobManager called %d times, want restore path to reacquire after apply failure", calls)
	}
	job, err := replacementJobs.GetJob(outcome.JobID)
	if err != nil {
		t.Fatalf("replacement GetJob(%q): %v", outcome.JobID, err)
	}
	if job.Status != jobStatusFailed || job.Phase != jobPhaseComplete || job.Summary != "Failed to apply backup files" || job.ErrorClass != "apply" {
		t.Fatalf("replacement job = status %q phase %q summary %q class %q, want failed/complete/apply-files/apply", job.Status, job.Phase, job.Summary, job.ErrorClass)
	}
	if len(h.audits) != 1 || h.audits[0].Message != "Failed to apply backup" {
		t.Fatalf("audits = %+v, want apply failure audit", h.audits)
	}
}
