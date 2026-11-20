package maintenance

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
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
	OperationRestoreResume OperationClass = "restore_isolation_resume"

	exclusiveLeaseReleaseAttempts       = 5
	exclusiveLeaseReleaseDelay          = 5 * time.Millisecond
	exclusiveLeaseReleaseAttemptTimeout = 500 * time.Millisecond
)

type State struct {
	RecoveryRequired    bool   `json:"recovery_required,omitempty"`
	RestorePhase        string `json:"restore_phase,omitempty"`
	RestoreMarkerDigest string `json:"restore_marker_digest,omitempty"`
	Active              bool   `json:"active"`
	Kind                string `json:"kind"`
	JobID               string `json:"job_id"`
	StartedAt           string `json:"started_at"`
	Actor               string `json:"actor"`
	Message             string `json:"message"`
}

const (
	RestorePreparing = "preparing_v1"
	RestoreReplacing = "replacing_v1"
	RestoreFinalized = "finalized_v1"
)

// ResumableRestorePhase identifies states that need no file replacement
// once live SQLite and the presence or identity of the marker are checked.
func (s State) ResumableRestorePhase() string {
	if !s.Active || s.RecoveryRequired || s.Kind != string(OperationBackupRestore) || s.JobID == "" {
		return ""
	}
	switch s.RestorePhase {
	case RestorePreparing:
		if s.RestoreMarkerDigest == "" {
			return s.RestorePhase
		}
	case RestoreFinalized:
		if digest, err := hex.DecodeString(s.RestoreMarkerDigest); err == nil && len(digest) == 32 {
			return s.RestorePhase
		}
	}
	return ""
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
	Blocker Blocker
}

type Blocker struct {
	Cause BlockCause
	JobID string
}

type BlockCause string

const (
	BlockBackupExport     BlockCause = "backup_export"
	BlockBackupRestore    BlockCause = "backup_restore"
	BlockRecoveryRequired BlockCause = "recovery_required"
)

type OperationFacts struct {
	JobID              string
	Actor              string
	Message            string
	RestorePreparation bool
}

type Store interface {
	Load(context.Context) (State, error)
	Save(context.Context, State) error
}

type Deps struct {
	Store                       Store
	Now                         func() time.Time
	RecoveryPending             func() (bool, error)
	ClearFinalizedRestoreMarker func(string) (bool, error)
}

type Coordinator struct {
	deps Deps
	gate sync.RWMutex

	stateMu             sync.RWMutex
	state               State
	replacementPrepared bool

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
	return c.initialize(ctx, true)
}

// InitializeAfterReplacement loads the new SQLite state during an in-process
// restore. The current operation still owns the filesystem recovery marker.
func (c *Coordinator) InitializeAfterReplacement(ctx context.Context) error {
	return c.initialize(ctx, false)
}

func (c *Coordinator) initialize(ctx context.Context, startup bool) error {
	if c == nil || c.deps.Store == nil {
		return errors.New("maintenance persistence is not configured")
	}
	state, err := c.deps.Store.Load(ctx)
	if err != nil {
		return err
	}
	if startup && c.deps.RecoveryPending != nil {
		pending, err := c.deps.RecoveryPending()
		if err != nil {
			return fmt.Errorf("inspect durable restore recovery marker: %w", err)
		}
		phase := state.ResumableRestorePhase()
		if phase != "" && (!pending || (phase == RestoreFinalized && c.deps.ClearFinalizedRestoreMarker != nil)) {
			if checker, ok := c.deps.Store.(interface{ CheckRestoreDatabase(context.Context) error }); ok {
				if err := checker.CheckRestoreDatabase(ctx); err == nil {
					if pending {
						cleared, err := c.deps.ClearFinalizedRestoreMarker(state.RestoreMarkerDigest)
						if err != nil {
							return fmt.Errorf("complete finalized restore: %w", err)
						}
						pending = !cleared
					}
					if !pending {
						if err := c.deps.Store.Save(ctx, State{}); err != nil {
							return err
						}
						state = State{}
					}
				}
			}
		}
		if pending {
			if state.Kind != string(OperationBackupRestore) {
				state = State{Kind: string(OperationBackupRestore)}
			}
			state.Active = true
			state.RecoveryRequired = true
		}
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
	if c == nil {
		return nil, Decision{}
	}
	c.stateMu.RLock()
	locked, state := c.gate.TryRLock(), c.state
	c.stateMu.RUnlock()
	if !locked {
		return nil, sharedDenial(state)
	}
	c.recoverPendingRelease()
	if state := c.currentState(); state.Active {
		c.gate.RUnlock()
		return nil, sharedDenial(state)
	}
	return &SharedLease{coordinator: c}, Decision{Allowed: true}
}

// WithDeniedPersistence keeps the observed operation stable while recording a
// denial. A restore seals the original persistence before files can be replaced.
func (c *Coordinator) WithDeniedPersistence(decision Decision, record func() error) error {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()
	if decision.Allowed || !c.state.Active || c.state.RecoveryRequired ||
		c.replacementPrepared || sharedDenial(c.state) != decision {
		return nil
	}
	return record()
}

func sharedDenial(state State) Decision {
	decision := Decision{State: snapshot(state)}
	if !state.Active {
		return decision
	}
	switch OperationClass(state.Kind) {
	case OperationBackupExport:
		decision.Blocker = Blocker{Cause: BlockBackupExport, JobID: state.JobID}
	case OperationBackupRestore:
		decision.Blocker = Blocker{Cause: BlockBackupRestore, JobID: state.JobID}
	}
	if state.RecoveryRequired {
		decision.Blocker.Cause = BlockRecoveryRequired
	}
	return decision
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

	mu               sync.Mutex
	activated        bool
	activeState      State
	restoreFinalized bool
	releasePending   bool
	gateReleased     bool
	closed           bool
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
	if l.operation == OperationBackupRestore && facts.RestorePreparation && state.JobID != "" {
		state.RestorePhase = RestorePreparing
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
	if l.activeState.RestorePhase == RestorePreparing {
		// The original database already holds Preparation and the staged
		// database holds Replacement. Overwriting either would erase evidence
		// needed if the process stops again before finalization.
		persisted, err := l.coordinator.deps.Store.Load(ctx)
		if err != nil {
			return err
		}
		if !l.ownsPersistedRestore(persisted) {
			return errors.New("restore handoff could not verify its persisted maintenance state")
		}
		return nil
	}
	return l.coordinator.deps.Store.Save(ctx, state)
}

func (l *ExclusiveLease) ownsPersistedRestore(state State) bool {
	return state.Active && state.Kind == l.activeState.Kind && state.JobID == l.activeState.JobID &&
		state.StartedAt == l.activeState.StartedAt &&
		(state.RestorePhase == RestorePreparing || state.RestorePhase == RestoreReplacing || state.RestorePhase == RestoreFinalized)
}

// FinalizeRestore durably records that the installed files and runtime were
// verified. The marker can be removed afterwards without leaving an
// unrecoverable maintenance lock if the process stops before Close.
func (l *ExclusiveLease) FinalizeRestore(ctx context.Context, markerDigest string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.releasePending || !l.activated || l.activeState.RestorePhase != RestorePreparing {
		return errors.New("active phased restore lease is required for finalization")
	}
	store := l.coordinator.deps.Store
	state, err := store.Load(ctx)
	if err != nil {
		return err
	}
	if !l.ownsPersistedRestore(state) {
		return errors.New("restore finalization could not verify its persisted maintenance state")
	}
	state.RestorePhase = RestoreFinalized
	state.RestoreMarkerDigest = markerDigest
	state.RecoveryRequired = false
	save := store.Save
	if durable, ok := store.(interface {
		SaveDurable(context.Context, State) error
	}); ok {
		save = durable.SaveDurable
	}
	if err := save(ctx, state); err != nil {
		return err
	}
	l.restoreFinalized = true
	return nil
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
	if l.restoreFinalized {
		// Marker cleanup can fail after the installed state has been verified.
		// Keep admission closed here and preserve the durable proof so startup
		// can retry cleanup without replacing files or running a rollback.
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
	c.replacementPrepared = false
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

// TryProbe grants a read-only view without retrying pending releases or touching
// persistence. Holding the lease prevents DB replacement during the short probe.
func (c *Coordinator) TryProbe() (*SharedLease, Snapshot, bool) {
	if c == nil {
		return nil, Snapshot{}, false
	}
	if !c.gate.TryRLock() {
		return nil, c.Snapshot(), false
	}
	state := c.Snapshot()
	if state.Active {
		c.gate.RUnlock()
		return nil, state, false
	}
	return &SharedLease{coordinator: c}, state, true
}
