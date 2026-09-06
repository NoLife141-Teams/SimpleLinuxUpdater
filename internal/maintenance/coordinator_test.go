package maintenance

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type releaseFailureStore struct {
	mu                  sync.Mutex
	state               State
	failInactive        bool
	failInactiveAttempts int
	inactiveSaveAttempts int
}

func (s *releaseFailureStore) Load(context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, nil
}

func (s *releaseFailureStore) Save(_ context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !state.Active {
		s.inactiveSaveAttempts++
		if s.failInactive || s.failInactiveAttempts > 0 {
			if s.failInactiveAttempts > 0 {
				s.failInactiveAttempts--
			}
			return errors.New("temporary maintenance release failure")
		}
	}
	s.state = state
	return nil
}

func (s *releaseFailureStore) setFailInactive(fail bool) {
	s.mu.Lock()
	s.failInactive = fail
	s.mu.Unlock()
}

func (s *releaseFailureStore) inactiveAttempts() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inactiveSaveAttempts
}

func TestCoordinatorSharedAndExclusiveAdmission(t *testing.T) {
	coordinator := NewCoordinator(Deps{Store: NewMemoryStore(), Now: func() time.Time {
		return time.Date(2026, 7, 10, 20, 0, 0, 0, time.UTC)
	}})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	first, decision := coordinator.TryShared(WorkInteractive)
	if !decision.Allowed || first == nil {
		t.Fatalf("TryShared(first) = %#v, %+v", first, decision)
	}
	second, decision := coordinator.TryShared(WorkScheduled)
	if !decision.Allowed || second == nil {
		t.Fatalf("TryShared(second) = %#v, %+v", second, decision)
	}
	if lease, decision := coordinator.TryExclusive(OperationBackupExport); decision.Allowed || lease != nil {
		t.Fatalf("TryExclusive() with readers = %#v, %+v, want denied", lease, decision)
	}
	second.Close()
	first.Close()

	exclusive, decision := coordinator.TryExclusive(OperationBackupExport)
	if !decision.Allowed || exclusive == nil {
		t.Fatalf("TryExclusive() = %#v, %+v", exclusive, decision)
	}
	if shared, decision := coordinator.TryShared(WorkInteractive); decision.Allowed || shared != nil {
		t.Fatalf("TryShared() during exclusive = %#v, %+v, want denied", shared, decision)
	}
	if err := exclusive.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestExclusiveLeaseActivationPersistsBeforePublishingRedactedSnapshot(t *testing.T) {
	store := NewMemoryStore()
	coordinator := NewCoordinator(Deps{Store: store, Now: func() time.Time {
		return time.Date(2026, 7, 10, 20, 1, 2, 3, time.UTC)
	}})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	lease, decision := coordinator.TryExclusive(OperationBackupRestore)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	if err := lease.Activate(context.Background(), OperationFacts{
		JobID:   "secret-job",
		Actor:   "secret-actor",
		Message: "Restore in progress",
	}); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	snapshot := coordinator.Snapshot()
	if !snapshot.Active || snapshot.Kind != string(OperationBackupRestore) || snapshot.Message != "Restore in progress" {
		t.Fatalf("Snapshot() = %+v", snapshot)
	}
	if snapshot.StartedAt != "2026-07-10T20:01:02.000000003Z" {
		t.Fatalf("Snapshot().StartedAt = %q", snapshot.StartedAt)
	}
	state, err := store.Load(context.Background())
	if err != nil || state.JobID != "secret-job" || state.Actor != "secret-actor" {
		t.Fatalf("Store.Load() = %+v, %v", state, err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestExclusiveLeaseActivationFailureDoesNotPublishState(t *testing.T) {
	store := NewMemoryStore()
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	lease, decision := coordinator.TryExclusive(OperationBackupExport)
	if !decision.Allowed {
		t.Fatalf("TryExclusive() decision = %+v", decision)
	}
	store.SaveError = errors.New("disk full")
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "job"}); err == nil {
		t.Fatal("Activate() error = nil, want persistence failure")
	}
	if snapshot := coordinator.Snapshot(); snapshot.Active {
		t.Fatalf("Snapshot() after failed activation = %+v", snapshot)
	}
	store.SaveError = nil
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestInitializeClearsStaleActiveStateBeforePublishing(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save(context.Background(), State{Active: true, JobID: "stale"}); err != nil {
		t.Fatal(err)
	}
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}
	if got := coordinator.Snapshot(); got.Active {
		t.Fatalf("Snapshot() = %+v, want inactive", got)
	}
	state, _ := store.Load(context.Background())
	if state.Active {
		t.Fatalf("persisted state = %+v, want inactive", state)
	}
}

func TestInitializeFailsClosedWhenStaleStateCannotBeCleared(t *testing.T) {
	store := NewMemoryStore()
	if err := store.Save(context.Background(), State{Active: true, JobID: "stale"}); err != nil {
		t.Fatal(err)
	}
	store.SaveError = errors.New("read only database")
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err == nil {
		t.Fatal("Initialize() error = nil, want clear failure")
	}
	if got := coordinator.Snapshot(); got.Active {
		t.Fatalf("Snapshot() published stale state = %+v", got)
	}
}

func TestExclusiveLeaseHandoffPersistsActiveStateToCurrentStore(t *testing.T) {
	store := NewMemoryStore()
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, _ := coordinator.TryExclusive(OperationBackupRestore)
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "restore"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), State{}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Handoff(context.Background()); err != nil {
		t.Fatalf("Handoff() error = %v", err)
	}
	state, _ := store.Load(context.Background())
	if !state.Active || state.JobID != "restore" {
		t.Fatalf("persisted state = %+v, want active restore", state)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExclusiveLeaseReleaseFailureKeepsFailClosedSnapshot(t *testing.T) {
	store := NewMemoryStore()
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, _ := coordinator.TryExclusive(OperationBackupExport)
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "export"}); err != nil {
		t.Fatal(err)
	}
	store.SaveError = errors.New("disk full")
	if err := lease.Close(); err == nil {
		t.Fatal("Close() error = nil, want persistence failure")
	}
	if got := coordinator.Snapshot(); !got.Active {
		t.Fatalf("Snapshot() = %+v, want fail-closed active state", got)
	}
	if shared, decision := coordinator.TryShared(WorkInteractive); shared != nil || decision.Allowed {
		t.Fatalf("TryShared() = %#v, %+v, want denied", shared, decision)
	}
}

func TestExclusiveLeaseReleaseRetriesTransientPersistenceFailure(t *testing.T) {
	store := &releaseFailureStore{failInactiveAttempts: 2}
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, decision := coordinator.TryExclusive(OperationBackupExport)
	if !decision.Allowed || lease == nil {
		t.Fatalf("TryExclusive() = %#v, %+v", lease, decision)
	}
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "export"}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() error = %v, want bounded retry recovery", err)
	}
	if attempts := store.inactiveAttempts(); attempts != 3 {
		t.Fatalf("inactive Save attempts = %d, want 3", attempts)
	}
	if got := coordinator.Snapshot(); got.Active {
		t.Fatalf("Snapshot() = %+v, want inactive after recovered release", got)
	}
}

func TestExclusiveLeasePendingReleaseRecoversOnNextAdmission(t *testing.T) {
	store := &releaseFailureStore{failInactive: true}
	coordinator := NewCoordinator(Deps{Store: store})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, _ := coordinator.TryExclusive(OperationBackupExport)
	if err := lease.Activate(context.Background(), OperationFacts{JobID: "export"}); err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err == nil {
		t.Fatal("Close() error = nil, want exhausted release retries")
	}
	if got := coordinator.Snapshot(); !got.Active {
		t.Fatalf("Snapshot() = %+v, want fail-closed active state", got)
	}

	store.setFailInactive(false)
	shared, decision := coordinator.TryShared(WorkInteractive)
	if !decision.Allowed || shared == nil {
		t.Fatalf("TryShared() after persistence recovery = %#v, %+v, want automatic release recovery", shared, decision)
	}
	shared.Close()
	if got := coordinator.Snapshot(); got.Active {
		t.Fatalf("Snapshot() = %+v, want inactive after online recovery", got)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("Close() after automatic recovery error = %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
}

func TestStaleLeaseRetryCannotClearNewActiveLease(t *testing.T) {
	store := &releaseFailureStore{failInactive: true}
	coordinator := NewCoordinator(Deps{Store: store, Now: func() time.Time {
		return time.Date(2026, 9, 5, 20, 0, 0, 0, time.UTC)
	}})
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
		t.Fatalf("TryExclusive(new) = %#v, %+v, want pending release recovery followed by admission", newLease, decision)
	}
	if err := newLease.Activate(context.Background(), OperationFacts{JobID: "new-restore"}); err != nil {
		t.Fatal(err)
	}
	if err := oldLease.Close(); err == nil {
		t.Fatal("stale Close() error = nil, want ownership mismatch")
	}
	state := newLease.State()
	if !state.Active || state.JobID != "new-restore" || state.Kind != string(OperationBackupRestore) {
		t.Fatalf("active state after stale retry = %+v, want new restore preserved", state)
	}
	if err := newLease.Close(); err != nil {
		t.Fatalf("new Close() error = %v", err)
	}
	if err := oldLease.Close(); err != nil {
		t.Fatalf("old Close() after new release error = %v", err)
	}
	shared, decision := coordinator.TryShared(WorkInteractive)
	if !decision.Allowed || shared == nil {
		t.Fatalf("TryShared() after releases = %#v, %+v", shared, decision)
	}
	shared.Close()
}
