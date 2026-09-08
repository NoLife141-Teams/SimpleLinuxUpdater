package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
	serverpkg "debian-updater/internal/servers"
)

func TestSecondReviewApprovalPreservesFullLog(t *testing.T) {
	for _, action := range []string{"approve", "cancel"} {
		t.Run(action, func(t *testing.T) {
			server := Server{Name: "retention", Host: "example.org", Port: 22, User: "root"}
			full := strings.Repeat("metadata output retained for diagnosis\n", 12000)
			preview := full[len(full)-32768:]
			h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval", Logs: preview})
			job := h.createPendingUpdateJob(t, server.Name)
			if err := h.jobManager.Transition(job.ID, JobTransitionIntent{LogsText: &full}); err != nil {
				t.Fatal(err)
			}
			var result serverActionLifecycleResult
			if action == "approve" {
				result = h.lifecycle().ApproveAll(server.Name, approvalIdentityForTest(h.state, server.Name))
			} else {
				result = h.lifecycle().Cancel(server.Name, approvalIdentityForTest(h.state, server.Name))
			}
			if result.statusCode != http.StatusOK {
				t.Fatalf("action failed: %+v", result)
			}
			saved, err := h.jobManager.GetJobWithLogs(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.LogsText != full {
				t.Fatalf("full log replaced: before=%d after=%d truncated=%t", len(full), len(saved.LogsText), saved.LogsTruncated)
			}
		})
	}
}

func TestSecondReviewRebootRechecksReconciliationAtAdmission(t *testing.T) {
	server := Server{Name: "admission", Host: "example.org", Port: 22, User: "root"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "idle"})
	l := h.lifecycle()
	l.maintenanceReadiness = func(Server) serverpkg.MaintenanceReadiness {
		// A concurrent admitted update can finish with an unknown APT outcome while this request checks credentials.
		h.state.RestoreStatusSnapshot(server.Name, &ServerStatus{Name: server.Name, Status: "needs_reconciliation", Logs: "APT outcome unknown"})
		return serverpkg.MaintenanceReadiness{Ready: true}
	}
	result := l.StartReboot(server.Name, "admin", "", true)
	if result.statusCode != http.StatusConflict {
		t.Fatalf("reboot admitted after reconciliation became required: HTTP %d, state=%s, runner=%t", result.statusCode, h.state.CurrentStatusSnapshot(server.Name).Status, h.runnerRun != nil)
	}
}

func TestSecondReviewRollbackClosesRestoredDatabaseBeforeReplacingFiles(t *testing.T) {
	key := []byte("01234567890123456789012345678901")
	original := buildBackupDatabaseDataWithKey(t, key, Server{Name: "original", Host: "old.example", Port: 22, User: "root"}, "")
	replacement := buildBackupDatabaseDataWithKey(t, key, Server{Name: "replacement", Host: "new.example", Port: 22, User: "root"}, "")
	target := filepath.Join(t.TempDir(), "servers.db")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	originalLive, openErr := sql.Open("sqlite", target)
	if openErr != nil {
		t.Fatal(openErr)
	}
	originalLive.SetMaxOpenConns(1)
	if _, err := originalLive.Exec("PRAGMA journal_mode=WAL; UPDATE servers SET name='original-committed'"); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"encryption_key": base64.StdEncoding.EncodeToString(key)})
	var live *sql.DB
	var restoredInfo os.FileInfo
	prepares, reloads := 0, 0
	replacedWhileOpen := false
	forward := errors.New("failure after restored database opened")
	service := NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		DBPath: func() string { return target }, CurrentEncryptionKey: func() []byte { return key }, TempDir: t.TempDir,
		RestoredRuntime: restoredRuntimeAdapter{
			prepare: func() {
				prepares++
				if originalLive != nil {
					if _, err := originalLive.Exec("UPDATE servers SET name='original-quiesced'"); err != nil {
						t.Fatal(err)
					}
					if err := originalLive.Close(); err != nil {
						t.Fatal(err)
					}
					originalLive = nil
				}
				if live != nil {
					info, err := os.Stat(target)
					if err != nil {
						t.Fatal(err)
					}
					replacedWhileOpen = restoredInfo != nil && !os.SameFile(info, restoredInfo)
					if err := live.Close(); err != nil {
						t.Fatal(err)
					}
					live = nil
				}
			},
			reload: func(context.Context) error {
				reloads++
				if reloads == 1 {
					var err error
					restoredInfo, err = os.Stat(target)
					if err != nil {
						return err
					}
					live, err = sql.Open("sqlite", target)
					if err != nil {
						return err
					}
					live.SetMaxOpenConns(1)
					if _, err = live.Exec("PRAGMA journal_mode=WAL; CREATE TABLE restore_write(value TEXT); INSERT INTO restore_write VALUES ('during reload')"); err != nil {
						return err
					}
					return forward
				}
				return nil
			},
		},
	})
	err := service.ApplyFiles(context.Background(), map[string][]byte{"servers.db": replacement, "config.json": config})
	if !errors.Is(err, forward) {
		t.Fatalf("unexpected restore error: %v", err)
	}
	check, openErr := sql.Open("sqlite", target)
	if openErr != nil {
		t.Fatal(openErr)
	}
	defer check.Close()
	var name string
	readErr := check.QueryRow("SELECT name FROM servers").Scan(&name)
	t.Logf("post-rollback database: server=%q readError=%v", name, readErr)
	if replacedWhileOpen || name != "original-quiesced" || readErr != nil {
		t.Fatalf("rollback replaced SQLite files before closing the restored handle: replacedWhileOpen=%t server=%q error=%v (prepare=%d reload=%d)", replacedWhileOpen, name, readErr, prepares, reloads)
	}
}

// In production these failures can come from filesystem replacement and runtime rehydration.
type secondReviewMaintenanceStore struct{ state maintenancepkg.State }

func (s *secondReviewMaintenanceStore) Load(context.Context) (maintenancepkg.State, error) {
	return s.state, nil
}
func (s *secondReviewMaintenanceStore) Save(_ context.Context, state maintenancepkg.State) error {
	s.state = state
	return nil
}
func TestSecondReviewFailedRollbackKeepsMaintenanceClosed(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	key := []byte("01234567890123456789012345678901")
	original := buildBackupDatabaseDataWithKey(t, key, Server{Name: "original", Host: "old.example", Port: 22, User: "root"}, "")
	replacement := buildBackupDatabaseDataWithKey(t, key, Server{Name: "replacement", Host: "new.example", Port: 22, User: "root"}, "")
	target := filepath.Join(t.TempDir(), "servers.db")
	if err := os.WriteFile(target, original, 0600); err != nil {
		t.Fatal(err)
	}
	config, _ := json.Marshal(map[string]string{"encryption_key": base64.StdEncoding.EncodeToString(key)})
	tarData, err := buildBackupTarGz(map[string][]byte{"servers.db": replacement, "config.json": config})
	if err != nil {
		t.Fatal(err)
	}
	encrypted, err := encryptBackupPayload(tarData, "very-strong-passphrase")
	if err != nil {
		t.Fatal(err)
	}
	reloads := 0
	h.lifecycle.deps.Archive = NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		DBPath: func() string { return target }, CurrentEncryptionKey: func() []byte { return key }, TempDir: t.TempDir,
		RestoredRuntime: restoredRuntimeAdapter{prepare: func() {}, reload: func(context.Context) error { reloads++; return errors.New("runtime inventory unavailable") }},
	})
	store := &secondReviewMaintenanceStore{}
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: store})
	lease, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("exclusive admission denied")
	}
	outcome := h.lifecycle.Restore(context.Background(), backupRestoreCommand{Actor: "admin", Passphrase: "very-strong-passphrase", Blob: encrypted, Lease: lease})
	var incomplete *internalbackup.IncompleteRecoveryError
	if !errors.As(outcome.Err, &incomplete) {
		t.Fatalf("missing typed incomplete recovery: %v", outcome.Err)
	}
	if _, err := os.Stat(filepath.Join(incomplete.SnapshotDir, "recovery.json")); err != nil {
		t.Fatalf("rollback recovery files were discarded: %v", err)
	}
	if len(h.audits) != 0 {
		t.Fatal("ordinary audit persistence used after failed runtime recovery")
	}
	if outcome.Err == nil || reloads != 2 {
		t.Fatalf("expected both reload failures, reloads=%d err=%v", reloads, outcome.Err)
	}
	next, decision := coordinator.TryShared(maintenancepkg.WorkInteractive)
	if next != nil {
		next.Close()
	}
	if decision.Allowed {
		t.Fatalf("interactive work allowed after rollback recovery failed; maintenance active=%t, outcome=%s", coordinator.Snapshot().Active, outcome.Kind)
	}
}

func TestSecondReviewStaleRemovalConfirmationCannotApproveRefreshedPlan(t *testing.T) {
	app := newIsolatedTestApp(t)
	server, err := app.Deps.ServerInventoryService.Create(Server{Name: "stale-approval", Host: "192.0.2.30", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	discoveries, approvals, mutations := 0, 0, 0
	var service *UpdateService
	// Two browser tabs viewed the initial removal list. The second tab keeps that confirmation dialog open.
	oldDialogConfirmRemovals := true
	var oldDialogIdentity serverActionApprovalIdentity
	session := &HostMaintenanceSessionFuncs{
		DiscoverPackagesFunc: func(context.Context, HostOperationRequest) (HostPackageDiscoveryResult, error) {
			discoveries++
			removal := "old-obsolete"
			if discoveries >= 2 {
				removal = "application-service"
			}
			return HostPackageDiscoveryResult{Attempts: 1, Outcome: PackageDiscoveryOutcome{
				PendingPackageCount: 1, Upgradable: []string{"openssl"}, PendingUpdates: []PendingUpdate{{Package: "openssl", Security: true, CVEState: "done"}},
				UpgradePlan: UpgradePlan{FullUpgradePlanAvailable: true, FullUpgradePackageCount: 1, StandardPackageCount: 1, FullUpgradeRemovedPackages: []string{removal}},
			}}, nil
		},
		RunCommandFunc: func(_ context.Context, req HostCommandRequest) (HostCommandResult, error) {
			if strings.Contains(req.Command, "full-upgrade") {
				mutations++
			}
			return HostCommandResult{Attempts: 1}, nil
		},
	}
	service = NewUpdateService(UpdateServiceDeps{
		ServerState: app.Deps.ServerState, CurrentJobManager: app.Deps.CurrentJobManager,
		HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return session, nil
		}),
		LoadPostUpdateCheckConfig: func() PostUpdateCheckConfig { return PostUpdateCheckConfig{Enabled: false} },
		WaitForApprovalPollContext: func(context.Context) error {
			approvals++
			if approvals > 2 {
				t.Fatal("unexpected additional approval")
			}
			if approvals == 1 {
				oldDialogIdentity = approvalIdentityForTest(app.Deps.ServerState, server.Name)
			}
			// Tab A first approves, then Tab B confirms the dialog describing old-obsolete, after revalidation changed the plan.
			result := (&serverActionLifecycle{serverState: app.Deps.ServerState, updateService: service, currentJobManager: app.Deps.CurrentJobManager}).ApproveFullUpgrade(server.Name, oldDialogConfirmRemovals, oldDialogIdentity)
			wantStatus := http.StatusOK
			if approvals == 2 {
				wantStatus = http.StatusConflict
			}
			if result.statusCode != wantStatus {
				t.Fatalf("approval %d returned HTTP %d, want %d for removal list %v", approvals, result.statusCode, wantStatus, app.Deps.ServerState.CurrentStatusSnapshot(server.Name).UpgradePlan.FullUpgradeRemovedPackages)
			}
			if result.statusCode != http.StatusOK {
				_, _ = service.CancelPendingUpdate(server.Name)
			}
			return nil
		},
		UpdateScheduledDiscoveryMeta: func(string, PackageDiscoveryOutcome) {}, SaveServerFacts: func(serverFactsRecord) error { return nil },
		AuditWithActor: func(_, _, _, _, _, _, _ string, _ map[string]any) {},
	})
	job, err := createServerActionJobWithStateAndManager(app.Deps.CurrentJobManager(), app.Deps.ServerState, jobKindUpdate, server.Name, "review", "", RetryPolicy{MaxAttempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	service.RunUpdateJob(UpdateRunRequest{Server: server, JobID: job.ID, Actor: "review", Policy: RetryPolicy{MaxAttempts: 1}})
	if approvals != 2 || discoveries != 2 {
		t.Fatalf("expected initial and refreshed approval plans, got approvals=%d discoveries=%d", approvals, discoveries)
	}
	if mutations > 0 {
		t.Fatalf("full-upgrade executed after stale removal confirmation; approvals=%d discoveries=%d mutations=%d", approvals, discoveries, mutations)
	}
}
