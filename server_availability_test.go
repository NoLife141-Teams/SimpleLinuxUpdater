package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	healthpkg "debian-updater/internal/health"
	maintenancepkg "debian-updater/internal/maintenance"
	policypkg "debian-updater/internal/policies"
	scheduledrunspkg "debian-updater/internal/scheduledruns"
	serverpkg "debian-updater/internal/servers"
)

func TestServerAvailabilityAPIBlocksMaintenanceAndPreservesConfiguration(t *testing.T) {
	app, cookie := newActionAPITestApp(t, filepath.Join(t.TempDir(), "availability.db"))
	server := Server{Name: "srv", Host: "example.org", User: "root", Pass: "private-password", Tags: []string{"prod"}}
	if _, err := app.Deps.ServerInventoryService.Create(server); err != nil {
		t.Fatal(err)
	}
	request := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(cookie)
		markSameOriginAuthRequest(req)
		rec := httptest.NewRecorder()
		app.Handler.ServeHTTP(rec, req)
		return rec
	}
	for _, body := range []string{`{}`, `{"disabled":null}`, `{"disabled":"true"}`} {
		if rec := request(http.MethodPut, "/api/servers/srv/availability", body); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid body %s: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	if rec := request(http.MethodPut, "/api/servers/missing/availability", `{"disabled":true}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing server: %d %s", rec.Code, rec.Body.String())
	}
	rec := request(http.MethodPut, "/api/servers/srv/availability", `{"disabled":true}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"disabled":true`) || strings.Contains(rec.Body.String(), server.Pass) {
		t.Fatalf("disable response: %d %s", rec.Code, rec.Body.String())
	}
	stored, err := (serverpkg.SQLiteRepository{DB: app.Deps.DB, Decrypt: decryptSecret}).Load()
	if err != nil || len(stored) != 1 || !stored[0].Disabled || stored[0].Pass != server.Pass {
		t.Fatalf("persisted availability/configuration: %+v, %v", stored, err)
	}
	for _, path := range []string{"/api/update/srv", "/api/autoremove/srv", "/api/servers/srv/facts/refresh"} {
		rec := request(http.MethodPost, path, `{}`)
		if rec.Code != http.StatusConflict || !strings.Contains(strings.ToLower(rec.Body.String()), "disabled") {
			t.Fatalf("disabled %s: %d %s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := request(http.MethodGet, "/api/servers", ""); !strings.Contains(rec.Body.String(), `"disabled":true`) {
		t.Fatalf("inventory omitted disabled server: %s", rec.Body.String())
	}
	var jobCount int
	if err := app.Deps.DB().QueryRow("SELECT COUNT(*) FROM jobs").Scan(&jobCount); err != nil || jobCount != 0 {
		t.Fatalf("disabled maintenance created %d jobs, %v", jobCount, err)
	}
	if rec := request(http.MethodPut, "/api/servers/srv/availability", `{"disabled":false}`); rec.Code != http.StatusOK {
		t.Fatalf("enable response: %d %s", rec.Code, rec.Body.String())
	}
	for _, action := range []string{"server.disable", "server.enable"} {
		audits, err := app.Deps.AuditService.List(AuditListFilter{Action: action, TargetName: server.Name})
		if err != nil || audits.Total != 1 || audits.Items[0].Status != "success" {
			t.Fatalf("%s audit = %+v, %v", action, audits, err)
		}
	}
	if _, err := app.Deps.ServerState.BeginAction(server.Name, "updating"); err != nil {
		t.Fatalf("enabled server could not be admitted: %v", err)
	}
	if rec := request(http.MethodPut, "/api/servers/srv/availability", `{"disabled":true}`); rec.Code != http.StatusConflict {
		t.Fatalf("disabled an active server: %d %s", rec.Code, rec.Body.String())
	}
}

func TestScheduledRunRechecksServerAvailabilityBeforeDispatch(t *testing.T) {
	for _, mode := range []string{policypkg.ExecutionScanOnly, policypkg.ExecutionApprovalRequired} {
		t.Run(mode, func(t *testing.T) {
			server := Server{Name: "srv", Host: "host", User: "root"}
			deps, policy, run, _ := newScheduledRunLifecycleTestDeps(t, "disabled-schedule.db", server, "idle")
			policy.ExecutionMode = mode
			// The candidate and readiness snapshot were accepted before disable.
			deps.AcquireScheduledAction = func(string) func() {
				deps.ServerState.Lock()
				deps.ServerState.Servers()[0].Disabled = true
				deps.ServerState.Unlock()
				return func() {}
			}
			deps.StartJobRunner = func(string, func(), ...func()) { t.Fatal("disabled server dispatched a job") }
			scheduledrunspkg.New(deps).ExecuteRun(run, policy, server)
			stored := getScheduledLifecycleRun(t, deps, run.ID)
			if stored.Status != policypkg.RunSkipped || stored.JobID != "" || !strings.Contains(stored.Summary, "disabled") {
				t.Fatalf("scheduled run = %+v", stored)
			}
			audits, err := deps.AuditService.List(AuditListFilter{Action: "schedule.run.skipped", TargetName: server.Name})
			if err != nil || audits.Total != 1 {
				t.Fatalf("skip audit = %+v, %v", audits, err)
			}
			encoded, _ := json.Marshal(audits.Items[0])
			if !strings.Contains(string(encoded), "server_disabled") || !strings.Contains(string(encoded), "scheduled_for_utc") {
				t.Fatalf("skip audit missing metadata: %s", encoded)
			}
		})
	}
}

func TestAutomaticRefreshRechecksServerAvailabilityBeforeSSH(t *testing.T) {
	state := newServerState()
	server := Server{Name: "srv", Host: "host", User: "root"}
	state.SetServers([]Server{server})
	state.SetStatusMap(map[string]*ServerStatus{server.Name: serverpkg.NewIdleStatus(server)})
	coordinator := maintenancepkg.NewCoordinator(maintenancepkg.Deps{Store: maintenancepkg.NewMemoryStore()})
	if err := coordinator.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	deps := AppDeps{
		ServerState: state, MaintenanceCoordinator: coordinator,
		UpdateService: NewUpdateService(UpdateServiceDeps{
			HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
				t.Fatal("disabled server opened an SSH session")
				return nil, nil
			}),
		}),
		MaintenanceReadiness: func([]Server) map[string]serverpkg.MaintenanceReadiness {
			state.Lock()
			state.Servers()[0].Disabled = true
			state.Unlock()
			return map[string]serverpkg.MaintenanceReadiness{server.Name: {Ready: true}}
		},
	}
	attempt := automaticHostFactsRefreshAttempt(context.Background(), deps, server)
	if attempt.State != healthpkg.RefreshAttemptDeferred || attempt.ReasonCode != serverpkg.MaintenanceReadinessDisabled {
		t.Fatalf("refresh attempt = %+v", attempt)
	}
}
