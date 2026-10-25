package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	auditpkg "debian-updater/internal/audit"
	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
)

func newBackupAutomationTest(t *testing.T) (*testApp, *backupAutomation, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "app.db"), Deps: AppDeps{Now: func() time.Time { return now }}})
	a := app.Deps.BackupAutomation
	a.deployment = backupAutomationDeployment{Directory: t.TempDir(), SecretFile: filepath.Join(t.TempDir(), "passphrase")}
	if err := os.Chmod(a.deployment.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.deployment.SecretFile, []byte("a separate automated backup secret\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.deps.ApplicationTime.Configure(context.Background(), "UTC"); err != nil {
		t.Fatal(err)
	}
	config := defaultBackupSchedule()
	config.Enabled = true
	config.Retain = 2
	if _, err := a.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	return app, a, &now
}
func backupAutomationStateForTest(t *testing.T, a *backupAutomation) backupAutomationState {
	t.Helper()
	s, err := a.load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestBackupAutomationCivilScheduleDST(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Paris")
	if err != nil {
		t.Fatal(err)
	}
	interpretation := apptimepkg.Interpretation{Location: loc}
	for _, tc := range []struct {
		name, after, want string
		cadence           string
		weekday           int
	}{
		{"spring gap skipped", "2026-03-28T03:00:00Z", "2026-03-30T00:30:00Z", "daily", 0},
		{"fall first occurrence", "2026-10-24T03:00:00Z", "2026-10-25T00:30:00Z", "daily", 0},
		{"fall repeated hour not replayed", "2026-10-25T00:30:00Z", "2026-10-26T01:30:00Z", "daily", 0},
		{"weekly gap skipped", "2026-03-28T03:00:00Z", "2026-04-05T00:30:00Z", "weekly", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := defaultBackupSchedule()
			c.Time = "02:30"
			c.Cadence = tc.cadence
			c.Weekday = tc.weekday
			got := nextBackupOccurrence(c, parseBackupStamp(tc.after), interpretation)
			if backupStamp(got) != tc.want {
				t.Fatalf("next=%s want=%s", got, tc.want)
			}
		})
	}
}

func TestBackupAutomationVerifiedPublicationRetentionAndHealth(t *testing.T) {
	app, a, now := newBackupAutomationTest(t)
	cookie := app.authenticate(t)
	foreign := filepath.Join(a.deployment.Directory, "manual.slubkp")
	if err := os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 3; n++ {
		s := backupAutomationStateForTest(t, a)
		*now = parseBackupStamp(s.NextAt)
		if err := a.Tick(context.Background(), false); err != nil {
			t.Fatal(err)
		}
	}
	s := backupAutomationStateForTest(t, a)
	if s.Outcome != "succeeded" || s.LastSuccessAt == "" || len(s.Archives) != 2 {
		t.Fatalf("unexpected result: %+v", s)
	}
	entries, err := os.ReadDir(a.deployment.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d want two owned and foreign", len(entries))
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
		t.Fatal("foreign archive modified")
	}
	for _, archive := range s.Archives {
		info, err := os.Stat(filepath.Join(a.deployment.Directory, archive.Name))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("archive is not private")
		}
	}
	health, err := app.Deps.BackupService.RecoveryHealth(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if health.State != internalbackup.RecoveryStateHealthy {
		t.Fatalf("recovery health=%+v", health)
	}
	var metadata string
	if err := app.Deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action='backup.automatic.succeeded' ORDER BY id DESC LIMIT 1").Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(metadata, s.OccurrenceAt) || s.OccurrenceAt == "" {
		t.Fatalf("missing occurrence audit: %s", metadata)
	}
	rec := performContractRequest(app.Handler, http.MethodGet, "/api/backup/status", nil, cookie, false)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"scheduled":true`) {
		t.Fatalf("status: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "separate automated backup secret") {
		t.Fatal("secret exposed")
	}
	// A new runtime instance and a second tick cannot replay the consumed occurrence.
	restarted := newBackupAutomation(app.Deps)
	restarted.deployment = a.deployment
	if err := restarted.Tick(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	after := backupAutomationStateForTest(t, a)
	if after.LastAttemptAt != s.LastAttemptAt || len(after.Archives) != 2 {
		t.Fatal("occurrence replayed")
	}
}

func TestBackupAutomationDeferralRestartExpiryAndTimezone(t *testing.T) {
	_, a, now := newBackupAutomationTest(t)
	ctx := context.Background()
	s := backupAutomationStateForTest(t, a)
	*now = parseBackupStamp(s.NextAt)
	shared, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		t.Fatal("shared denied")
	}
	if err := a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = backupAutomationStateForTest(t, a)
	if s.Outcome != "deferred" || s.RetryAt == "" {
		t.Fatalf("not deferred: %+v", s)
	}
	firstOccurrence := s.PendingAt
	*now = now.Add(61 * time.Minute)
	if err := a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	shared.Close()
	s = backupAutomationStateForTest(t, a)
	if s.PendingAt != "" || s.Outcome != "failed" || len(s.Archives) != 0 {
		t.Fatalf("unbounded retry: %+v", s)
	}
	// A claimed but interrupted run is terminal, its owned temporary is cleaned.
	owner, err := a.localIdentity(false)
	if err != nil {
		t.Fatal(err)
	}
	s.Outcome = "running"
	s.PendingAt = firstOccurrence
	s.Temporary = "slu-auto-" + owner + "-interrupted.slubkp.tmp"
	temp := filepath.Join(a.deployment.Directory, s.Temporary)
	if err = os.WriteFile(temp, []byte("partial"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = backupAutomationStateForTest(t, a)
	if s.Outcome != "failed" || s.PendingAt != "" {
		t.Fatalf("interrupted result=%+v", s)
	}
	if _, err = os.Stat(temp); !os.IsNotExist(err) {
		t.Fatal("abandoned temporary remains")
	}
	if _, err = a.deps.ApplicationTime.Configure(ctx, "America/Toronto"); err != nil {
		t.Fatal(err)
	}
	preview, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if preview.NextAt == s.NextAt {
		t.Fatal("timezone did not rebase next occurrence")
	}
	if err = a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = backupAutomationStateForTest(t, a)
	if s.NextAt != preview.NextAt {
		t.Fatal("displayed next time differs from persisted schedule")
	}
}

func TestBackupAutomationFailuresPreserveLastArchive(t *testing.T) {
	_, a, now := newBackupAutomationTest(t)
	ctx := context.Background()
	s := backupAutomationStateForTest(t, a)
	*now = parseBackupStamp(s.NextAt)
	if err := a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	s = backupAutomationStateForTest(t, a)
	original := s.Archives[0]
	path := filepath.Join(a.deployment.Directory, original.Name)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	realVerify := a.verify
	a.verify = func(context.Context, internalbackup.TemporaryFile, string) (internalbackup.VerifyResult, error) {
		return internalbackup.VerifyResult{}, errors.New("injected verification failure")
	}
	*now = parseBackupStamp(s.NextAt)
	if err = a.Tick(ctx, false); err == nil {
		t.Fatal("verification failure accepted")
	}
	s = backupAutomationStateForTest(t, a)
	if s.Outcome != "failed" || len(s.Archives) != 1 {
		t.Fatalf("failure applied retention: %+v", s)
	}
	a.verify = realVerify
	// Replace the directory with a file: publication cannot proceed and must not
	// touch the existing archive moved aside with its original directory.
	savedDir := a.deployment.Directory + "-saved"
	if err = os.Rename(a.deployment.Directory, savedDir); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(a.deployment.Directory, []byte("not directory"), 0600); err != nil {
		t.Fatal(err)
	}
	*now = parseBackupStamp(s.NextAt)
	if err = a.Tick(ctx, false); err == nil {
		t.Fatal("invalid destination accepted")
	}
	if err = os.Remove(a.deployment.Directory); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(savedDir, a.deployment.Directory); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(path); err != nil || !bytes.Equal(before, data) {
		t.Fatal("last valid backup changed")
	}
	entries, err := os.ReadDir(a.deployment.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("failed temporary not cleaned: %v", entries)
	}
}

func TestBackupAutomationDeploymentRejectsUnsafePaths(t *testing.T) {
	_, a, _ := newBackupAutomationTest(t)
	original := a.deployment
	for _, name := range []string{"secret symlink", "destination symlink", "secret in archives", "active directory", "public secret", "short secret", "writable directory"} {
		t.Run(name, func(t *testing.T) {
			a.deployment = original
			defer func() { a.deployment = original }()
			switch name {
			case "secret symlink":
				a.deployment.SecretFile = filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(original.SecretFile, a.deployment.SecretFile); err != nil {
					t.Fatal(err)
				}
			case "destination symlink":
				a.deployment.Directory = filepath.Join(t.TempDir(), "link")
				if err := os.Symlink(original.Directory, a.deployment.Directory); err != nil {
					t.Fatal(err)
				}
			case "secret in archives":
				a.deployment.SecretFile = filepath.Join(original.Directory, "secret")
				if err := os.WriteFile(a.deployment.SecretFile, []byte("a sufficiently long secret"), 0600); err != nil {
					t.Fatal(err)
				}
				defer os.Remove(a.deployment.SecretFile)
			case "active directory":
				a.deployment.Directory = filepath.Dir(a.deps.DBPath())
			case "public secret":
				if err := os.Chmod(original.SecretFile, 0644); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(original.SecretFile, 0600)
			case "short secret":
				a.deployment.SecretFile = filepath.Join(t.TempDir(), "short")
				if err := os.WriteFile(a.deployment.SecretFile, []byte("short"), 0600); err != nil {
					t.Fatal(err)
				}
			case "writable directory":
				if err := os.Chmod(original.Directory, 0777); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(original.Directory, 0700)
			}
			root, _, _, _, err := a.destination(false)
			if root != nil {
				root.Close()
			}
			if err == nil {
				t.Fatal("unsafe deployment accepted")
			}
		})
	}
}

func TestBackupAutomationRetentionIgnoresForeignAndSymlinkFiles(t *testing.T) {
	_, a, _ := newBackupAutomationTest(t)
	root, _, owner, _, err := a.destination(false)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	s := backupAutomationStateForTest(t, a)
	s.Config.Retain = 1
	foreign := filepath.Join(t.TempDir(), "foreign")
	if err = os.WriteFile(foreign, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"old", "new"} {
		filename := "slu-auto-" + owner + "-" + name + ".slubkp"
		data := []byte(name)
		if err = os.WriteFile(filepath.Join(a.deployment.Directory, filename), data, 0600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		s.Archives = append(s.Archives, ownedBackupArchive{Name: filename, Owner: owner, SHA256: hex.EncodeToString(digest[:])})
	}
	linkName := "slu-auto-" + owner + "-link.slubkp"
	if err = os.Symlink(foreign, filepath.Join(a.deployment.Directory, linkName)); err != nil {
		t.Fatal(err)
	}
	s.Archives = append([]ownedBackupArchive{{Name: linkName, Owner: owner}}, s.Archives...)
	if err = a.retain(root, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Archives) != 1 || !strings.Contains(s.Archives[0].Name, "-new.") {
		t.Fatalf("retained: %+v", s.Archives)
	}
	if data, err := os.ReadFile(foreign); err != nil || string(data) != "foreign" {
		t.Fatal("symlink target changed")
	}
	if _, err = os.Lstat(filepath.Join(a.deployment.Directory, linkName)); err != nil {
		t.Fatal("foreign symlink deleted")
	}
}

func TestBackupAutomationRestoreSuspendsAndRevalidates(t *testing.T) {
	_, a, now := newBackupAutomationTest(t)
	ctx := context.Background()
	s := backupAutomationStateForTest(t, a)
	*now = parseBackupStamp(s.NextAt)
	if err := a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	if err := a.SuspendAfterRestore(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Config.Enabled || status.State != "suspended" || status.Configured {
		t.Fatalf("restored schedule not suspended: %+v", status)
	}
	if err = a.Tick(ctx, false); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Configure(ctx, status.Config); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx)
	if err != nil || !status.Configured || status.State != "configured" {
		t.Fatalf("revalidation failed: %+v %v", status, err)
	}
}

func TestBackupAutomationHTTPValidationAndRunAdmission(t *testing.T) {
	app, a, _ := newBackupAutomationTest(t)
	cookie := app.authenticate(t)
	invalid := performContractRequest(app.Handler, http.MethodPut, "/api/backup/automation", bytes.NewBufferString(`{"directory":"/arbitrary"}`), cookie, true)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("arbitrary path accepted: %d", invalid.Code)
	}
	shared, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		t.Fatal("shared lease denied")
	}
	result := performContractRequest(app.Handler, http.MethodPost, "/api/backup/automation/run", nil, cookie, true)
	shared.Close()
	if result.Code != http.StatusOK {
		t.Fatalf("run busy: %d %s", result.Code, result.Body.String())
	}
	var status backupAutomationStatus
	if err := json.Unmarshal(result.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status.State != "deferred" {
		t.Fatalf("run busy status: %+v", status)
	}
}

func TestBackupAutomationDisabledConfigurationReportsValidationFailure(t *testing.T) {
	app, a, _ := newBackupAutomationTest(t)
	cookie := app.authenticate(t)
	if err := os.Remove(a.deployment.SecretFile); err != nil {
		t.Fatal(err)
	}
	config := defaultBackupSchedule()
	config.Enabled = false
	body, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	result := performContractRequest(app.Handler, http.MethodPut, "/api/backup/automation", bytes.NewBuffer(body), cookie, true)
	if result.Code != http.StatusOK {
		t.Fatalf("disabling invalid deployment: %d %s", result.Code, result.Body.String())
	}
	for _, response := range []*httptest.ResponseRecorder{result, performContractRequest(app.Handler, http.MethodGet, "/api/backup/automation", nil, cookie, false)} {
		var status backupAutomationStatus
		if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status.Config.Enabled || status.Configured || status.State != "disabled" || !strings.Contains(status.Message, "Backup secret is unavailable") {
			t.Fatalf("disabled validation failure was hidden: %+v", status)
		}
	}
	// Repairing the local secret and validating again must clear the diagnostic
	// and permit a control backup while recurring scheduling remains disabled.
	if err := os.WriteFile(a.deployment.SecretFile, []byte("a repaired automated backup secret"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := a.Configure(context.Background(), config)
	if err != nil || status.Config.Enabled || !status.Configured || strings.Contains(status.Message, "failed") {
		t.Fatalf("revalidation did not clear the failure: %+v %v", status, err)
	}
}

func TestBackupAutomationArchiveRestoresOnDistinctInstance(t *testing.T) {
	// The source and destination use the normal global DB reopening adapter so the
	// real restore handoff exercises persistence replacement and runtime reload.
	preserveServerState(t)
	source := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "source.db"), Deps: AppDeps{DB: getDB}})
	source.authenticate(t)
	a := source.Deps.BackupAutomation
	a.deployment = backupAutomationDeployment{Directory: t.TempDir(), SecretFile: filepath.Join(t.TempDir(), "secret")}
	if err := os.Chmod(a.deployment.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(a.deployment.SecretFile, []byte("distinct instance restore secret"), 0600); err != nil {
		t.Fatal(err)
	}
	config := defaultBackupSchedule()
	config.Enabled = true
	if _, err := a.Configure(context.Background(), config); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Deps.DB().Exec("INSERT INTO servers(name,host,port,user,pass_enc,key_enc,key_path,tags) VALUES('backup-only-host','invalid.example',22,'root','','','','')"); err != nil {
		t.Fatal(err)
	}
	if err := a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	s := backupAutomationStateForTest(t, a)
	archivePath := filepath.Join(a.deployment.Directory, s.Archives[0].Name)
	info, err := os.Stat(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	target := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "target.db"), Deps: AppDeps{DB: getDB}})
	target.authenticate(t)
	lease, decision := target.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("restore lease denied")
	}
	defer lease.Close()
	outcome := newBackupOperationLifecycle(target.Deps).Restore(context.Background(), backupRestoreCommand{Actor: "test", File: internalbackup.TemporaryFile{Path: archivePath, Size: info.Size()}, Passphrase: "distinct instance restore secret", Lease: lease})
	if outcome.Kind != backupOperationSucceeded {
		t.Fatalf("real restore: %+v", outcome)
	}
	var count int
	if err = target.Deps.DB().QueryRow("SELECT COUNT(*) FROM servers WHERE name='backup-only-host'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("restored inventory count=%d err=%v", count, err)
	}
	restored := backupAutomationStateForTest(t, target.Deps.BackupAutomation)
	if !restored.Config.Enabled || restored.Approval != "" || restored.Outcome != "suspended" {
		t.Fatalf("restore failed to suspend automation: %+v", restored)
	}
	// Secret file and installation identity are never archive resources.
	verified, err := target.Deps.BackupService.VerifyArchiveFile(context.Background(), internalbackup.TemporaryFile{Path: archivePath, Size: info.Size()}, "distinct instance restore secret")
	if err != nil || !verified.RestoreReady {
		t.Fatalf("archive readiness: %+v %v", verified, err)
	}
	for _, name := range verified.FileNames {
		if strings.Contains(name, "secret") || strings.Contains(name, "backup-instance") {
			t.Fatalf("local-only file archived: %s", name)
		}
	}
}

func TestBackupAutomationMidWriteAndCancellationFailures(t *testing.T) {
	for _, failure := range []string{"full disk", "permission", "cancelled", "not restore ready"} {
		t.Run(failure, func(t *testing.T) {
			_, a, now := newBackupAutomationTest(t)
			s := backupAutomationStateForTest(t, a)
			*now = parseBackupStamp(s.NextAt)
			if err := a.Tick(context.Background(), false); err != nil {
				t.Fatal(err)
			}
			s = backupAutomationStateForTest(t, a)
			previous := s.Archives[0]
			before, err := os.ReadFile(filepath.Join(a.deployment.Directory, previous.Name))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			a.copyArchive = func(dst io.Writer, src io.Reader) (int64, error) {
				if failure == "full disk" || failure == "permission" {
					n, err := io.CopyN(dst, src, 64)
					if err != nil {
						return n, err
					}
					if failure == "full disk" {
						return n, syscall.ENOSPC
					}
					return n, os.ErrPermission
				}
				n, err := io.Copy(dst, src)
				if failure == "cancelled" {
					cancel()
				}
				return n, err
			}
			if failure == "not restore ready" {
				a.verify = func(context.Context, internalbackup.TemporaryFile, string) (internalbackup.VerifyResult, error) {
					return internalbackup.VerifyResult{RestoreReady: false}, nil
				}
			}
			*now = parseBackupStamp(s.NextAt)
			if err = a.Tick(ctx, false); err == nil {
				t.Fatal("injected failure accepted")
			}
			s = backupAutomationStateForTest(t, a)
			if s.Outcome != "failed" || len(s.Archives) != 1 || s.LastSuccessAt == "" {
				t.Fatalf("failure result: %+v", s)
			}
			entries, err := os.ReadDir(a.deployment.Directory)
			if err != nil || len(entries) != 1 {
				t.Fatalf("temporary remained: %v %v", entries, err)
			}
			after, err := os.ReadFile(filepath.Join(a.deployment.Directory, previous.Name))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("valid backup changed after failure")
			}
		})
	}
}

func TestBackupAutomationDisabledControlDeferralEventuallyRuns(t *testing.T) {
	_, a, now := newBackupAutomationTest(t)
	config := defaultBackupSchedule()
	config.Enabled = false
	status, err := a.Configure(context.Background(), config)
	if err != nil || !status.Configured {
		t.Fatalf("disabled schedule validation: %+v %v", status, err)
	}
	shared, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
	if !decision.Allowed {
		t.Fatal("shared denied")
	}
	if err = a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	shared.Close()
	status, err = a.Status(context.Background())
	if err != nil || status.State != "deferred" || status.NextAt == "" {
		t.Fatalf("disabled control not deferred: %+v %v", status, err)
	}
	*now = now.Add(backupRetryDelay)
	if err = a.Tick(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	s := backupAutomationStateForTest(t, a)
	if s.Config.Enabled || s.Outcome != "succeeded" || len(s.Archives) != 1 || s.PendingManual {
		t.Fatalf("deferred control not completed: %+v", s)
	}
}

func TestBackupAutomationLocalArchivesAreNotInferredFromRestoredHistory(t *testing.T) {
	_, a, _ := newBackupAutomationTest(t)
	if err := a.Tick(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(context.Background())
	if err != nil || status.LocalArchiveCount != 1 {
		t.Fatalf("local archive count=%+v %v", status, err)
	}
	// A newly installed identity can validate the same deployment paths but has
	// no ownership of the old installation's files and historical last success.
	if err = os.Remove(a.deps.DBPath() + ".backup-instance"); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Configure(context.Background(), status.Config); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(context.Background())
	if err != nil || status.LastSuccessAt == "" || status.LocalArchiveCount != 0 {
		t.Fatalf("historical success mistaken for local archive: %+v %v", status, err)
	}
}

func TestBackupAutomationInterruptedClaimIsNotDeferredOrReplayed(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "scheduled", true: "disabled control"}[manual], func(t *testing.T) {
			_, a, now := newBackupAutomationTest(t)
			ctx := context.Background()
			s := backupAutomationStateForTest(t, a)
			*now = parseBackupStamp(s.NextAt)
			s.Config.Enabled = !manual
			s.Outcome = "running"
			s.PendingManual = manual
			s.PendingAt = backupStamp(*now)
			s.OccurrenceAt = s.PendingAt
			s.NextAt = a.next(s.Config, *now)
			if err := a.save(ctx, s); err != nil {
				t.Fatal(err)
			}
			restarted := newBackupAutomation(a.deps)
			restarted.deployment = a.deployment
			restarted.export = func(context.Context, backupExportCommand) backupOperationOutcome {
				t.Fatal("replayed an already claimed backup occurrence")
				return backupOperationOutcome{}
			}
			shared, decision := a.deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkInteractive)
			if !decision.Allowed {
				t.Fatal("shared admission denied")
			}
			defer shared.Close()
			if err := restarted.Tick(ctx, false); err != nil {
				t.Fatal(err)
			}
			shared.Close()
			*now = now.Add(backupRetryDelay)
			if err := restarted.Tick(ctx, false); err != nil {
				t.Fatal(err)
			}
			after := backupAutomationStateForTest(t, a)
			if after.Outcome != "failed" || after.PendingAt != "" || after.PendingManual || after.NextAt != s.NextAt {
				t.Fatalf("interrupted claim was not consumed: %+v", after)
			}
			var metadata string
			if err := a.deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action='backup.automatic.failed' ORDER BY id DESC LIMIT 1").Scan(&metadata); err != nil || !strings.Contains(metadata, s.OccurrenceAt) {
				t.Fatalf("interruption audit metadata=%s error=%v", metadata, err)
			}
		})
	}
}

func TestBackupAutomationRunBlocksBeforeAuthenticationDuringRestore(t *testing.T) {
	dbCalls := 0
	app := newTestApp(t, testAppOptions{Deps: AppDeps{DB: func() *sql.DB {
		dbCalls++
		return getDB()
	}}})
	lease, decision := app.Deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("restore admission denied")
	}
	defer lease.Close()
	dbCalls = 0
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/backup/automation/run", nil)
	markSameOriginAuthRequest(req)
	app.Router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable || dbCalls != 0 {
		t.Fatalf("restore admission response=%d database accesses=%d body=%s", rec.Code, dbCalls, rec.Body.String())
	}
}

func TestBackupAutomationControlRunProtectsCompletionAndStatus(t *testing.T) {
	app, a, _ := newBackupAutomationTest(t)
	cookie := app.authenticate(t)
	assertAdmissionHeld := func() {
		t.Helper()
		if lease, decision := a.deps.MaintenanceCoordinator.TryExclusive(maintenancepkg.OperationBackupRestore); decision.Allowed {
			lease.Close()
			t.Error("backup completion accessed persistence without maintenance admission")
		}
	}
	db := a.deps.DB
	a.deps.DB = func() *sql.DB {
		assertAdmissionHeld()
		return db()
	}
	recorded := false
	a.deps.AuditService = auditpkg.NewService(auditpkg.ServiceOptions{DB: db, OnRecord: func(event auditpkg.Event) {
		if event.Action == "backup.automatic.succeeded" {
			recorded = true
			assertAdmissionHeld()
		}
	}})
	result := performContractRequest(app.Handler, http.MethodPost, "/api/backup/automation/run", nil, cookie, true)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"state":"succeeded"`) || !recorded {
		t.Fatalf("control run response=%d recorded=%v body=%s", result.Code, recorded, result.Body.String())
	}
}

func TestBackupAutomationApprovalBindsDeploymentWithoutPassphrase(t *testing.T) {
	_, a, _ := newBackupAutomationTest(t)
	ctx := context.Background()
	s := backupAutomationStateForTest(t, a)
	root, pass, owner, binding, err := a.destination(false)
	if err != nil {
		t.Fatal(err)
	}
	root.Close()
	if binding != owner+"\x00"+a.deployment.Directory+"\x00"+a.deployment.SecretFile || binding != s.Approval {
		t.Fatal("approval does not bind the exact local deployment")
	}
	var raw string
	if err := a.deps.DB().QueryRow("SELECT value FROM settings WHERE key=?", backupAutomationSettingsKey).Scan(&raw); err != nil || strings.Contains(raw, pass) {
		t.Fatalf("passphrase persisted or settings unavailable: %v", err)
	}
	if err := os.WriteFile(a.deployment.SecretFile, []byte("rotated secret for future archives"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(ctx)
	if err != nil || !status.Configured {
		t.Fatalf("in-place rotation should preserve local approval: %+v %v", status, err)
	}
	previousPath := a.deployment.SecretFile
	a.deployment.SecretFile = filepath.Join(t.TempDir(), "other-secret")
	if err := os.WriteFile(a.deployment.SecretFile, []byte("rotated secret for future archives"), 0600); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx)
	if err != nil || status.Configured || status.State != "suspended" {
		t.Fatalf("changed path must require revalidation: %+v %v", status, err)
	}
	a.deployment.SecretFile = previousPath
	// Previous versions persisted a 64-character hex fingerprint.
	s.Approval = strings.Repeat("a", sha256.Size*2)
	if err := a.save(ctx, s); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx)
	if err != nil || status.Configured || status.State != "suspended" {
		t.Fatalf("legacy approval must require revalidation: %+v %v", status, err)
	}
	status, err = a.Configure(ctx, s.Config)
	if err != nil || !status.Configured {
		t.Fatalf("legacy approval revalidation failed: %+v %v", status, err)
	}
}
