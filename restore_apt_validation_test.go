package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

func TestRestoredAPTProofFencesAdmissionUntilFreshResume(t *testing.T) {
	var clock atomic.Int64
	clock.Store(time.Now().Unix())
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "servers.db"), AppDeps{
		Now: func() time.Time { return time.Unix(clock.Load(), 0).UTC() },
	})
	admin := app.authenticate(t)
	if _, err := app.Deps.AuthService.CreateAccount("operator", testPasswordStrong, "operator"); err != nil {
		t.Fatal(err)
	}
	operator := identityLogin(t, app, "operator")
	inventory := app.Deps.ServerInventoryService
	hosts := make([]Server, 0, 2)
	for _, name := range []string{"fresh", "expired"} {
		server, err := inventory.Create(Server{Name: name, Host: name + ".example.test", User: "root"})
		if err != nil {
			t.Fatal(err)
		}
		hosts = append(hosts, server)
		if _, err := app.Deps.DB().Exec("INSERT INTO restore_apt_validation(identity) VALUES (?)", server.Identity); err != nil {
			t.Fatal(err)
		}
	}
	if err := app.Deps.RestoreIsolation.Activate(); err != nil {
		t.Fatal(err)
	}
	var dials []string
	fingerprint := "SHA256:validated"
	app.Deps.RestoreAPTValidation.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		dials = append(dials, req.Server.Name)
		if err := req.VerifyHostKey(fingerprint); err != nil {
			return nil, err
		}
		if req.OnConnectedHostKey != nil {
			req.OnConnectedHostKey(fingerprint)
		}
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string { return "healthy" }}, nil
	})
	body := func(server Server) gin.H { return gin.H{"name": server.Name, "identity": server.Identity} }
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, operator, "", body(hosts[0])); res.Code != 403 {
		t.Fatalf("operator APT probe = %d, %s", res.Code, res.Body.String())
	}
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", gin.H{"name": "fresh", "identity": hosts[1].Identity}); res.Code != 409 {
		t.Fatalf("probe with stale selection = %d, %s", res.Code, res.Body.String())
	}
	if len(dials) != 0 {
		t.Fatalf("denied requests connected to %v", dials)
	}
	for _, index := range []int{1, 0} {
		res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", body(hosts[index]))
		if res.Code != 200 || !strings.Contains(res.Body.String(), `"state":"healthy"`) || strings.Contains(res.Body.String(), fingerprint) {
			t.Fatalf("probe = %d, %s", res.Code, res.Body.String())
		}
		if index == 1 {
			clock.Add(int64(restoreAPTProofLifetime/time.Second) + 1)
		}
	}
	res := identityRequest(app, "POST", restoreIsolationResumeRoute, admin, "", gin.H{"confirmation": "RESUME"})
	if res.Code != 200 {
		t.Fatalf("resume = %d, %s", res.Code, res.Body.String())
	}
	v := app.Deps.RestoreAPTValidation
	if got, err := v.authorize(t.Context(), hosts[0]); err != nil || got != fingerprint {
		t.Fatalf("fresh proof = %q, %v", got, err)
	}
	if _, err := v.authorize(t.Context(), hosts[1]); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("expired proof error = %v", err)
	}
	changed := hosts[0]
	changed.Host = "changed.example.test"
	if _, err := v.authorize(t.Context(), changed); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("proof still admitted a changed endpoint: %v", err)
	}
	fingerprint = "SHA256:different"
	if _, err := guardRestoredAPTSession(v).Open(t.Context(), HostMaintenanceSessionRequest{Server: hosts[0], PackageMaintenance: true}); err == nil {
		t.Fatal("maintenance session used a different SSH key than the validated key")
	}
	if _, err := v.authorize(t.Context(), hosts[0]); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("key change did not revoke the proof: %v", err)
	}
	run := scheduledRunLifecycleFromComposedApp(app.Deps).HandleScheduledRun(policypkg.ScheduledRunRequest{
		Policy: policypkg.Policy{ID: 1, Name: "After restore", ExecutionMode: policypkg.ExecutionAutoApply},
		Server: hosts[1], ScheduledForUTC: time.Unix(clock.Load(), 0).UTC().Format(time.RFC3339Nano),
	})
	if run.Err != nil || run.RunID == 0 || run.JobID != "" {
		t.Fatalf("scheduled run = %+v", run)
	}
	stored, err := app.Deps.PolicyRepository.GetRun(run.RunID)
	if err != nil || stored.Status != policypkg.RunSkipped || !strings.Contains(stored.Summary, "APT validation is required") {
		t.Fatalf("scheduled denial = %+v, %v", stored, err)
	}
}
