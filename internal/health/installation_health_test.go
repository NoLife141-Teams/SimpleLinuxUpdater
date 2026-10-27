package health

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRefreshWorkerInstallationHealthUsesOwnerClock(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	failed := true
	worker := NewRefreshWorker(RefreshWorkerDeps{Now: func() time.Time { return now }, LatestFacts: func() (map[string]CollectedFacts, error) {
		if failed {
			return nil, errors.New("private database error")
		}
		return nil, nil
	}}, RefreshWorkerOptions{})
	worker.RunOnce(context.Background())
	got := worker.WorkerHealth(true)
	if got.Errors != 1 || !got.LastTick.Equal(now) || !got.LastError.Equal(now) || !got.LastProgress.IsZero() {
		t.Fatal(got)
	}
	failed = false
	now = now.Add(time.Hour)
	worker.RunOnce(context.Background())
	got = worker.WorkerHealth(false)
	if got.Enabled || got.Running || !got.LastProgress.Equal(now) || got.Errors != 1 {
		t.Fatal(got)
	}
}
