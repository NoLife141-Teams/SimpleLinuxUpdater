package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
	"github.com/gin-gonic/gin"
)

const backupAutomationSettingsKey = "backup_automation_v1"
const backupDeferralWindow = time.Hour
const backupRetryDelay = 5 * time.Minute

type backupScheduleConfig struct {
	Enabled           bool   `json:"enabled"`
	Cadence           string `json:"cadence"`
	Time              string `json:"time"`
	Weekday           int    `json:"weekday"`
	IncludeKnownHosts bool   `json:"include_known_hosts"`
	Retain            int    `json:"retain"`
}

type ownedBackupArchive struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Owner  string `json:"owner"`
}

type backupAutomationState struct {
	PendingManual bool                 `json:"pending_manual"`
	Timezone      string               `json:"timezone"`
	OccurrenceAt  string               `json:"occurrence_at"`
	Config        backupScheduleConfig `json:"config"`
	Approval      string               `json:"approval"`
	NextAt        string               `json:"next_at"`
	PendingAt     string               `json:"pending_at"`
	RetryAt       string               `json:"retry_at"`
	LastAttemptAt string               `json:"last_attempt_at"`
	LastSuccessAt string               `json:"last_success_at"`
	Outcome       string               `json:"outcome"`
	Message       string               `json:"message"`
	Temporary     string               `json:"temporary"`
	Archives      []ownedBackupArchive `json:"archives"`
}

type backupAutomationStatus struct {
	LocalArchiveCount int                  `json:"local_archive_count"`
	Config            backupScheduleConfig `json:"config"`
	State             string               `json:"state"`
	Message           string               `json:"message"`
	Configured        bool                 `json:"configured"`
	NextAt            string               `json:"next_at,omitempty"`
	RetryAt           string               `json:"retry_at,omitempty"`
	LastAttemptAt     string               `json:"last_attempt_at,omitempty"`
	LastSuccessAt     string               `json:"last_success_at,omitempty"`
}

type backupAutomationDeployment struct{ Directory, SecretFile string }

type backupAutomation struct {
	copyArchive func(io.Writer, io.Reader) (int64, error)
	mu          sync.Mutex
	wg          sync.WaitGroup
	deps        AppDeps
	deployment  backupAutomationDeployment
	export      func(context.Context, backupExportCommand) backupOperationOutcome
	verify      func(context.Context, internalbackup.TemporaryFile, string) (internalbackup.VerifyResult, error)
}

func loadBackupAutomationDeployment() (backupAutomationDeployment, error) {
	d := backupAutomationDeployment{strings.TrimSpace(os.Getenv("DEBIAN_UPDATER_BACKUP_DIR")), strings.TrimSpace(os.Getenv("DEBIAN_UPDATER_BACKUP_PASSPHRASE_FILE"))}
	if (d.Directory == "") != (d.SecretFile == "") {
		return d, errors.New("DEBIAN_UPDATER_BACKUP_DIR and DEBIAN_UPDATER_BACKUP_PASSPHRASE_FILE must be configured together")
	}
	if d.Directory != "" && (!filepath.IsAbs(d.Directory) || !filepath.IsAbs(d.SecretFile)) {
		return d, errors.New("automated backup deployment paths must be absolute")
	}
	return d, nil
}

func newBackupAutomation(deps AppDeps) *backupAutomation {
	d, _ := loadBackupAutomationDeployment() // main validates malformed environment before startup.
	a := &backupAutomation{deps: deps, deployment: d, copyArchive: io.Copy}
	a.export = func(ctx context.Context, cmd backupExportCommand) backupOperationOutcome {
		return newBackupOperationLifecycle(deps).Export(ctx, cmd)
	}
	a.verify = deps.BackupService.VerifyArchiveFile
	return a
}

func defaultBackupSchedule() backupScheduleConfig {
	return backupScheduleConfig{Cadence: "daily", Time: "02:00", Weekday: 0, IncludeKnownHosts: true, Retain: 7}
}
func (c backupScheduleConfig) validate() error {
	if c.Cadence != "daily" && c.Cadence != "weekly" {
		return errors.New("cadence must be daily or weekly")
	}
	if _, err := time.Parse("15:04", c.Time); err != nil || len(c.Time) != 5 {
		return errors.New("time must be HH:MM")
	}
	if c.Weekday < 0 || c.Weekday > 6 {
		return errors.New("weekday must be between 0 (Sunday) and 6")
	}
	if c.Retain < 1 || c.Retain > 100 {
		return errors.New("retain must be between 1 and 100")
	}
	return nil
}

// ResolveLocal chooses the first occurrence of an ambiguous civil time. A missing
// DST time is skipped, so the displayed next instant is always actually runnable.
func nextBackupOccurrence(c backupScheduleConfig, after time.Time, interpretation apptimepkg.Interpretation) time.Time {
	parsed, err := time.Parse("15:04", c.Time)
	if err != nil {
		return time.Time{}
	}
	loc := interpretation.Location
	if loc == nil {
		loc = time.UTC
	}
	local := after.In(loc)
	day := time.Date(local.Year(), local.Month(), local.Day(), 12, 0, 0, 0, loc)
	for n := 0; n < 15; n++ {
		candidateDay := day.AddDate(0, 0, n)
		if c.Cadence == "weekly" && int(candidateDay.Weekday()) != c.Weekday {
			continue
		}
		occurrence := interpretation.ResolveLocal(candidateDay, parsed.Hour(), parsed.Minute())
		if occurrence.Kind != apptimepkg.OccurrenceNonexistent && occurrence.Instant.After(after) {
			return occurrence.Instant.UTC()
		}
	}
	return time.Time{}
}

func (a *backupAutomation) load(ctx context.Context) (backupAutomationState, error) {
	s := backupAutomationState{Config: defaultBackupSchedule(), Outcome: "disabled", Message: "Automated backups are disabled."}
	var raw string
	err := a.deps.DB().QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", backupAutomationSettingsKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	err = json.Unmarshal([]byte(raw), &s)
	if err == nil {
		err = s.Config.validate()
	}
	return s, err
}
func (a *backupAutomation) save(ctx context.Context, s backupAutomationState) error {
	raw, err := json.Marshal(s)
	if err != nil {
		return err
	}
	_, err = a.deps.DB().ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", backupAutomationSettingsKey, string(raw))
	return err
}
func backupStamp(t time.Time) string        { return t.UTC().Format(time.RFC3339Nano) }
func parseBackupStamp(raw string) time.Time { t, _ := time.Parse(time.RFC3339Nano, raw); return t }
func (a *backupAutomation) now() time.Time  { return a.deps.Now().UTC() }
func (a *backupAutomation) next(c backupScheduleConfig, after time.Time) string {
	return backupStamp(nextBackupOccurrence(c, after, a.deps.ApplicationTime.Current()))
}

// Reject symlinks at every existing component, not just the final file. The
// archive directory is subsequently pinned with os.Root for file operations.
func backupRealPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	if filepath.Clean(resolved) != filepath.Clean(absolute) {
		return "", errors.New("backup paths must not contain symbolic links")
	}
	return resolved, nil
}
func pathWithin(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (a *backupAutomation) localIdentity(create bool) (string, error) {
	path := a.deps.DBPath() + ".backup-instance"
	if _, err := backupRealPath(filepath.Dir(path)); err != nil {
		return "", errors.New("local backup identity directory is unavailable")
	}
	if create {
		token := make([]byte, 24)
		if _, err := rand.Read(token); err != nil {
			return "", err
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err == nil {
			_, err = f.WriteString(hex.EncodeToString(token))
			if err == nil {
				err = f.Sync()
			}
			closeErr := f.Close()
			if err != nil {
				return "", err
			}
			if closeErr != nil {
				return "", closeErr
			}
		} else if !os.IsExist(err) {
			return "", err
		}
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return "", errors.New("local backup identity is unavailable or not private")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(data) != 48 {
		return "", errors.New("invalid local backup identity")
	}
	if _, err = hex.DecodeString(string(data)); err != nil {
		return "", errors.New("invalid local backup identity")
	}
	return string(data), nil
}

func (a *backupAutomation) destination(createIdentity bool) (*os.Root, string, string, string, error) {
	fail := func(message string) (*os.Root, string, string, string, error) {
		return nil, "", "", "", errors.New(message)
	}
	if a.deployment.Directory == "" || a.deployment.SecretFile == "" {
		return fail("Configure the backup directory and passphrase file at deployment first.")
	}
	dir, err := backupRealPath(a.deployment.Directory)
	if err != nil {
		return fail("Backup destination is unavailable or contains a symbolic link.")
	}
	secretPath, err := backupRealPath(a.deployment.SecretFile)
	if err != nil {
		return fail("Backup secret is unavailable or contains a symbolic link.")
	}
	paths := a.deps.BackupService.Status()
	secretInfo, err := os.Stat(secretPath)
	if err != nil {
		return fail("Backup secret cannot be inspected.")
	}
	for _, p := range []string{paths.DBPath, paths.ConfigPath, paths.KnownHostsPath, a.deps.DBPath() + ".backup-instance"} {
		if p == "" {
			continue
		}
		parent, err := backupRealPath(filepath.Dir(p))
		if err != nil {
			return fail("Active data directory cannot be validated.")
		}
		if pathWithin(dir, parent) || pathWithin(parent, dir) {
			return fail("Backup destination must be separate from active application data directories.")
		}
		absolute, _ := filepath.Abs(p)
		activeInfo, activeErr := os.Stat(absolute)
		if secretPath == absolute || (activeErr == nil && os.SameFile(secretInfo, activeInfo)) {
			return fail("Backup secret must be separate from application data files.")
		}
	}
	if pathWithin(dir, secretPath) {
		return fail("Backup secret must be outside the archive directory.")
	}
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0022 != 0 {
		return fail("Backup destination must be a directory without group or world write access.")
	}
	info, err = os.Stat(secretPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4096 || info.Mode().Perm()&0077 != 0 {
		return fail("Backup secret must be a private regular file of at most 4096 bytes.")
	}
	f, err := os.Open(secretPath)
	if err != nil {
		return fail("Backup secret cannot be read.")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	_ = f.Close()
	if err != nil || len(data) > 4096 {
		return fail("Backup secret cannot be read.")
	}
	pass := strings.TrimSpace(string(data))
	if validateBackupPassphrase(pass) != nil {
		return fail("Backup secret must contain a passphrase of at least 12 characters.")
	}
	owner, err := a.localIdentity(createIdentity)
	if err != nil {
		return fail("Local backup identity requires revalidation.")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return fail("Backup destination cannot be opened.")
	}
	if createIdentity {
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			_ = root.Close()
			return fail("Backup destination validation could not start.")
		}
		name := ".slu-validation-" + hex.EncodeToString(token)
		probe, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			_ = root.Close()
			return fail("Backup destination is not writable.")
		}
		syncErr := probe.Sync()
		closeErr := probe.Close()
		removeErr := root.Remove(name)
		if syncErr != nil || closeErr != nil || removeErr != nil {
			_ = root.Close()
			return fail("Backup destination write validation or cleanup failed.")
		}
	}
	// Approval binds public deployment coordinates, not passphrase contents. NUL
	// cannot occur in a filesystem path, so the tuple has unambiguous boundaries.
	binding := owner + "\x00" + dir + "\x00" + secretPath
	return root, pass, owner, binding, nil
}

func (a *backupAutomation) statusLocked(ctx context.Context) (backupAutomationStatus, error) {
	s, err := a.load(ctx)
	if err != nil {
		return backupAutomationStatus{}, err
	}
	status := backupAutomationStatus{Config: s.Config, State: s.Outcome, Message: s.Message, LastAttemptAt: s.LastAttemptAt, LastSuccessAt: s.LastSuccessAt, RetryAt: s.RetryAt}
	root, _, owner, binding, validationErr := a.destination(false)
	if root != nil {
		for _, archive := range s.Archives {
			if archive.Owner != owner || filepath.Base(archive.Name) != archive.Name || !strings.HasPrefix(archive.Name, "slu-auto-"+owner+"-") {
				continue
			}
			if info, err := root.Lstat(archive.Name); err == nil && info.Mode().IsRegular() {
				status.LocalArchiveCount++
			}
		}
		_ = root.Close()
	}
	status.Configured = validationErr == nil && s.Approval == binding
	if !s.Config.Enabled {
		if s.PendingAt != "" {
			status.NextAt = s.RetryAt
		} else if s.Outcome == "configured" || s.Outcome == "disabled" {
			status.State = "disabled"
			status.Message = "Automated backups are disabled."
		}
	} else if !status.Configured {
		status.State = "suspended"
		status.Message = "Validate the local destination and secret to resume automated backups."
		if validationErr != nil {
			status.Message = validationErr.Error()
		}
	} else {
		status.NextAt = s.NextAt
		if s.Timezone != a.deps.ApplicationTime.Current().DisplayName {
			status.NextAt = a.next(s.Config, a.now())
		}
		if s.PendingAt != "" {
			status.NextAt = s.RetryAt
		}
	}
	return status, nil
}
func (a *backupAutomation) Status(ctx context.Context) (backupAutomationStatus, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.statusLocked(ctx)
}
func (a *backupAutomation) Configure(ctx context.Context, c backupScheduleConfig) (backupAutomationStatus, error) {
	if err := c.validate(); err != nil {
		return backupAutomationStatus{}, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.load(ctx)
	if err != nil {
		return backupAutomationStatus{}, err
	}
	root, _, _, binding, validationErr := a.destination(true)
	if root != nil {
		defer root.Close()
	}
	if validationErr != nil && c.Enabled {
		return backupAutomationStatus{}, validationErr
	}
	if validationErr == nil {
		s.Approval = binding
	} else {
		s.Approval = ""
	}
	s.Config = c
	s.NextAt = a.next(c, a.now())
	s.Timezone = a.deps.ApplicationTime.Current().DisplayName
	s.PendingAt = ""
	s.PendingManual = false
	s.RetryAt = ""
	s.Outcome = "configured"
	s.Message = "Local backup destination and secret validated."
	if err = a.save(ctx, s); err != nil {
		return backupAutomationStatus{}, err
	}
	return a.statusLocked(ctx)
}
func (a *backupAutomation) SuspendAfterRestore(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.load(ctx)
	if err != nil {
		return err
	}
	s.Approval = ""
	s.PendingAt = ""
	s.PendingManual = false
	s.RetryAt = ""
	s.Temporary = ""
	s.Outcome = "suspended"
	s.Message = "Restored settings require local backup destination and secret revalidation."
	return a.save(ctx, s)
}
func (a *backupAutomation) record(s backupAutomationState, action, status string) {
	if a.deps.AuditService != nil {
		if err := a.deps.AuditService.Record("backup-scheduler", "", action, "backup", "automatic", status, s.Message, map[string]any{"occurrence_at": s.OccurrenceAt, "last_attempt_at": s.LastAttemptAt, "next_at": s.NextAt, "retry_at": s.RetryAt}); err != nil {
			log.Printf("automated backup audit unavailable: %v", err)
		}
	}
}

// Tick claims a single occurrence durably before exporting. A crash consumes that
// occurrence; the next tick reports interruption instead of exporting it twice.
func (a *backupAutomation) Tick(ctx context.Context, manual bool) error {
	lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupExport)
	if !decision.Allowed {
		shared, allowed := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
		if !allowed.Allowed {
			return errors.New("backup deferred while exclusive maintenance is active")
		}
		defer shared.Close()
		a.mu.Lock()
		defer a.mu.Unlock()
		s, err := a.load(ctx)
		if err != nil {
			return err
		}
		if err := a.recoverInterrupted(ctx, &s); err != nil {
			return err
		}
		if !manual && ((!s.Config.Enabled && !s.PendingManual) || s.Approval == "") {
			return nil
		}
		if manual {
			root, _, _, binding, err := a.destination(false)
			if root != nil {
				_ = root.Close()
			}
			if err != nil || binding != s.Approval {
				return errors.New("validate the local backup destination and secret before running a control backup")
			}
		}
		now := a.now()
		if err := a.rebaseTimezone(ctx, &s, now); err != nil {
			return err
		}
		if !manual && now.Before(parseBackupStamp(s.NextAt)) && s.PendingAt == "" {
			return nil
		}
		return a.deferLocked(ctx, &s, now, manual)
	}
	defer lease.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, err := a.load(ctx)
	if err != nil {
		return err
	}
	now := a.now()
	if err := a.rebaseTimezone(ctx, &s, now); err != nil {
		return err
	}
	if s.Temporary != "" {
		root, _, owner, binding, err := a.destination(false)
		if err == nil {
			if s.Approval == binding {
				err = a.cleanupTemporary(root, &s, owner)
			}
			_ = root.Close()
			if err != nil {
				return err
			}
			if err = a.save(ctx, s); err != nil {
				return err
			}
		}
	}
	if err := a.recoverInterrupted(ctx, &s); err != nil {
		return err
	}
	if !manual && ((!s.Config.Enabled && !s.PendingManual) || s.Approval == "") {
		return nil
	}
	if !manual && s.PendingAt == "" && now.Before(parseBackupStamp(s.NextAt)) {
		return nil
	}
	if !manual && s.PendingAt != "" && now.Before(parseBackupStamp(s.RetryAt)) {
		return nil
	}
	root, pass, owner, binding, err := a.destination(false)
	if err != nil || binding != s.Approval {
		s.OccurrenceAt = s.PendingAt
		if s.OccurrenceAt == "" {
			s.OccurrenceAt = s.NextAt
		}
		if manual {
			s.OccurrenceAt = backupStamp(now)
		}
		s.LastAttemptAt = backupStamp(now)
		if root != nil {
			_ = root.Close()
		}
		s.Outcome = "suspended"
		s.Message = "Local backup destination and secret require revalidation."
		if err != nil {
			s.Message = err.Error()
		}
		s.Approval = ""
		_ = a.save(ctx, s)
		a.record(s, "backup.automatic.failed", "failure")
		return errors.New(s.Message)
	}
	defer root.Close()
	if err = a.cleanupTemporary(root, &s, owner); err != nil {
		return err
	}
	if s.PendingAt != "" && now.Sub(parseBackupStamp(s.PendingAt)) >= backupDeferralWindow {
		s.OccurrenceAt = s.PendingAt
		s.LastAttemptAt = backupStamp(now)
		s.PendingAt = ""
		s.PendingManual = false
		s.RetryAt = ""
		s.NextAt = a.next(s.Config, now)
		s.Outcome = "failed"
		s.Message = "Backup deferral expired after one hour; no backlog will be replayed."
		if err = a.save(ctx, s); err != nil {
			return err
		}
		a.record(s, "backup.automatic.failed", "failure")
		return nil
	}
	if !manual && s.PendingAt == "" && now.Sub(parseBackupStamp(s.NextAt)) >= backupDeferralWindow {
		s.OccurrenceAt = s.NextAt
		s.LastAttemptAt = backupStamp(now)
		s.NextAt = a.next(s.Config, now)
		s.Outcome = "failed"
		s.Message = "Missed backup occurrence expired; the next future occurrence is scheduled."
		if err = a.save(ctx, s); err != nil {
			return err
		}
		a.record(s, "backup.automatic.failed", "failure")
		return nil
	}
	if len(a.deps.ServerState.ActiveActionNames()) > 0 {
		return a.deferLocked(ctx, &s, now, manual)
	}
	if s.PendingAt == "" {
		s.PendingManual = manual
		s.PendingAt = s.NextAt
		if manual {
			s.PendingAt = backupStamp(now)
		}
	}
	s.NextAt = a.next(s.Config, now)
	s.OccurrenceAt = s.PendingAt
	s.LastAttemptAt = backupStamp(now)
	s.Outcome = "running"
	s.Message = "Creating and verifying an automated backup."
	s.RetryAt = ""
	token := make([]byte, 16)
	if _, err = rand.Read(token); err != nil {
		return err
	}
	name := "slu-auto-" + owner + "-" + hex.EncodeToString(token) + internalbackup.FileExtension
	s.Temporary = name + ".tmp"
	if err = a.save(ctx, s); err != nil {
		return err
	}
	published := false
	outcome := a.export(ctx, backupExportCommand{Actor: "backup-scheduler", Request: backupExportRequest{Passphrase: pass, IncludeKnownHosts: &s.Config.IncludeKnownHosts}, Lease: lease, HoldLease: true, Finalize: func(ctx context.Context, file internalbackup.TemporaryFile) error {
		archive, err := a.publish(ctx, root, file, pass, owner, name, s.Temporary)
		if err != nil {
			return err
		}
		published = true
		s.Temporary = ""
		s.Archives = append(s.Archives, archive)
		// Persist ownership before retention, so interrupted publication never grants
		// authority to delete an unregistered file discovered in the directory.
		if err = a.save(ctx, s); err != nil {
			return errors.New("published archive retained, but ownership could not be recorded")
		}
		a.recordVerifiedPublication(archive, file.Size)
		if err = a.retain(root, &s); err != nil {
			return err
		}
		return nil
	}})
	if outcome.ExportFile.Path != "" {
		_ = outcome.ExportFile.Remove()
	}
	if outcome.Kind == backupOperationSucceeded {
		s.Outcome = "succeeded"
		s.Message = "Verified encrypted backup published."
		s.LastSuccessAt = backupStamp(a.now())
	} else {
		s.Outcome = "failed"
		s.Message = outcome.PublicError
		if outcome.Err != nil && published {
			s.Message = "Verified archive published, but retention or completion failed; check destination permissions before the next run."
		}
		if s.Message == "" {
			s.Message = "Automated backup failed."
		}
	}
	if err = a.cleanupTemporary(root, &s, owner); err != nil {
		s.Outcome = "failed"
		s.Message = "Backup temporary file cleanup failed; retry after checking destination permissions."
	}
	s.PendingAt = ""
	s.PendingManual = false
	s.RetryAt = ""
	if err = a.save(context.WithoutCancel(ctx), s); err != nil {
		return err
	}
	if s.Outcome == "succeeded" {
		a.record(s, "backup.automatic.succeeded", "success")
	} else {
		a.record(s, "backup.automatic.failed", "failure")
	}
	if err = lease.Close(); err != nil {
		s.Outcome = "failed"
		s.Message = "Backup completed but maintenance mode could not be cleared."
		// Close releases admission even when clearing persistent maintenance fails.
		// Only write completion evidence if shared admission can be recovered.
		if shared, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled); decision.Allowed {
			defer shared.Close()
			_ = a.save(context.WithoutCancel(ctx), s)
			a.record(s, "backup.automatic.failed", "failure")
		}
	}
	if s.Outcome != "succeeded" {
		return errors.New(s.Message)
	}
	return nil
}

func (a *backupAutomation) recoverInterrupted(ctx context.Context, s *backupAutomationState) error {
	if s.Outcome != "running" {
		return nil
	}
	s.Outcome = "failed"
	s.Message = "The previous automated backup was interrupted; its occurrence will not be replayed."
	s.PendingAt = ""
	s.PendingManual = false
	s.RetryAt = ""
	if err := a.save(ctx, *s); err != nil {
		return err
	}
	a.record(*s, "backup.automatic.failed", "failure")
	return nil
}

func (a *backupAutomation) deferLocked(ctx context.Context, s *backupAutomationState, now time.Time, manual bool) error {
	if s.PendingAt != "" && now.Before(parseBackupStamp(s.RetryAt)) {
		return nil
	}
	if s.PendingAt == "" {
		s.PendingManual = manual
		s.PendingAt = s.NextAt
		if manual {
			s.PendingAt = backupStamp(now)
		}
		s.NextAt = a.next(s.Config, now)
	}
	s.OccurrenceAt = s.PendingAt
	if now.Sub(parseBackupStamp(s.PendingAt)) >= backupDeferralWindow {
		s.PendingAt = ""
		s.PendingManual = false
		s.RetryAt = ""
		s.Outcome = "failed"
		s.Message = "Backup deferral expired after one hour; active work was not interrupted."
		s.NextAt = a.next(s.Config, now)
	} else {
		s.Outcome = "deferred"
		s.Message = "Backup deferred for active work; retrying in five minutes for at most one hour."
		s.RetryAt = backupStamp(now.Add(backupRetryDelay))
	}
	s.LastAttemptAt = backupStamp(now)
	if err := a.save(ctx, *s); err != nil {
		return err
	}
	if s.Outcome == "deferred" {
		a.record(*s, "backup.automatic.deferred", "skipped")
	} else {
		a.record(*s, "backup.automatic.failed", "failure")
	}
	return nil
}

func (a *backupAutomation) cleanupTemporary(root *os.Root, s *backupAutomationState, owner string) error {
	name := s.Temporary
	if name == "" {
		return nil
	}
	if filepath.Base(name) != name || !strings.HasPrefix(name, "slu-auto-"+owner+"-") || !strings.HasSuffix(name, ".slubkp.tmp") {
		s.Temporary = ""
		return nil
	}
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		s.Temporary = ""
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("owned backup temporary path is not a regular file")
	}
	if err = root.Remove(name); err != nil {
		return err
	}
	s.Temporary = ""
	return nil
}

func (a *backupAutomation) publish(ctx context.Context, root *os.Root, source internalbackup.TemporaryFile, pass, owner, name, temp string) (ownedBackupArchive, error) {
	archive := ownedBackupArchive{Name: name, Owner: owner}
	src, err := os.Open(source.Path)
	if err != nil {
		return archive, errors.New("backup export file cannot be read")
	}
	defer src.Close()
	dst, err := root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return archive, errors.New("private backup temporary file cannot be created")
	}
	digest := sha256.New()
	written, err := a.copyArchive(io.MultiWriter(dst, digest), io.LimitReader(src, internalbackup.MaxUploadBytes+1))
	if err == nil && written > internalbackup.MaxUploadBytes {
		err = errors.New("backup exceeds archive size limit")
	}
	if err == nil {
		err = dst.Sync()
	}
	closeErr := dst.Close()
	if err != nil || closeErr != nil {
		return archive, errors.New("backup destination write or sync failed")
	}
	if err = ctx.Err(); err != nil {
		return archive, err
	}
	result, err := a.verify(ctx, internalbackup.TemporaryFile{Path: filepath.Join(root.Name(), temp), Size: written}, pass)
	if err != nil || !result.RestoreReady {
		return archive, errors.New("automated archive did not pass restore readiness verification")
	}
	if err = ctx.Err(); err != nil {
		return archive, err
	}
	// Link provides atomic no-replace publication on the same filesystem. Unlike
	// Rename it cannot overwrite a foreign file that already has the target name.
	if err = root.Link(temp, name); err != nil {
		return archive, errors.New("verified backup could not be published atomically")
	}
	if err = root.Remove(temp); err != nil {
		return archive, errors.New("published backup temporary link could not be removed")
	}
	dir, err := root.Open(".")
	if err != nil {
		return archive, errors.New("backup directory cannot be synced")
	}
	err = dir.Sync()
	_ = dir.Close()
	if err != nil {
		return archive, errors.New("published backup directory sync failed")
	}
	archive.SHA256 = hex.EncodeToString(digest.Sum(nil))
	return archive, nil
}

func backupArchiveMatches(root *os.Root, archive ownedBackupArchive) bool {
	if filepath.Base(archive.Name) != archive.Name || !strings.HasPrefix(archive.Name, "slu-auto-"+archive.Owner+"-") || !strings.HasSuffix(archive.Name, ".slubkp") {
		return false
	}
	info, err := root.Lstat(archive.Name)
	if err != nil || !info.Mode().IsRegular() || info.Size() > internalbackup.MaxUploadBytes {
		return false
	}
	f, err := root.Open(archive.Name)
	if err != nil {
		return false
	}
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, io.LimitReader(f, internalbackup.MaxUploadBytes+1))
	return err == nil && hex.EncodeToString(h.Sum(nil)) == archive.SHA256
}
func (a *backupAutomation) retain(root *os.Root, s *backupAutomationState) error {
	owner, err := a.localIdentity(false)
	if err != nil {
		return err
	}
	keep := s.Config.Retain
	if keep < 1 {
		keep = 1
	}
	valid := 0
	retained := make([]ownedBackupArchive, 0, len(s.Archives))
	for i := len(s.Archives) - 1; i >= 0; i-- {
		archive := s.Archives[i]
		if archive.Owner != owner || !backupArchiveMatches(root, archive) {
			continue
		}
		valid++
		if valid > keep {
			if err = root.Remove(archive.Name); err != nil {
				return errors.New("verified backup published, but an owned archive could not be removed by retention")
			}
		} else {
			retained = append(retained, archive)
		}
	}
	for i, j := 0, len(retained)-1; i < j; i, j = i+1, j-1 {
		retained[i], retained[j] = retained[j], retained[i]
	}
	s.Archives = retained
	return nil
}

func (a *backupAutomation) Start(ctx context.Context) {
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			if err := a.Tick(ctx, false); err != nil && ctx.Err() == nil {
				log.Printf("automated backup: %v", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}
func (a *backupAutomation) Wait() { a.wg.Wait() }

func handleBackupAutomationStatus(c *gin.Context, a *backupAutomation) {
	status, err := a.Status(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Automated backup state is unavailable."})
		return
	}
	c.JSON(http.StatusOK, status)
}
func handleBackupAutomationConfigure(c *gin.Context, a *backupAutomation) {
	var config backupScheduleConfig
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid backup schedule configuration."})
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid backup schedule configuration."})
		return
	}
	status, err := a.Configure(c.Request.Context(), config)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	audit(c, "backup.automatic.configured", "backup", "automatic", "success", "Automated backup settings saved", map[string]any{"enabled": config.Enabled})
	c.JSON(http.StatusOK, status)
}
func handleBackupAutomationRun(c *gin.Context, a *backupAutomation) {
	// Authentication runs under shared admission. Release that lease before Tick
	// tries exclusive admission, otherwise every control run defers itself.
	if value, ok := c.Get(maintenanceSharedLeaseContextKey); ok {
		if lease, ok := value.(*maintenancepkg.SharedLease); ok {
			lease.Close()
		}
	}
	if err := a.Tick(c.Request.Context(), true); err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	lease, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		writeMaintenanceBlockedSnapshotResponseWithTime(c, decision.State, a.deps.ApplicationTime)
		return
	}
	defer lease.Close()
	handleBackupAutomationStatus(c, a)
}

func (a *backupAutomation) rebaseTimezone(ctx context.Context, s *backupAutomationState, now time.Time) error {
	timezone := a.deps.ApplicationTime.Current().DisplayName
	if s.Timezone == timezone {
		return nil
	}
	s.Timezone = timezone
	// Keep an already deferred occurrence; only future civil scheduling is rebased.
	s.NextAt = a.next(s.Config, now)
	return a.save(ctx, *s)
}
func (a *backupAutomation) recordVerifiedPublication(archive ownedBackupArchive, size int64) {
	if a.deps.AuditService == nil {
		return
	}
	if err := a.deps.AuditService.Record("backup-scheduler", "", "backup.verify", "backup", "automatic", "success", "Published automated backup passed restore readiness verification", map[string]any{"restore_ready": true, "archive_size_bytes": size, "archive": archive.Name}); err != nil {
		log.Printf("automated backup verification audit unavailable: %v", err)
	}
}
