package health

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestRefreshWorkerRunOnceRefreshesOnlyDueHostsAndBacksOffFailures(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	facts := map[string]CollectedFacts{
		"fresh": {ServerName: "fresh", CollectedAt: now.Add(-time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
		"stale": {ServerName: "stale", CollectedAt: now.Add(-25 * time.Hour).Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"},
	}
	var attempts []string
	var outcomes []RefreshAttemptState
	worker := NewRefreshWorker(RefreshWorkerDeps{
		Now: func() time.Time { return now },
		SnapshotServers: func() []servers.Server {
			return []servers.Server{{Name: "stale"}, {Name: "fresh"}, {Name: "missing"}}
		},
		LatestFacts: func() (map[string]CollectedFacts, error) {
			result := make(map[string]CollectedFacts, len(facts))
			for name, record := range facts {
				result[name] = record
			}
			return result, nil
		},
		Refresh: func(_ context.Context, server servers.Server) RefreshAttempt {
			attempts = append(attempts, server.Name)
			if server.Name == "missing" {
				return RefreshAttempt{State: RefreshAttemptFailed, Err: errors.New("dial failed")}
			}
			record := CollectedFacts{ServerName: server.Name, CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"}
			facts[server.Name] = record
			return RefreshAttempt{State: RefreshAttemptSucceeded, Facts: record}
		},
		ObserveAttempt: func(_ servers.Server, attempt RefreshAttempt) {
			outcomes = append(outcomes, attempt.State)
		},
	}, RefreshWorkerOptions{
		RefreshAfter:  20 * time.Hour,
		SweepInterval: time.Hour,
		RetryBase:     15 * time.Minute,
		RetryMax:      time.Hour,
	})

	worker.RunOnce(context.Background())
	if !reflect.DeepEqual(attempts, []string{"missing", "stale"}) {
		t.Fatalf("attempts = %v, want missing and stale in stable order", attempts)
	}
	if !reflect.DeepEqual(outcomes, []RefreshAttemptState{RefreshAttemptFailed, RefreshAttemptSucceeded}) {
		t.Fatalf("outcomes = %v", outcomes)
	}

	worker.RunOnce(context.Background())
	if !reflect.DeepEqual(attempts, []string{"missing", "stale"}) {
		t.Fatalf("attempts after immediate rerun = %v, want failed host backed off and successful host fresh", attempts)
	}

	now = now.Add(15 * time.Minute)
	worker.RunOnce(context.Background())
	if !reflect.DeepEqual(attempts, []string{"missing", "stale", "missing"}) {
		t.Fatalf("attempts after retry base = %v, want missing host retried", attempts)
	}
}

func TestRefreshWorkerRetriesIncompleteFactsWithBackoff(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	attempts := 0
	worker := NewRefreshWorker(RefreshWorkerDeps{
		Now:             func() time.Time { return now },
		SnapshotServers: func() []servers.Server { return []servers.Server{{Name: "partial"}} },
		LatestFacts:     func() (map[string]CollectedFacts, error) { return map[string]CollectedFacts{}, nil },
		Refresh: func(context.Context, servers.Server) RefreshAttempt {
			attempts++
			return RefreshAttempt{
				State: RefreshAttemptSucceeded,
				Facts: CollectedFacts{ServerName: "partial", CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "unknown"},
			}
		},
	}, RefreshWorkerOptions{RefreshAfter: 20 * time.Hour, SweepInterval: time.Hour, RetryBase: 10 * time.Minute, RetryMax: time.Hour})

	worker.RunOnce(context.Background())
	worker.RunOnce(context.Background())
	if attempts != 1 {
		t.Fatalf("attempts = %d, want incomplete result backed off", attempts)
	}
}

func TestRefreshWorkerBacksOffDeferredHosts(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	attempts := 0
	worker := NewRefreshWorker(RefreshWorkerDeps{
		Now:             func() time.Time { return now },
		SnapshotServers: func() []servers.Server { return []servers.Server{{Name: "busy"}} },
		LatestFacts:     func() (map[string]CollectedFacts, error) { return map[string]CollectedFacts{}, nil },
		Refresh: func(context.Context, servers.Server) RefreshAttempt {
			attempts++
			return RefreshAttempt{State: RefreshAttemptDeferred, ReasonCode: "busy"}
		},
	}, RefreshWorkerOptions{RefreshAfter: 20 * time.Hour, SweepInterval: time.Hour, RetryBase: 15 * time.Minute, RetryMax: time.Hour})

	worker.RunOnce(context.Background())
	worker.RunOnce(context.Background())
	if attempts != 1 {
		t.Fatalf("attempts = %d, want deferred host backed off", attempts)
	}
	now = now.Add(15 * time.Minute)
	worker.RunOnce(context.Background())
	if attempts != 2 {
		t.Fatalf("attempts after retry base = %d, want deferred host retried", attempts)
	}
}

func TestRefreshWorkerResetsBackoffWhenAnotherPathRefreshesFacts(t *testing.T) {
	now := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	facts := map[string]CollectedFacts{}
	attempts := 0
	worker := NewRefreshWorker(RefreshWorkerDeps{
		Now:             func() time.Time { return now },
		SnapshotServers: func() []servers.Server { return []servers.Server{{Name: "shared"}} },
		LatestFacts:     func() (map[string]CollectedFacts, error) { return facts, nil },
		Refresh: func(context.Context, servers.Server) RefreshAttempt {
			attempts++
			return RefreshAttempt{State: RefreshAttemptFailed, Err: errors.New("dial failed")}
		},
	}, RefreshWorkerOptions{RefreshAfter: 20 * time.Hour, SweepInterval: time.Hour, RetryBase: 15 * time.Minute, RetryMax: time.Hour})

	worker.RunOnce(context.Background())
	facts["shared"] = CollectedFacts{ServerName: "shared", CollectedAt: now.Format(time.RFC3339), DiskStatus: "ok", AptStatus: "ok"}
	worker.RunOnce(context.Background())

	now = now.Add(23 * time.Hour)
	worker.RunOnce(context.Background())
	now = now.Add(15 * time.Minute)
	worker.RunOnce(context.Background())
	if attempts != 3 {
		t.Fatalf("attempts = %d, want external fresh facts to reset retry backoff", attempts)
	}
}
