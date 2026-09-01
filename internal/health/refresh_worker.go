package health

import (
	"context"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"time"

	"debian-updater/internal/servers"
)

const (
	DefaultAutomaticRefreshAfter = 20 * time.Hour
	DefaultRefreshSweepInterval  = time.Hour
	DefaultRefreshJitterWindow   = 2 * time.Hour
	DefaultRefreshRetryBase      = 15 * time.Minute
	DefaultRefreshRetryMax       = 6 * time.Hour
)

type RefreshAttemptState string

const (
	RefreshAttemptSucceeded  RefreshAttemptState = "succeeded"
	RefreshAttemptIncomplete RefreshAttemptState = "incomplete"
	RefreshAttemptDeferred   RefreshAttemptState = "deferred"
	RefreshAttemptFailed     RefreshAttemptState = "failed"
)

type RefreshAttempt struct {
	State      RefreshAttemptState
	Facts      CollectedFacts
	Reason     string
	ReasonCode string
	Err        error
}

type RefreshWorkerDeps struct {
	Now             func() time.Time
	SnapshotServers func() []servers.Server
	LatestFacts     func() (map[string]CollectedFacts, error)
	Refresh         func(context.Context, servers.Server) RefreshAttempt
	ObserveAttempt  func(servers.Server, RefreshAttempt)
	Logf            func(string, ...any)
}

type RefreshWorkerOptions struct {
	RefreshAfter  time.Duration
	SweepInterval time.Duration
	JitterWindow  time.Duration
	RetryBase     time.Duration
	RetryMax      time.Duration
}

func (o RefreshWorkerOptions) withDefaults() RefreshWorkerOptions {
	if o.RefreshAfter <= 0 {
		o.RefreshAfter = DefaultAutomaticRefreshAfter
	}
	if o.SweepInterval <= 0 {
		o.SweepInterval = DefaultRefreshSweepInterval
	}
	if o.JitterWindow < 0 {
		o.JitterWindow = 0
	} else if o.JitterWindow == 0 {
		o.JitterWindow = DefaultRefreshJitterWindow
	}
	if o.RetryBase <= 0 {
		o.RetryBase = DefaultRefreshRetryBase
	}
	if o.RetryMax <= 0 {
		o.RetryMax = DefaultRefreshRetryMax
	}
	if o.RetryMax < o.RetryBase {
		o.RetryMax = o.RetryBase
	}
	return o
}

type refreshRetryState struct {
	failures    int
	nextAttempt time.Time
}

type RefreshWorker struct {
	deps    RefreshWorkerDeps
	options RefreshWorkerOptions

	startOnce sync.Once
	runMu     sync.Mutex
	doneMu    sync.Mutex
	done      chan struct{}
	retries   map[string]refreshRetryState
}

func NewRefreshWorker(deps RefreshWorkerDeps, options RefreshWorkerOptions) *RefreshWorker {
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}
	if deps.SnapshotServers == nil {
		deps.SnapshotServers = func() []servers.Server { return nil }
	}
	if deps.LatestFacts == nil {
		deps.LatestFacts = func() (map[string]CollectedFacts, error) { return map[string]CollectedFacts{}, nil }
	}
	if deps.Refresh == nil {
		deps.Refresh = func(context.Context, servers.Server) RefreshAttempt {
			return RefreshAttempt{State: RefreshAttemptDeferred, Reason: "host facts refresh is unavailable"}
		}
	}
	if deps.ObserveAttempt == nil {
		deps.ObserveAttempt = func(servers.Server, RefreshAttempt) {}
	}
	if deps.Logf == nil {
		deps.Logf = func(string, ...any) {}
	}
	return &RefreshWorker{deps: deps, options: options.withDefaults(), retries: map[string]refreshRetryState{}}
}

func (w *RefreshWorker) Start(ctx context.Context) {
	if w == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.startOnce.Do(func() {
		done := make(chan struct{})
		w.doneMu.Lock()
		w.done = done
		w.doneMu.Unlock()
		go func() {
			defer close(done)
			w.RunOnce(ctx)
			ticker := time.NewTicker(w.options.SweepInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ticker.C:
					w.RunOnce(ctx)
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

func (w *RefreshWorker) Wait() {
	if w == nil {
		return
	}
	w.doneMu.Lock()
	done := w.done
	w.doneMu.Unlock()
	if done != nil {
		<-done
	}
}

func (w *RefreshWorker) RunOnce(ctx context.Context) {
	if w == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.runMu.Lock()
	defer w.runMu.Unlock()
	if ctx.Err() != nil {
		return
	}
	facts, err := w.deps.LatestFacts()
	if err != nil {
		w.deps.Logf("automatic host facts refresh could not load current facts: %v", err)
		return
	}
	serverList := w.deps.SnapshotServers()
	sort.Slice(serverList, func(i, j int) bool { return serverList[i].Name < serverList[j].Name })
	now := w.deps.Now().UTC()
	for _, server := range serverList {
		if ctx.Err() != nil {
			return
		}
		name := strings.TrimSpace(server.Name)
		if name == "" {
			continue
		}
		if !w.refreshDue(name, facts[name], now) {
			delete(w.retries, name)
			continue
		}
		if w.retryPending(name, now) {
			continue
		}
		attempt := w.deps.Refresh(ctx, server)
		if attempt.State == "" {
			attempt.State = RefreshAttemptFailed
		}
		if attempt.State == RefreshAttemptSucceeded && !FactsHealthComplete(attempt.Facts) {
			attempt.State = RefreshAttemptIncomplete
			if strings.TrimSpace(attempt.Reason) == "" {
				attempt.Reason = "host facts refresh returned incomplete disk or APT health data"
			}
		}
		w.recordRetry(name, attempt.State, now)
		w.deps.ObserveAttempt(server, attempt)
	}
}

func (w *RefreshWorker) refreshDue(name string, facts CollectedFacts, now time.Time) bool {
	if strings.TrimSpace(facts.ServerName) == "" || !FactsHealthComplete(facts) {
		return true
	}
	collectedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(facts.CollectedAt))
	if err != nil {
		return true
	}
	dueAfter := w.options.RefreshAfter + stableRefreshJitter(name, w.options.JitterWindow)
	return !now.Before(collectedAt.UTC().Add(dueAfter))
}

func (w *RefreshWorker) retryPending(name string, now time.Time) bool {
	retry, ok := w.retries[name]
	return ok && now.Before(retry.nextAttempt)
}

func (w *RefreshWorker) recordRetry(name string, state RefreshAttemptState, now time.Time) {
	switch state {
	case RefreshAttemptSucceeded:
		delete(w.retries, name)
	default:
		retry := w.retries[name]
		retry.failures++
		delay := w.options.RetryBase
		for i := 1; i < retry.failures && delay < w.options.RetryMax; i++ {
			delay *= 2
			if delay >= w.options.RetryMax {
				delay = w.options.RetryMax
				break
			}
		}
		retry.nextAttempt = now.Add(delay)
		w.retries[name] = retry
	}
}

func FactsHealthComplete(facts CollectedFacts) bool {
	for _, value := range []string{facts.DiskStatus, facts.AptStatus} {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || value == "unknown" {
			return false
		}
	}
	return true
}

func stableRefreshJitter(name string, window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(name))
	return time.Duration(hash.Sum64() % uint64(window))
}
