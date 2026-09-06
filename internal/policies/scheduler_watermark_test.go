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

func TestSQLiteRepositorySchedulerStateFingerprintRoundTrip(t *testing.T) {
	repo, _ := newTestRepository(t)
	if got, found, err := repo.LoadSchedulerStateFingerprint(); err != nil || found || got != "" {
		t.Fatalf("initial LoadSchedulerStateFingerprint() = %q, %t, %v; want empty, false, nil", got, found, err)
	}
	if err := repo.SaveSchedulerStateFingerprint("fingerprint-v1"); err != nil {
		t.Fatalf("SaveSchedulerStateFingerprint() error = %v", err)
	}
	got, found, err := repo.LoadSchedulerStateFingerprint()
	if err != nil {
		t.Fatalf("LoadSchedulerStateFingerprint() error = %v", err)
	}
	if !found || got != "fingerprint-v1" {
		t.Fatalf("LoadSchedulerStateFingerprint() = %q, %t; want fingerprint-v1, true", got, found)
	}
}

func TestSQLiteRepositorySchedulerRecoveryScopeRoundTrip(t *testing.T) {
	repo, _ := newTestRepository(t)
	const policyID int64 = 7
	const scheduled = "2026-09-06T14:35:00.000000000Z"
	if marked, err := repo.HasSchedulerRecoveryScope(policyID, scheduled); err != nil || marked {
		t.Fatalf("initial HasSchedulerRecoveryScope() = %t, %v; want false, nil", marked, err)
	}
	if err := repo.MarkSchedulerRecoveryScope(policyID, scheduled); err != nil {
		t.Fatalf("MarkSchedulerRecoveryScope() error = %v", err)
	}
	marked, err := repo.HasSchedulerRecoveryScope(policyID, scheduled)
	if err != nil {
		t.Fatalf("HasSchedulerRecoveryScope() error = %v", err)
	}
	if !marked {
		t.Fatal("HasSchedulerRecoveryScope() = false, want true")
	}
	if other, err := repo.HasSchedulerRecoveryScope(policyID, "2026-09-07T14:35:00.000000000Z"); err != nil || other {
		t.Fatalf("other recovery scope = %t, %v; want false, nil", other, err)
	}
}
