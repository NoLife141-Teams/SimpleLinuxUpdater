package maintenance

import (
	"context"
	"errors"
	"testing"
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
