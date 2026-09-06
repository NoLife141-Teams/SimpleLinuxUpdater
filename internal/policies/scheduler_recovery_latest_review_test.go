package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessMissedDueSlotDoesNotLetDelayedSelectedWaveCompeteAtOrigin(t *testing.T) {
	high := Policy{
		ID: 71, Name: "high rollout", Enabled: true, TargetTag: "prod",
		PackageScope: PackageScopeFull, ExecutionMode: ExecutionApprovalRequired,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
		RolloutMode: RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5,
	}
	low := Policy{
		ID: 72, Name: "low immediate", Enabled: true, TargetServers: []string{"srv-b"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	origin := time.Date(2026, 1, 5, 3, 0, 0, 0, time.UTC)
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{high, low}, nil }
	deps.ListRolloutRuns = func([]RolloutRunScope) ([]Run, error) { return nil, nil }
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{
			{Name: "srv-a", Tags: []string{"prod"}},
			{Name: "srv-b", Tags: []string{"prod"}},
		}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}

	service := NewService(deps)
	selected := map[int64]struct{}{high.ID: {}, low.ID: {}}
	if err := service.processMissedDueSlotWithStore(origin, RunReasonSchedulerMissed, selected, SchedulerWatermarkStore{}); err != nil {
		t.Fatalf("processMissedDueSlotWithStore() error = %v", err)
	}
	if len(handled) != 3 {
		t.Fatalf("handled = %+v, want high canary + high delayed closure + low immediate", handled)
	}

	foundLow := false
	foundDelayedHigh := false
	for _, req := range handled {
		if req.Policy.ID == low.ID && req.Server.Name == "srv-b" {
			foundLow = true
			if req.Outcome != RunReasonSchedulerMissed {
				t.Fatalf("low srv-b outcome = %q, want %q; delayed high wave must not compete at origin", req.Outcome, RunReasonSchedulerMissed)
			}
		}
		if req.Policy.ID == high.ID && req.Server.Name == "srv-b" {
			foundDelayedHigh = true
			if req.Outcome != RunReasonSchedulerMissed {
				t.Fatalf("high delayed srv-b outcome = %q, want recovery closure %q", req.Outcome, RunReasonSchedulerMissed)
			}
		}
	}
	if !foundLow || !foundDelayedHigh {
		t.Fatalf("handled = %+v, want both low srv-b and delayed high srv-b rows", handled)
	}
}

func TestSQLiteSchedulerStateRevisionDetectsChangeThenRevert(t *testing.T) {
	repo, db := newTestRepository(t)
	if err := servers.EnsureSchema(db); err != nil {
		t.Fatalf("servers.EnsureSchema() error = %v", err)
	}
	initialRevision, err := repo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("LoadSchedulerStateRevision() initial error = %v", err)
	}
	if _, err := db.Exec(`INSERT INTO servers(name, host, port, user, pass_enc, key_enc, key_path, tags) VALUES('srv-a', '127.0.0.1', 22, 'root', '', '', '', 'prod')`); err != nil {
		t.Fatalf("insert server: %v", err)
	}
	afterInsert, err := repo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("LoadSchedulerStateRevision() after insert error = %v", err)
	}
	if afterInsert <= initialRevision {
		t.Fatalf("revision after insert = %d, want > %d", afterInsert, initialRevision)
	}
	if _, err := db.Exec(`UPDATE servers SET tags = 'staging' WHERE name = 'srv-a'`); err != nil {
		t.Fatalf("temporarily change server tags: %v", err)
	}
	if _, err := db.Exec(`UPDATE servers SET tags = 'prod' WHERE name = 'srv-a'`); err != nil {
		t.Fatalf("revert server tags: %v", err)
	}
	finalRevision, err := repo.LoadSchedulerStateRevision()
	if err != nil {
		t.Fatalf("LoadSchedulerStateRevision() final error = %v", err)
	}
	if finalRevision < afterInsert+2 {
		t.Fatalf("revision after change+revert = %d, want at least %d", finalRevision, afterInsert+2)
	}
	var tags string
	if err := db.QueryRow(`SELECT tags FROM servers WHERE name = 'srv-a'`).Scan(&tags); err != nil {
		t.Fatalf("read reverted server tags: %v", err)
	}
	if tags != "prod" {
		t.Fatalf("final tags = %q, want endpoint state reverted to prod", tags)
	}
}

func TestProcessDueWithRecoveryRebasesAfterStateRevisionChangesAndReverts(t *testing.T) {
	policy := Policy{
		ID: 73, Name: "revision guarded", Enabled: true, TargetServers: []string{"srv-a"},
		PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
		CadenceKind: CadenceDaily, TimeLocal: "03:00",
		CreatedAt: "2026-01-01T00:00:00Z", UpdatedAt: "2026-01-01T00:00:00Z",
	}
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) { return []Policy{policy}, nil }
	deps.SnapshotServers = func() []servers.Server { return []servers.Server{{Name: "srv-a", Tags: []string{"prod"}}} }
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	baseFingerprint, err := service.schedulerRecoveryStateFingerprint()
	if err != nil {
		t.Fatalf("schedulerRecoveryStateFingerprint() error = %v", err)
	}

	watermark := time.Date(2026, 1, 5, 2, 59, 0, 0, time.UTC)
	const currentRevision int64 = 3
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error { watermark = value; return nil },
		LoadStateFingerprint: func() (string, bool, error) {
			// Endpoint state is byte-for-byte equal, but two mutations occurred
			// since the checkpoint (change + revert), so revision 1 is stale.
			return baseFingerprint + ":rev:1", true, nil
		},
		SaveStateFingerprint: func(string) error { return nil },
		LoadStateRevision:    func() (int64, error) { return currentRevision, nil },
	}
	now := time.Date(2026, 1, 5, 4, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(now, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 0 {
		t.Fatalf("handled = %+v, want no historical rows after intervening change+revert", handled)
	}
	if want := now.UTC().Truncate(time.Minute); !watermark.Equal(want) {
		t.Fatalf("watermark = %v, want conservative rebase %v", watermark, want)
	}
}
