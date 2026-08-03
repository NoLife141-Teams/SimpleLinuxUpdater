package policies

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

func newTestRepository(t *testing.T) (*SQLiteRepository, *sql.DB) {
	t.Helper()
	db, err := sql.Open("sqlite", t.TempDir()+"/policies.db")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create settings table: %v", err)
	}
	if err := EnsureSchema(db); err != nil {
		t.Fatalf("EnsureSchema() error = %v", err)
	}
	repo := NewSQLiteRepository(SQLiteRepositoryDeps{
		DB:          func() *sql.DB { return db },
		NowString:   func() string { return "2026-01-02T03:04:05.000000000Z" },
		MarshalJSON: marshalJSON,
	})
	return repo, db
}

func TestSQLiteRepositoryQueryRunsFiltersAndPaginatesDeterministically(t *testing.T) {
	repo, _ := newTestRepository(t)
	fixtures := []Run{
		{PolicyID: 1, PolicyName: "Nightly security", ServerName: "srv-web-01", ScheduledForUTC: "2026-01-04T03:00:00.000000000Z", ExecutionMode: ExecutionScanOnly, PackageScope: PackageScopeSecurity, Status: RunSkipped, Reason: RunReasonBusy},
		{PolicyID: 1, PolicyName: "Nightly security", ServerName: "srv-web-01", ScheduledForUTC: "2026-01-03T03:00:00.000000000Z", ExecutionMode: ExecutionScanOnly, PackageScope: PackageScopeSecurity, Status: RunSucceeded},
		{PolicyID: 2, PolicyName: "Database maintenance", ServerName: "srv-db-01", ScheduledForUTC: "2026-01-02T03:00:00.000000000Z", ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeFull, Status: RunFailed},
		{PolicyID: 1, PolicyName: "Nightly security", ServerName: "srv-web-02", ScheduledForUTC: "2026-01-01T03:00:00.000000000Z", ExecutionMode: ExecutionScanOnly, PackageScope: PackageScopeSecurity, Status: RunSkipped, Reason: RunReasonBlackout},
	}
	for index, fixture := range fixtures {
		if _, inserted, err := repo.CreateRun(fixture); err != nil || !inserted {
			t.Fatalf("CreateRun(%d) = inserted %t, err %v", index, inserted, err)
		}
	}

	page, err := repo.QueryRuns(RunQuery{
		Policy:         "nightly",
		Server:         "web",
		Outcome:        RunSkipped,
		FromUTC:        "2026-01-01T00:00:00.000000000Z",
		ToUTCExclusive: "2026-01-05T00:00:00.000000000Z",
		Page:           1,
		PageSize:       1,
	})
	if err != nil {
		t.Fatalf("QueryRuns() error = %v", err)
	}
	if page.Total != 2 || page.TotalPages != 2 || page.Page != 1 || page.PageSize != 1 {
		t.Fatalf("QueryRuns() metadata = %+v, want total=2 totalPages=2 page=1 pageSize=1", page)
	}
	if len(page.Items) != 1 || page.Items[0].Reason != RunReasonBusy {
		t.Fatalf("QueryRuns() first page = %+v, want newest busy skip", page.Items)
	}

	second, err := repo.QueryRuns(RunQuery{
		Policy:   "Nightly",
		Server:   "srv-web",
		Outcome:  RunSkipped,
		Page:     2,
		PageSize: 1,
	})
	if err != nil {
		t.Fatalf("QueryRuns(page 2) error = %v", err)
	}
	if len(second.Items) != 1 || second.Items[0].Reason != RunReasonBlackout {
		t.Fatalf("QueryRuns() second page = %+v, want older blackout skip", second.Items)
	}

	for index := 0; index < 2; index++ {
		if _, inserted, err := repo.CreateRun(Run{
			PolicyID:        3,
			PolicyName:      "Same instant",
			ServerName:      fmt.Sprintf("srv-same-%d", index),
			ScheduledForUTC: "2026-01-05T03:00:00.000000000Z",
			ExecutionMode:   ExecutionScanOnly,
			PackageScope:    PackageScopeSecurity,
			Status:          RunSucceeded,
		}); err != nil || !inserted {
			t.Fatalf("CreateRun(same instant %d) = inserted %t, err %v", index, inserted, err)
		}
	}
	tied, err := repo.QueryRuns(RunQuery{Policy: "Same instant", Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("QueryRuns(tied) error = %v", err)
	}
	if len(tied.Items) != 2 || tied.Items[0].ID <= tied.Items[1].ID {
		t.Fatalf("QueryRuns(tied) IDs = [%d, %d], want descending deterministic ID order", tied.Items[0].ID, tied.Items[1].ID)
	}
}

func TestSQLiteRepositoryQueryRunsDoesNotPreallocateFromPageSize(t *testing.T) {
	repo, _ := newTestRepository(t)

	page, err := repo.QueryRuns(RunQuery{Page: 1, PageSize: MaxRunsLimit})
	if err != nil {
		t.Fatalf("QueryRuns() error = %v", err)
	}
	if page.Items == nil || cap(page.Items) != 0 {
		t.Fatalf("QueryRuns() empty items = %#v with capacity %d, want non-nil empty slice without request-sized preallocation", page.Items, cap(page.Items))
	}
}

func TestSQLiteRepositoryListRolloutRunsScopesCompleteOccurrences(t *testing.T) {
	repo, db := newTestRepository(t)
	targetTime := "2026-01-05T03:00:00.000000000Z"
	otherTime := "2026-01-06T03:00:00.000000000Z"
	fixtures := []Run{
		{PolicyID: 11, PolicyName: "Target", ServerName: "srv-a", ScheduledForUTC: targetTime, ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, Status: RunSucceeded},
		{PolicyID: 11, PolicyName: "Target", ServerName: "srv-b", ScheduledForUTC: targetTime, ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, Status: RunRunning},
		{PolicyID: 11, PolicyName: "Other occurrence", ServerName: "srv-a", ScheduledForUTC: otherTime, ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, Status: RunSucceeded},
		{PolicyID: 12, PolicyName: "Other policy", ServerName: "srv-a", ScheduledForUTC: targetTime, ExecutionMode: ExecutionAutoApply, PackageScope: PackageScopeSecurity, Status: RunSucceeded},
	}
	for index, fixture := range fixtures {
		if _, inserted, err := repo.CreateRun(fixture); err != nil || !inserted {
			t.Fatalf("CreateRun(%d) = inserted %t, err %v", index, inserted, err)
		}
	}

	runs, err := repo.ListRolloutRuns([]RolloutRunScope{
		{PolicyID: 11, ScheduledForUTC: targetTime},
		{PolicyID: 11, ScheduledForUTC: targetTime},
	})
	if err != nil {
		t.Fatalf("ListRolloutRuns() error = %v", err)
	}
	if len(runs) != 2 || runs[0].ServerName != "srv-a" || runs[1].ServerName != "srv-b" {
		t.Fatalf("ListRolloutRuns() = %+v, want complete target occurrence", runs)
	}

	var plan string
	rows, err := db.Query("EXPLAIN QUERY PLAN SELECT server_name FROM update_policy_runs WHERE policy_id = ? AND scheduled_for_utc = ?", 11, targetTime)
	if err != nil {
		t.Fatalf("EXPLAIN QUERY PLAN error = %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		plan += detail + "\n"
	}
	if !strings.Contains(plan, "idx_update_policy_runs_rollout_scope") {
		t.Fatalf("rollout query plan does not use scoped index:\n%s", plan)
	}
}

func TestSQLiteRepositoryListRolloutRunsRejectsInvalidScope(t *testing.T) {
	repo, _ := newTestRepository(t)
	if _, err := repo.ListRolloutRuns([]RolloutRunScope{{PolicyID: 0, ScheduledForUTC: "2026-01-05T03:00:00Z"}}); err == nil {
		t.Fatal("ListRolloutRuns() error = nil, want invalid scope rejection")
	}
}

func TestSQLiteRepositoryPolicyCRUDOverridesAndRuns(t *testing.T) {
	repo, _ := newTestRepository(t)
	policy, err := repo.CreatePolicy(Policy{
		Name:             "Nightly",
		Enabled:          true,
		TargetServers:    []string{"srv-a"},
		PackageScope:     PackageScopeSecurity,
		ExecutionMode:    ExecutionScanOnly,
		RolloutMode:      RolloutCanaryWaves,
		CanaryCount:      1,
		WaveSize:         4,
		WaveDelayMinutes: 12,
		CadenceKind:      CadenceDaily,
		TimeLocal:        "03:00",
		Weekdays:         []string{},
		PolicyBlackouts:  []BlackoutWindow{},
	})
	if err != nil {
		t.Fatalf("CreatePolicy() error = %v", err)
	}
	if policy.ID == 0 || policy.Name != "Nightly" || len(policy.TargetServers) != 1 {
		t.Fatalf("CreatePolicy() = %+v, want persisted policy", policy)
	}
	if policy.UpgradeMode != UpgradeModeStandard {
		t.Fatalf("CreatePolicy().UpgradeMode = %q, want default %q", policy.UpgradeMode, UpgradeModeStandard)
	}
	if policy.RolloutMode != RolloutCanaryWaves || policy.CanaryCount != 1 || policy.WaveSize != 4 || policy.WaveDelayMinutes != 12 {
		t.Fatalf("CreatePolicy() rollout = %+v", policy)
	}

	policy.Name = "Morning"
	policy.UpgradeMode = UpgradeModeFull
	updated, err := repo.UpdatePolicy(policy.ID, policy)
	if err != nil {
		t.Fatalf("UpdatePolicy() error = %v", err)
	}
	if updated.Name != "Morning" {
		t.Fatalf("UpdatePolicy().Name = %q, want Morning", updated.Name)
	}
	if updated.UpgradeMode != UpgradeModeFull {
		t.Fatalf("UpdatePolicy().UpgradeMode = %q, want %q", updated.UpgradeMode, UpgradeModeFull)
	}

	override, err := repo.SetOverride(policy.ID, "srv-a", true)
	if err != nil {
		t.Fatalf("SetOverride(true) error = %v", err)
	}
	if !override.Disabled {
		t.Fatalf("override.Disabled = false, want true")
	}
	allOverrides, err := repo.LoadAllOverrides()
	if err != nil {
		t.Fatalf("LoadAllOverrides() error = %v", err)
	}
	if !allOverrides[policy.ID]["srv-a"] {
		t.Fatalf("LoadAllOverrides() = %+v, want disabled override", allOverrides)
	}

	run, inserted, err := repo.CreateRun(Run{
		PolicyID:        policy.ID,
		PolicyName:      policy.Name,
		ServerName:      "srv-a",
		ScheduledForUTC: "2026-01-02T03:00:00.000000000Z",
		ExecutionMode:   ExecutionScanOnly,
		PackageScope:    PackageScopeSecurity,
	})
	if err != nil || !inserted {
		t.Fatalf("CreateRun() = (%+v, %t, %v), want inserted", run, inserted, err)
	}
	if run.UpgradeMode != UpgradeModeStandard {
		t.Fatalf("CreateRun().UpgradeMode = %q, want default %q", run.UpgradeMode, UpgradeModeStandard)
	}
	summary := "done"
	status := RunSucceeded
	if err := repo.UpdateRun(run.ID, RunUpdate{Status: &status, Summary: &summary}); err != nil {
		t.Fatalf("UpdateRun() error = %v", err)
	}
	gotRun, err := repo.GetRun(run.ID)
	if err != nil {
		t.Fatalf("GetRun() error = %v", err)
	}
	if gotRun.Status != RunSucceeded || gotRun.Summary != "done" {
		t.Fatalf("GetRun() = %+v, want updated status/summary", gotRun)
	}
	if gotRun.UpgradeMode != UpgradeModeStandard {
		t.Fatalf("GetRun().UpgradeMode = %q, want %q", gotRun.UpgradeMode, UpgradeModeStandard)
	}
}

func TestSQLiteRepositoryGlobalBlackoutsRoundTrip(t *testing.T) {
	repo, _ := newTestRepository(t)
	windows, err := repo.SaveGlobalBlackouts([]BlackoutWindow{{
		Weekdays:  []string{"Monday"},
		StartTime: "22:00",
		EndTime:   "02:00",
	}})
	if err != nil {
		t.Fatalf("SaveGlobalBlackouts() error = %v", err)
	}
	if windows[0].Weekdays[0] != "mon" {
		t.Fatalf("normalized weekday = %q, want mon", windows[0].Weekdays[0])
	}
	loaded, err := repo.LoadGlobalBlackouts()
	if err != nil {
		t.Fatalf("LoadGlobalBlackouts() error = %v", err)
	}
	if len(loaded) != 1 || loaded[0].StartTime != "22:00" || loaded[0].EndTime != "02:00" {
		t.Fatalf("LoadGlobalBlackouts() = %+v", loaded)
	}
}
