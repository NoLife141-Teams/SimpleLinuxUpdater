package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func TestIncompleteRestoreRetainsMaintenanceAcrossCloseAndRestart(t *testing.T) {
	store := NewMemoryStore()
	c := NewCoordinator(Deps{Store: store})
	lease, d := c.TryExclusive(OperationBackupRestore)
	if !d.Allowed {
		t.Fatal("admission failed")
	}
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "restore"}); err != nil {
		t.Fatal(err)
	}
	if err := lease.RetainForRecovery(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	for _, current := range []*Coordinator{c, NewCoordinator(Deps{Store: store})} {
		if current != c {
			if err := current.Initialize(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if !current.Snapshot().RecoveryRequired {
			t.Fatal("recovery latch disappeared")
		}
		for _, work := range []WorkClass{WorkInteractive, WorkScheduled, WorkAudit} {
			shared, d := current.TryShared(work)
			if shared != nil {
				shared.Close()
			}
			if d.Allowed {
				t.Fatalf("admitted %s during failed recovery", work)
			}
		}
		exclusive, d := current.TryExclusive(OperationBackupExport)
		if exclusive != nil {
			_ = exclusive.Close()
		}
		if d.Allowed {
			t.Fatal("admitted export during failed recovery")
		}
	}
}

func TestRecoveryLatchPersistenceFailureStillBlocksCurrentProcess(t *testing.T) {
	store := NewMemoryStore()
	c := NewCoordinator(Deps{Store: store})
	lease, _ := c.TryExclusive(OperationBackupRestore)
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "restore"}); err != nil {
		t.Fatal(err)
	}
	store.SaveError = errors.New("disk unavailable")
	if err := lease.RetainForRecovery(context.Background()); err == nil {
		t.Fatal("expected persistence error")
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	shared, d := c.TryShared(WorkInteractive)
	if shared != nil {
		shared.Close()
	}
	if d.Allowed || !c.Snapshot().RecoveryRequired {
		t.Fatal("persistence error reopened admission")
	}
}

func TestRestoreCleanupFailureCanResumeFinalizedState(t *testing.T) {
	ctx := t.Context()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "servers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatal(err)
	}
	marker := strings.Repeat("a", 64)
	deps := Deps{
		Store:           SQLiteStore{DB: func() *sql.DB { return db }},
		RecoveryPending: func() (bool, error) { return marker != "", nil },
		ClearFinalizedRestoreMarker: func(digest string) (bool, error) {
			if digest != marker {
				return false, nil
			}
			marker = ""
			return true, nil
		},
	}
	coordinator := NewCoordinator(deps)
	lease, decision := coordinator.TryExclusive(OperationBackupRestore)
	if !decision.Allowed {
		t.Fatal("restore was not admitted")
	}
	if err := lease.Activate(ctx, OperationFacts{JobID: "restore", RestorePreparation: true}); err != nil {
		t.Fatal(err)
	}
	if err := lease.FinalizeRestore(ctx, marker); err != nil {
		t.Fatal(err)
	}
	// The archive service confirmed the installed files but could not remove
	// the marker. The lifecycle keeps the request blocked until restart.
	if err := lease.RetainForRecovery(ctx); err != nil {
		t.Fatal(err)
	}
	if shared, decision := coordinator.TryShared(WorkScheduled); shared != nil || decision.Allowed {
		t.Fatal("cleanup failure released admission in the running process")
	}
	restarted := NewCoordinator(deps)
	if err := restarted.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	shared, decision := restarted.TryShared(WorkScheduled)
	if shared != nil {
		defer shared.Close()
	}
	if !decision.Allowed || marker != "" || restarted.Snapshot().RecoveryRequired {
		t.Fatalf("restart did not complete finalization: %+v, marker present=%t", decision, marker != "")
	}
}
