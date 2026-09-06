package maintenance

import (
	"context"
	"testing"
)

func TestStaleLeaseRetryPreservesNewerPendingRelease(t *testing.T) {
	store := &releaseFailureStore{failInactive: true}
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}

	oldLease, _ := coordinator.TryExclusive(OperationBackupExport)
	if err := oldLease.Activate(context.Background(), OperationFacts{JobID: "old-export"}); err != nil {
		t.Fatal(err)
	}
	if err := oldLease.Close(); err == nil {
		t.Fatal("old Close() error = nil, want persistence failure")
	}

	store.setFailInactive(false)
	newLease, decision := coordinator.TryExclusive(OperationBackupRestore)
	if !decision.Allowed || newLease == nil {
		t.Fatalf("TryExclusive(new) = %#v, %+v", newLease, decision)
	}
	if err := newLease.Activate(context.Background(), OperationFacts{JobID: "new-restore"}); err != nil {
		t.Fatal(err)
	}

	store.setFailInactive(true)
	if err := newLease.Close(); err == nil {
		t.Fatal("new Close() error = nil, want persistence failure")
	}
	if err := oldLease.Close(); err == nil {
		t.Fatal("stale old Close() error = nil, want ownership mismatch")
	}
	if state := newLease.State(); !state.Active || state.JobID != "new-restore" {
		t.Fatalf("state after stale old retry = %+v, want new release still pending", state)
	}

	store.setFailInactive(false)
	shared, decision := coordinator.TryShared(WorkInteractive)
	if !decision.Allowed || shared == nil {
		t.Fatalf("TryShared() = %#v, %+v, want recovery of newer pending release", shared, decision)
	}
	shared.Close()
	if got := coordinator.Snapshot(); got.Active {
		t.Fatalf("Snapshot() = %+v, want inactive after newer pending release recovery", got)
	}
	if err := newLease.Close(); err != nil {
		t.Fatalf("new Close() after automatic recovery error = %v", err)
	}
	if err := oldLease.Close(); err != nil {
		t.Fatalf("old Close() after all releases error = %v", err)
	}
}
