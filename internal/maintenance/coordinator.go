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
)

type State struct {
	Active    bool   `json:"active"`
	Kind      string `json:"kind"`
	JobID     string `json:"job_id"`
	StartedAt string `json:"started_at"`
	Actor     string `json:"actor"`
	Message   string `json:"message"`
}

type Snapshot struct {
	Active    bool   `json:"active"`
	Kind      string `json:"kind"`
	StartedAt string `json:"started_at"`
	Message   string `json:"message"`
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

	mu        sync.Mutex
	activated bool
	closed    bool
	closeErr  error
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
	l.activated = true
	return nil
}

func (l *ExclusiveLease) State() State {
	if l == nil || l.coordinator == nil {
		return State{}
	}
	l.coordinator.stateMu.RLock()
	defer l.coordinator.stateMu.RUnlock()
	return l.coordinator.state
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
	state := l.State()
	if !state.Active {
		return nil
	}
	return l.coordinator.deps.Store.Save(ctx, state)
}

func (l *ExclusiveLease) Close() error {
	if l == nil || l.coordinator == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return l.closeErr
	}
	l.closed = true
	if l.activated {
		if err := l.coordinator.deps.Store.Save(context.Background(), State{}); err != nil {
			l.closeErr = err
		} else {
			l.coordinator.publish(State{})
		}
	}
	l.coordinator.gate.Unlock()
	return l.closeErr
}

func (c *Coordinator) publish(state State) {
	c.stateMu.Lock()
	c.state = state
	c.stateMu.Unlock()
}

func snapshot(state State) Snapshot {
	return Snapshot{
		Active:    state.Active,
		Kind:      state.Kind,
		StartedAt: state.StartedAt,
		Message:   state.Message,
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
