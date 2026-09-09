package policies

import (
	"strings"
	"testing"
)

func TestRolloutOriginsExcludeHistoryOutsideActiveHorizon(t *testing.T) {
	repo, _ := newTestRepository(t)
	for _, fixture := range []Run{
		{PolicyID: 91, ServerName: "canary", ScheduledForUTC: "2025-01-01T03:00:00.000000000Z", Status: RunSucceeded},
		{PolicyID: 91, ServerName: "canary", ScheduledForUTC: "2026-09-07T03:00:00.000000000Z", Status: RunSucceeded},
		{PolicyID: 91, ServerName: "wave", ScheduledForUTC: "2026-09-07T03:00:00.000000000Z", Status: RunSucceeded},
		{PolicyID: 91, ServerName: "canary", ScheduledForUTC: "2026-09-08T03:00:00.000000000Z", Status: RunSucceeded},
		{PolicyID: 92, ServerName: "canary", ScheduledForUTC: "2026-09-07T03:00:00.000000000Z", Status: RunSucceeded},
	} {
		if _, inserted, err := repo.CreateRun(fixture); err != nil || !inserted {
			t.Fatalf("create run: %v", err)
		}
	}
	origins, err := repo.ListRolloutOrigins([]RolloutOriginRange{{PolicyID: 91, FromUTC: "2026-09-07T03:00:00.000000000Z", BeforeUTC: "2026-09-08T03:00:00.000000000Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(origins) != 1 || origins[0].PolicyID != 91 || origins[0].ScheduledForUTC != "2026-09-07T03:00:00.000000000Z" {
		t.Fatalf("origins=%+v, want only prior occurrence within active horizon", origins)
	}
}

func TestRolloutOriginQueryUsesBoundedIndexScan(t *testing.T) {
	_, db := newTestRepository(t)
	rows, err := db.Query("EXPLAIN QUERY PLAN "+rolloutOriginsQuery, 91, "2026-09-07T03:00:00.000000000Z", "2026-09-08T03:00:00.000000000Z")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan += detail + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(plan, "idx_update_policy_runs_rollout_scope") || !strings.Contains(plan, "scheduled_for_utc>?") || !strings.Contains(plan, "scheduled_for_utc<?") {
		t.Fatalf("query did not use timestamp bounds in index: %s", plan)
	}
}
