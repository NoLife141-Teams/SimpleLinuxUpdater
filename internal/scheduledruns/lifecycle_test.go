package scheduledruns_test

import (
	"database/sql"
	"testing"
	"time"

	"debian-updater/internal/audit"
	"debian-updater/internal/policies"
	"debian-updater/internal/scheduledruns"
	"debian-updater/internal/servers"

	_ "modernc.org/sqlite"
)

func TestLifecycleHandlesSkippedCandidateIdempotently(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("sql.Open() error = %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	if err := policies.EnsureSchema(db); err != nil {
		t.Fatalf("policies.EnsureSchema() error = %v", err)
	}
	if err := audit.EnsureSchema(db); err != nil {
		t.Fatalf("audit.EnsureSchema() error = %v", err)
	}
	now := time.Date(2026, 7, 12, 18, 0, 0, 0, time.UTC)
	repository := policies.NewSQLiteRepository(policies.SQLiteRepositoryDeps{
		DB:        func() *sql.DB { return db },
		NowString: func() string { return now.Format(time.RFC3339Nano) },
	})
	audits := audit.NewService(audit.ServiceOptions{
		DB:  func() *sql.DB { return db },
		Now: func() time.Time { return now },
	})
	lifecycle := scheduledruns.New(scheduledruns.Deps{
		AuditService:     audits,
		JobTimestampNow:  func() string { return now.Add(time.Second).Format(time.RFC3339Nano) },
		PolicyRepository: repository,
	})
	request := policies.ScheduledRunRequest{
		Policy: policies.Policy{
			ID:            41,
			Name:          "blackout policy",
			ExecutionMode: policies.ExecutionScanOnly,
			PackageScope:  policies.PackageScopeSecurity,
			UpgradeMode:   policies.UpgradeModeStandard,
		},
		Server:          servers.Server{Name: "srv-blackout"},
		ScheduledForUTC: now.Format(time.RFC3339Nano),
		Outcome:         policies.RunReasonBlackout,
	}

	first := lifecycle.HandleScheduledRun(request)
	second := lifecycle.HandleScheduledRun(request)
	if !first.Handled || !first.Inserted || first.Status != policies.RunSkipped {
		t.Fatalf("first HandleScheduledRun() = %+v", first)
	}
	if !second.Handled || second.Inserted || second.RunID != first.RunID {
		t.Fatalf("second HandleScheduledRun() = %+v, want existing run %d", second, first.RunID)
	}
	run, err := repository.GetRun(first.RunID)
	if err != nil {
		t.Fatalf("GetRun(%d) error = %v", first.RunID, err)
	}
	if run.Reason != policies.RunReasonBlackout || run.Summary != "Scheduled run skipped due to blackout window" || run.FinishedAt == "" {
		t.Fatalf("persisted run = %+v", run)
	}
	listed, err := audits.List(audit.ListFilter{Action: "schedule.run.skipped", TargetName: request.Server.Name})
	if err != nil {
		t.Fatalf("AuditService.List() error = %v", err)
	}
	if listed.Total != 1 || listed.Items[0].Status != "ignored" {
		t.Fatalf("audit events = %+v, want one ignored skip", listed.Items)
	}
}
