package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	maintenancepkg "debian-updater/internal/maintenance"
	notificationpkg "debian-updater/internal/notifications"
)

func TestBackupPreparationFailureReleasesMaintenanceWithoutReplacingPersistence(t *testing.T) {
	h := newBackupLifecycleHarness(t)
	key := []byte("01234567890123456789012345678901")
	original := buildBackupDatabaseDataWithKey(t, key, Server{Name: "original", Host: "old.example", Port: 22, User: "root"}, "")
	replacement := buildBackupDatabaseDataWithKey(t, key, Server{Name: "replacement", Host: "new.example", Port: 22, User: "root"}, "")
	dir := t.TempDir()
	target := filepath.Join(dir, "servers.db")
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

	// Exercise the production notification preparer and Runtime Composition.
	// Renaming the outbox makes preparation fail after pausing persistence.
	notifications := notificationpkg.NewService(notificationpkg.ServiceDeps{DB: func() *sql.DB { return h.db }, Logf: func(string, ...any) {}})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := notifications.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	if _, err := h.db.Exec("ALTER TABLE notification_outbox RENAME TO unavailable_outbox"); err != nil {
		t.Fatal(err)
	}
	composition := newRuntimeComposition(AppDeps{NotificationService: notifications})
	composition.resetCaches = func() { t.Error("preparation failure closed original runtime persistence") }
	h.lifecycle.deps.Archive = NewBackupServiceWithDeps(internalbackup.ServiceDeps{
		DBPath: func() string { return target }, CurrentEncryptionKey: func() []byte { return key }, TempDir: func() string { return dir },
		RestoredRuntime: composition,
	})
	store := maintenancepkg.NewMemoryStore()
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: store})
	lease, decision := coordinator.TryExclusive(maintenancepkg.OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("exclusive admission denied")
	}
	outcome := h.lifecycle.Restore(context.Background(), backupRestoreCommand{Actor: "admin", Passphrase: "very-strong-passphrase", Blob: encrypted, Lease: lease})
	if outcome.Kind != backupOperationRestoreApplyFailed || outcome.Err == nil || !strings.Contains(outcome.Err.Error(), "notification_outbox") {
		t.Fatalf("expected notification preparation failure, got %+v", outcome)
	}
	var incomplete *internalbackup.IncompleteRecoveryError
	if errors.As(outcome.Err, &incomplete) {
		t.Error("recoverable preparation failure was classified as incomplete recovery")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	for _, current := range []*maintenancepkg.Coordinator{coordinator, maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: store})} {
		if current != coordinator {
			if err := current.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if current.Snapshot().Active || current.Snapshot().RecoveryRequired {
			t.Error("preparation failure retained maintenance in current process or after restart")
		}
		for _, work := range []maintenancepkg.WorkClass{maintenancepkg.WorkInteractive, maintenancepkg.WorkScheduled} {
			next, decision := current.TryShared(work)
			if next != nil {
				next.Close()
			}
			if !decision.Allowed {
				t.Errorf("preparation failure blocked %s work", work)
			}
		}
	}
	actual, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(actual, original) {
		t.Fatalf("original database changed: %v", err)
	}
	snapshots, err := filepath.Glob(filepath.Join(dir, "slu-restore-rollback-*"))
	if err != nil || len(snapshots) != 0 {
		t.Fatalf("preparation failure created rollback snapshots: %v %v", snapshots, err)
	}
	job, err := h.jobManager.GetJob(outcome.JobID)
	if err != nil || job.Status != jobStatusFailed {
		t.Fatalf("failed restore job was not persisted: %+v %v", job, err)
	}
	if len(h.audits) != 1 || h.audits[0].Action != "backup.restore" || h.audits[0].Status != "failure" {
		t.Fatalf("missing ordinary restore failure audit: %+v", h.audits)
	}
}
