package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	"debian-updater/internal/policies"
)

func TestPolicySchedules472APIRoundTripsAndConsumedPreview(t *testing.T) {
	app := newIsolatedTestApp(t)
	cookie := app.authenticate(t)
	for index, name := range []string{"a", "b"} {
		if _, e := app.Deps.ServerInventoryService.Create(Server{Name: name, Host: fmt.Sprintf("192.0.2.%d", index+10), User: "fixture", Tags: []string{"prod"}}); e != nil {
			t.Fatal(e)
		}
	}
	request := func(method, url string, body any) *httptest.ResponseRecorder {
		t.Helper()
		encoded, _ := json.Marshal(body)
		req := httptest.NewRequest(method, url, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		markSameOriginAuthRequest(req)
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	var once policies.Policy
	for _, cadence := range []string{policies.CadenceOnce, policies.CadenceMonthly} {
		p := policies.Policy{Name: "API " + cadence, Enabled: true, TargetTag: "prod", ExecutionMode: policies.ExecutionScanOnly, PackageScope: policies.PackageScopeFull, CadenceKind: cadence, DateLocal: "2032-02-29", TimeLocal: "03:00", MonthDay: 31, RolloutMode: policies.RolloutCanaryWaves, CanaryMode: policies.CanaryExplicit, CanaryServers: []string{"b"}, WaveSize: 1, WaveDelayMinutes: 5}
		rec := request(http.MethodPost, "/api/update-policies", p)
		if rec.Code != http.StatusCreated {
			t.Fatalf("create=%d %s", rec.Code, rec.Body)
		}
		var saved policies.Policy
		if e := json.Unmarshal(rec.Body.Bytes(), &saved); e != nil {
			t.Fatal(e)
		}
		if saved.CadenceKind != cadence || saved.CanaryMode != policies.CanaryExplicit || saved.Revision != 1 {
			t.Fatal(saved)
		}
		preview := request(http.MethodPost, "/api/update-policies/preview", saved)
		var facts policies.PreviewResponse
		if e := json.Unmarshal(preview.Body.Bytes(), &facts); e != nil {
			t.Fatal(e)
		}
		if preview.Code != 200 || len(facts.ValidationErrors) != 0 || len(facts.UpcomingOccurrences) == 0 {
			t.Fatalf("preview=%d %s", preview.Code, preview.Body)
		}
		if facts.MatchedServers[1].RolloutStage != "canary" {
			t.Fatal(facts.MatchedServers)
		}
		if cadence == policies.CadenceOnce {
			once = saved
		} else {
			saved.MonthLastDay = true
			rec = request(http.MethodPut, fmt.Sprintf("/api/update-policies/%d", saved.ID), saved)
			if rec.Code != 200 {
				t.Fatal(rec.Body)
			}
			var changed policies.Policy
			if e := json.Unmarshal(rec.Body.Bytes(), &changed); e != nil {
				t.Fatal(e)
			}
			if !changed.MonthLastDay || changed.MonthDay != 0 || changed.Revision != 2 {
				t.Fatal(changed)
			}
		}
	}
	store := app.Deps.PolicyRepository.(policies.OccurrenceStore)
	accepted := policies.Occurrence{Policy: once, Key: "once", ScheduledForUTC: "2032-02-29T03:00:00.000000000Z", Batches: policies.BuildRolloutBatches(once, []string{"a", "b"})}
	if _, e := store.AcceptOccurrence(accepted); e != nil {
		t.Fatal(e)
	}
	once.DateLocal = "2032-03-01"
	rec := request(http.MethodPut, fmt.Sprintf("/api/update-policies/%d", once.ID), once)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	rec = request(http.MethodPost, "/api/update-policies/preview", once)
	var preview policies.PreviewResponse
	if e := json.Unmarshal(rec.Body.Bytes(), &preview); e != nil {
		t.Fatal(e)
	}
	if len(preview.UpcomingOccurrences) != 0 {
		t.Fatal(preview)
	}
	found := false
	for _, fact := range preview.InformationalFacts {
		if fact.Code == "once_consumed" {
			found = true
		}
	}
	if !found {
		t.Fatal(preview)
	}
	rec = request(http.MethodGet, "/api/update-policies/calendar?days=14", nil)
	if rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	var calendar policies.CalendarResponse
	if e := json.Unmarshal(rec.Body.Bytes(), &calendar); e != nil {
		t.Fatal(e)
	}
	if len(calendar.AcceptedRollouts) != 1 || calendar.AcceptedRollouts[0].Policy.Revision != 1 {
		t.Fatal(calendar)
	}
}

func TestPolicySchedules472EncryptedBackupRestorePreservesConsumptionAndManifests(t *testing.T) {
	preserveServerState(t)
	app := newTestApp(t, testAppOptions{DBPath: filepath.Join(t.TempDir(), "schedules-restore.db"), Deps: AppDeps{DB: getDB}})
	app.authenticate(t)
	seedUpdatePolicyTestInventory(t, app, []Server{{Name: "a", Host: "192.0.2.1", Port: 22, User: "fixture", Pass: "test-only", Tags: []string{"prod"}}}, nil)
	if _, e := app.Deps.ApplicationTime.Configure(t.Context(), "UTC"); e != nil {
		t.Fatal(e)
	}
	p := UpdatePolicy{Name: "Backed up once", Enabled: true, TargetTag: "prod", ExecutionMode: policies.ExecutionScanOnly, PackageScope: policies.PackageScopeFull, CadenceKind: policies.CadenceOnce, DateLocal: "2030-10-25", TimeLocal: "03:00", RolloutMode: policies.RolloutCanaryWaves, CanaryMode: policies.CanaryExplicit, CanaryServers: []string{"a"}, WaveSize: 1, WaveDelayMinutes: 5}
	if e := app.Deps.PolicyService.NormalizePolicy(&p); e != nil {
		t.Fatal(e)
	}
	p, e := app.Deps.PolicyRepository.CreatePolicy(p)
	if e != nil {
		t.Fatal(e)
	}
	store := app.Deps.PolicyRepository.(policies.OccurrenceStore)
	original, e := store.AcceptOccurrence(policies.Occurrence{Policy: p, Key: "once", ScheduledForUTC: "2030-10-25T03:00:00.000000000Z", Batches: policies.BuildRolloutBatches(p, []string{"a"})})
	if e != nil {
		t.Fatal(e)
	}
	monthly := p
	monthly.Name = "Monthly backup"
	monthly.CadenceKind = policies.CadenceMonthly
	monthly.MonthLastDay = true
	monthly.DateLocal = ""
	monthly, e = app.Deps.PolicyRepository.CreatePolicy(monthly)
	if e != nil {
		t.Fatal(e)
	}
	archive, e := app.Deps.BackupService.ExportArchive(t.Context(), internalbackup.ExportRequest{Passphrase: "policy schedules test backup"})
	if e != nil {
		t.Fatal(e)
	}
	if e := store.CloseOccurrence(p.ID, "once"); e != nil {
		t.Fatal(e)
	}
	if e := app.Deps.PolicyRepository.DeletePolicy(monthly.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := app.Deps.ApplicationTime.Configure(t.Context(), "Europe/Paris"); e != nil {
		t.Fatal(e)
	}
	if _, e := app.Deps.BackupService.RestoreArchive(t.Context(), archive.Bytes, "policy schedules test backup"); e != nil {
		t.Fatal(e)
	}
	restored, e := app.Deps.PolicyRepository.GetPolicy(p.ID)
	if e != nil || restored.OnceConsumedAt == "" || restored.OnceScheduledForUTC != original.ScheduledForUTC {
		t.Fatalf("restored policy=%+v err=%v", restored, e)
	}
	open, e := store.ListOpenOccurrences()
	if e != nil || len(open) != 1 || !reflect.DeepEqual(open[0], original) {
		t.Fatalf("manifest roundtrip=%+v err=%v", open, e)
	}
	monthly, e = app.Deps.PolicyRepository.GetPolicy(monthly.ID)
	if e != nil || monthly.CadenceKind != policies.CadenceMonthly || !monthly.MonthLastDay {
		t.Fatalf("restored monthly=%+v %v", monthly, e)
	}
	deps := app.Deps.PolicyService.EnsureDeps()
	deps.CurrentLocation = func() *time.Location { return time.FixedZone("-01:00", -3600) }
	deps.ApplicationTime = nil
	calls := 0
	deps.HandleScheduledRun = func(req policies.ScheduledRunRequest) policies.ScheduledRunResult {
		if req.Outcome == "" {
			calls++
		}
		return policies.ScheduledRunResult{Handled: true}
	}
	service := policies.NewService(deps)
	if e := service.ProcessDue(time.Date(2030, 10, 25, 4, 0, 0, 0, time.UTC)); e != nil {
		t.Fatal(e)
	}
	if calls != 0 {
		t.Fatal("restored one-time work replayed")
	}
}
