package policies

import (
	"errors"
	"testing"
	"time"
)

func TestSchedulerInstallationHealthUsesOwnerClock(t *testing.T) {
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	service := NewService(ServiceDeps{Now: func() time.Time { return now }, ListPolicies: func() ([]Policy, error) { return nil, errors.New("private DB error") }})
	if err := service.ProcessDue(now); err == nil {
		t.Fatal("expected error")
	}
	if got := service.WorkerHealth(); !got.Enabled || got.Running || !got.LastTick.Equal(now) || !got.LastError.Equal(now) || !got.LastProgress.IsZero() || got.Errors != 1 {
		t.Fatal(got)
	}
	now = now.Add(time.Minute)
	if err := service.ProcessDueWithRecovery(now, SchedulerWatermarkStore{Load: func() (time.Time, bool, error) { return time.Time{}, false, errors.New("private checkpoint") }, Save: func(time.Time) error { return nil }}); err == nil {
		t.Fatal("expected recovery error")
	}
	if got := service.WorkerHealth(); got.Errors != 2 || !got.LastError.Equal(now) {
		t.Fatal(got)
	}
}
