package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/policies"
)

func TestFailedCanaryRemainsBlockedAfterInventoryEditAndRecovery(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for _, server := range []Server{{Name: "srv-a", Host: "192.0.2.10", User: "review", Tags: []string{"prod"}}, {Name: "srv-b", Host: "192.0.2.11", User: "review", Tags: []string{"prod"}}} {
		if _, err := app.Deps.ServerInventoryService.Create(server); err != nil {
			t.Fatal(err)
		}
	}
	origin := time.Now().UTC().Truncate(24 * time.Hour).Add(24*time.Hour + 3*time.Hour)
	p, err := app.Deps.PolicyRepository.CreatePolicy(UpdatePolicy{Name: "review-rollout", Enabled: true, TargetTag: "prod", ExecutionMode: policies.ExecutionAutoApply, PackageScope: policies.PackageScopeSecurity, CadenceKind: policies.CadenceDaily, TimeLocal: "03:00", RolloutMode: policies.RolloutCanaryWaves, CanaryCount: 1, WaveSize: 1, WaveDelayMinutes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := app.Deps.PolicyRepository.CreateRun(policies.Run{PolicyID: p.ID, PolicyName: p.Name, ServerName: "srv-a", ScheduledForUTC: origin.Format(policies.DefaultTimestampLayout), Status: policies.RunFailed}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPut, "/api/servers/srv-a", strings.NewReader(`{"name":"srv-a","host":"192.0.2.10","user":"review","tags":[]}`))
	req.Header.Set("Content-Type", "application/json")
	markSameOriginAuthRequest(req)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	app.Handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("tag edit rejected: %d %s", rec.Code, rec.Body.String())
	}
	serviceDeps := app.Deps.PolicyService.EnsureDeps()
	serviceDeps.CurrentLocation = func() *time.Location { return time.UTC }
	serviceDeps.ApplicationTime = nil
	productionHandler := serviceDeps.HandleScheduledRun
	var dispatched []policies.ScheduledRunRequest
	serviceDeps.HandleScheduledRun = func(req policies.ScheduledRunRequest) policies.ScheduledRunResult {
		dispatched = append(dispatched, req)
		if req.Outcome != "" {
			return productionHandler(req)
		}
		return policies.ScheduledRunResult{Handled: true, Inserted: true}
	}
	service := policies.NewService(serviceDeps)
	err = service.ProcessDueWithRecovery(origin.Add(5*time.Minute), policies.SchedulerWatermarkStore{
		Load: func() (time.Time, bool, error) { return origin, true, nil }, Save: func(time.Time) error { return nil },
		LoadStateFingerprint: func() (string, bool, error) { return "before-inventory-edit", true, nil }, SaveStateFingerprint: func(string) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := app.Deps.PolicyRepository.FindRun(p.ID, "srv-b", origin.Format(policies.DefaultTimestampLayout))
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != policies.RunSkipped || run.Reason != policies.RunReasonRolloutGate {
		t.Fatalf("run = %+v", run)
	}
	var raw string
	if err := app.Deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action = 'schedule.run.skipped' AND target_name = 'srv-b'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var meta map[string]any
	if err := json.Unmarshal([]byte(raw), &meta); err != nil {
		t.Fatal(err)
	}
	if meta["reason"] != policies.RunReasonRolloutGate || meta["scheduled_for_utc"] != origin.Format(policies.DefaultTimestampLayout) || meta["run_id"] != float64(run.ID) || meta["policy_id"] != float64(p.ID) {
		t.Fatalf("unexpected audit metadata: %v", meta)
	}
	t.Logf("real HTTP tag edit=%d; persisted failed canary retained; dispatched=%d", rec.Code, len(dispatched))
	for _, req := range dispatched {
		t.Logf("server=%s outcome=%q", req.Server.Name, req.Outcome)
		if req.Outcome == "" {
			t.Errorf("recovery path dispatched %s after failed canary", req.Server.Name)
		}
	}
}
