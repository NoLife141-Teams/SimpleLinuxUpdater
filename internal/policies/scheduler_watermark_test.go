package policies

import (
	"testing"
	"time"
)

func TestSQLiteRepositorySchedulerWatermarkRoundTrip(t *testing.T) {
	repo, _ := newTestRepository(t)
	if got, found, err := repo.LoadSchedulerWatermark(); err != nil || found || !got.IsZero() {
		t.Fatalf("initial LoadSchedulerWatermark() = %v, %t, %v; want zero, false, nil", got, found, err)
	}

	input := time.Date(2026, 9, 6, 14, 35, 47, 123, time.UTC)
	if err := repo.SaveSchedulerWatermark(input); err != nil {
		t.Fatalf("SaveSchedulerWatermark() error = %v", err)
	}
	got, found, err := repo.LoadSchedulerWatermark()
	if err != nil {
		t.Fatalf("LoadSchedulerWatermark() error = %v", err)
	}
	want := input.UTC().Truncate(time.Minute)
	if !found || !got.Equal(want) {
		t.Fatalf("LoadSchedulerWatermark() = %v, %t; want %v, true", got, found, want)
	}
}
