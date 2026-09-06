package policies

import (
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestProcessDueWithRecoveryDoesNotBackfillDailyPolicyAtWeeklyPolicySlot(t *testing.T) {
	watermark := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var handled []ScheduledRunRequest
	deps := testServiceDeps()
	deps.ListPolicies = func() ([]Policy, error) {
		return []Policy{
			{
				ID: 11, Name: "daily", Enabled: true, TargetServers: []string{"srv-daily"},
				PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
				CadenceKind: CadenceDaily, TimeLocal: "03:00",
			},
			{
				ID: 12, Name: "weekly", Enabled: true, TargetServers: []string{"srv-weekly"},
				PackageScope: PackageScopeSecurity, ExecutionMode: ExecutionScanOnly,
				CadenceKind: CadenceWeekly, TimeLocal: "03:00", Weekdays: []string{"wed"},
			},
		}, nil
	}
	deps.SnapshotServers = func() []servers.Server {
		return []servers.Server{{Name: "srv-daily"}, {Name: "srv-weekly"}}
	}
	deps.HandleScheduledRun = func(req ScheduledRunRequest) ScheduledRunResult {
		handled = append(handled, req)
		return ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := NewService(deps)
	store := SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return watermark, true, nil },
		Save: func(value time.Time) error {
			watermark = value
			return nil
		},
	}

	current := time.Date(2026, 1, 8, 4, 0, 0, 0, time.UTC)
	if err := service.ProcessDueWithRecovery(current, store); err != nil {
		t.Fatalf("ProcessDueWithRecovery() error = %v", err)
	}
	if len(handled) != 2 {
		t.Fatalf("handled = %+v, want one latest occurrence per policy", handled)
	}
	byPolicy := map[int64]ScheduledRunRequest{}
	for _, req := range handled {
		byPolicy[req.Policy.ID] = req
	}
	if got := byPolicy[11]; got.ScheduledForUTC != "2026-01-08T03:00:00.000000000Z" || got.Outcome != RunReasonSchedulerMissed {
		t.Fatalf("daily recovery = %+v, want only Jan 8 latest occurrence", got)
	}
	if got := byPolicy[12]; got.ScheduledForUTC != "2026-01-07T03:00:00.000000000Z" || got.Outcome != RunReasonSchedulerMissed {
		t.Fatalf("weekly recovery = %+v, want Jan 7 weekly occurrence", got)
	}
}
