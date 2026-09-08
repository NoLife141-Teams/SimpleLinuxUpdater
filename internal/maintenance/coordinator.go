package maintenance

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

type WorkClass string

const (
	WorkInteractive WorkClass = "interactive"
	WorkScheduled   WorkClass = "scheduled"
	WorkAudit       WorkClass = "audit"
)

type OperationClass string

const (
	OperationBackupExport  OperationClass = "backup_export"
	OperationBackupRestore OperationClass = "backup_restore"

	exclusiveLeaseReleaseAttempts       = 5
	exclusiveLeaseReleaseDelay          = 5 * time.Millisecond
	exclusiveLeaseReleaseAttemptTimeout = 500 * time.Millisecond
)

type State struct {
	RecoveryRequired bool   `json:"recovery_required,omitempty"`
	Active           bool   `json:"active"`
	Kind             string `json:"kind"`
	JobID            string `json:"job_id"`
	StartedAt        string `json:"started_at"`
	Actor            string `json:"actor"`
	Message          string `json:"message"`
}

type Snapshot struct {
	RecoveryRequired bool   `json:"recovery_required,omitempty"`
	Active           bool   `json:"active"`
	Kind             string `json:"kind"`
	StartedAt        string `json:"started_at"`
	Message          string `json:"message"`
}

type Decision struct {
	Allowed bool
	State   Snapshot
}

type OperationFacts struct {
	JobID   string
	Actor   string
	Message string
}

type Store interface {
	Load(context.Context) (State, error)
	Save(context.Context, State) error
}

type Deps struct {
	Store Store
	Now   func() time.Time
}

type Coordinator struct {
	deps Deps
	gate sync.RWMutex

	stateMu sync.RWMutex
	state   State

	releaseMu      sync.Mutex
	pendingRelease *State
}

func NewCoordinator(deps Deps) *Coordinator {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	return &Coordinator{deps: deps}
}

func (c *Coordinator) Initialize(ctx context.Context) error {
	if c == nil || c.deps.Store == nil {
		return errors.New("maintenance persistence is not configured")
	}
	state, err := c.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if state.Active && (state.RecoveryRequired || state.Kind == string(OperationBackupRestore)) {
		state.RecoveryRequired = true
		state.Message = recoveryRequiredMessage
		c.publish(state)
		return nil
	}
	if state.Active {
		state = State{}
		if err := c.deps.Store.Save(ctx, state); err != nil {
			return err
		}
	}
	c.publish(state)
	return nil
}

func (c *Coordinator) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{}
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return snapshot(c.state)
}

func (c *Coordinator) TryShared(_ WorkClass) (*SharedLease, Decision) {
	if c == nil || !c.gate.TryRLock() {
		return nil, Decision{State: c.Snapshot()}
	}
	c.recoverPendingRelease()
	if state := c.Snapshot(); state.Active {
		c.gate.RUnlock()
		return nil, Decision{State: state}
	}
	return &SharedLease{coordinator: c}, Decision{Allowed: true}
}

func (c *Coordinator) TryExclusive(operation OperationClass) (*ExclusiveLease, Decision) {
	if c == nil || !c.gate.TryLock() {
		return nil, Decision{State: c.Snapshot()}
	}
	c.recoverPendingRelease()
	if state := c.Snapshot(); state.Active {
		c.gate.Unlock()
		return nil, Decision{State: state}
	}
	return &ExclusiveLease{coordinator: c, operation: operation}, Decision{Allowed: true}
}

type SharedLease struct {
	coordinator *Coordinator
	once        sync.Once
}

func (l *SharedLease) Close() {
	if l == nil || l.coordinator == nil {
		return
	}
	l.once.Do(l.coordinator.gate.RUnlock)
}

type ExclusiveLease struct {
	coordinator *Coordinator
	operation   OperationClass

	mu             sync.Mutex
	activated      bool
	activeState    State
	releasePending bool
	gateReleased   bool
	closed         bool
}

func (l *ExclusiveLease) Activate(ctx context.Context, facts OperationFacts) error {
	if l == nil || l.coordinator == nil {
		return errors.New("maintenance lease is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("maintenance lease is closed")
	}
	if l.releasePending {
		return errors.New("maintenance lease release is pending")
	}
	if l.activated {
		return nil
	}
	state := State{
		Active:    true,
		Kind:      string(l.operation),
		JobID:     strings.TrimSpace(facts.JobID),
		StartedAt: l.coordinator.deps.Now().UTC().Format(time.RFC3339Nano),
		Actor:     strings.TrimSpace(facts.Actor),
		Message:   strings.TrimSpace(facts.Message),
	}
	if err := l.coordinator.deps.Store.Save(ctx, state); err != nil {
		return err
	}
	l.coordinator.publish(state)
	l.activeState = state
	l.activated = true
	return nil
}

func (l *ExclusiveLease) State() State {
	if l == nil || l.coordinator == nil {
		return State{}
	}
	return l.coordinator.currentState()
}

func (l *ExclusiveLease) Handoff(ctx context.Context) error {
	if l == nil || l.coordinator == nil {
		return errors.New("maintenance lease is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return errors.New("maintenance lease is closed")
	}
	if l.releasePending {
		return errors.New("maintenance lease release is pending")
	}
	state := l.State()
	if !state.Active {
		return nil
	}
	return l.coordinator.deps.Store.Save(ctx, state)
}

const recoveryRequiredMessage = "Backup recovery is incomplete. Stop the application and restore verified state before resuming. See the recovery steps in docs/troubleshooting.md."

// RetainForRecovery releases the request's gate but keeps admission closed.
// Close (including a middleware defer) cannot clear this recovery latch.
func (l *ExclusiveLease) RetainForRecovery(ctx context.Context) error {
	if l == nil || l.coordinator == nil {
		return errors.New("maintenance lease is not configured")
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	state := l.activeState
	state.Active = true
	state.RecoveryRequired = true
	state.Message = recoveryRequiredMessage
	l.coordinator.publish(state)
	l.closed = true
	l.releaseGate()
	return l.coordinator.deps.Store.Save(ctx, state)
}

func (l *ExclusiveLease) Close() error {
	if l == nil || l.coordinator == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	if l.activated {
		if err := l.coordinator.release(l.activeState); err != nil {
			l.releasePending = true
			l.releaseGate()
			return err
		}
	}
	l.releasePending = false
	l.closed = true
	l.releaseGate()
	return nil
}

func (l *ExclusiveLease) releaseGate() {
	if l.gateReleased {
		return
	}
	l.gateReleased = true
	l.coordinator.gate.Unlock()
}

func (c *Coordinator) release(expected State) error {
	c.releaseMu.Lock()
	defer c.releaseMu.Unlock()
	return c.releaseLocked(expected)
}

func (c *Coordinator) releaseLocked(expected State) error {
	current := c.currentState()
	if !current.Active {
		c.pendingRelease = nil
		return nil
	}
	if current != expected {
		c.clearPendingReleaseIfOwnedBy(expected)
		return errors.New("maintenance lease no longer owns the active state")
	}
	var err error
	for attempt := 1; attempt <= exclusiveLeaseReleaseAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), exclusiveLeaseReleaseAttemptTimeout)
		err = c.deps.Store.Save(ctx, State{})
		cancel()
		if err == nil {
			c.publish(State{})
			c.pendingRelease = nil
			return nil
		}
		if attempt < exclusiveLeaseReleaseAttempts {
			time.Sleep(time.Duration(attempt) * exclusiveLeaseReleaseDelay)
		}
	}
	pending := expected
	c.pendingRelease = &pending
	return err
}

func (c *Coordinator) clearPendingReleaseIfOwnedBy(expected State) {
	if c.pendingRelease != nil && *c.pendingRelease == expected {
		c.pendingRelease = nil
	}
}

func (c *Coordinator) recoverPendingRelease() {
	if c == nil {
		return
	}
	c.releaseMu.Lock()
	defer c.releaseMu.Unlock()
	if c.pendingRelease == nil {
		return
	}
	_ = c.releaseLocked(*c.pendingRelease)
}

func (c *Coordinator) currentState() State {
	if c == nil {
		return State{}
	}
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	return c.state
}

func (c *Coordinator) publish(state State) {
	c.stateMu.Lock()
	c.state = state
	c.stateMu.Unlock()
}

func snapshot(state State) Snapshot {
	return Snapshot{
		RecoveryRequired: state.RecoveryRequired,
		Active:           state.Active,
		Kind:             state.Kind,
		StartedAt:        state.StartedAt,
		Message:          state.Message,
	}
}

type MemoryStore struct {
	mu        sync.Mutex
	state     State
	LoadError error
	SaveError error
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{} }

func (s *MemoryStore) Load(context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state, s.LoadError
}

func (s *MemoryStore) Save(_ context.Context, state State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.SaveError != nil {
		return s.SaveError
	}
	s.state = state
	return nil
}
