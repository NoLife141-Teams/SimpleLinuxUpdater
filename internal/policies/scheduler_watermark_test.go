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

func TestSQLiteRepositorySchedulerCheckpointRoundTrip(t *testing.T) {
	repo, _ := newTestRepository(t)
	if checkpoint, found, err := repo.LoadSchedulerCheckpoint(); err != nil || found || !checkpoint.Watermark.IsZero() || checkpoint.StateFingerprint != "" {
		t.Fatalf("initial LoadSchedulerCheckpoint() = %+v, %t, %v; want empty checkpoint", checkpoint, found, err)
	}

	input := SchedulerCheckpoint{
		Watermark:        time.Date(2026, 9, 6, 16, 40, 47, 123, time.UTC),
		StateFingerprint: "checkpoint-fingerprint-v1",
	}
	if err := repo.SaveSchedulerCheckpoint(input); err != nil {
		t.Fatalf("SaveSchedulerCheckpoint() error = %v", err)
	}
	got, found, err := repo.LoadSchedulerCheckpoint()
	if err != nil {
		t.Fatalf("LoadSchedulerCheckpoint() error = %v", err)
	}
	if !found || !got.Watermark.Equal(input.Watermark.UTC().Truncate(time.Minute)) || got.StateFingerprint != input.StateFingerprint {
		t.Fatalf("LoadSchedulerCheckpoint() = %+v, %t; want watermark=%v fingerprint=%q", got, found, input.Watermark.UTC().Truncate(time.Minute), input.StateFingerprint)
	}
}

func TestSQLiteRepositorySchedulerCheckpointRollsBackBothValues(t *testing.T) {
	repo, db := newTestRepository(t)
	if _, err := db.Exec(`
		CREATE TRIGGER fail_scheduler_checkpoint_fingerprint
		BEFORE INSERT ON settings
		WHEN NEW.key = 'update_policy_scheduler_state_fingerprint'
		BEGIN
			SELECT RAISE(ABORT, 'forced checkpoint failure');
		END
	`); err != nil {
		t.Fatalf("create checkpoint failure trigger: %v", err)
	}

	err := repo.SaveSchedulerCheckpoint(SchedulerCheckpoint{
		Watermark:        time.Date(2026, 9, 6, 16, 41, 0, 0, time.UTC),
		StateFingerprint: "must-rollback",
	})
	if err == nil {
		t.Fatal("SaveSchedulerCheckpoint() error = nil, want forced failure")
	}
	if watermark, found, loadErr := repo.LoadSchedulerWatermark(); loadErr != nil || found || !watermark.IsZero() {
		t.Fatalf("watermark after failed checkpoint = %v, %t, %v; want zero, false, nil", watermark, found, loadErr)
	}
	if fingerprint, found, loadErr := repo.LoadSchedulerStateFingerprint(); loadErr != nil || found || fingerprint != "" {
		t.Fatalf("fingerprint after failed checkpoint = %q, %t, %v; want empty, false, nil", fingerprint, found, loadErr)
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
