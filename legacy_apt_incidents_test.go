package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

func TestLegacyAPTIncidentGuardsMaintenanceUntilExplicitVerification(t *testing.T) {
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	inventory := app.Deps.ServerInventoryService
	affected, err := inventory.Create(Server{Name: "legacy-host", Host: "legacy.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	unaffected, err := inventory.Create(Server{Name: "resolved-host", Host: "resolved.example.test", User: "root"})
	if err != nil {
		t.Fatal(err)
	}
	db := app.Deps.DB()
	const id = "legacy-interrupted-job"
	for _, job := range []struct{ id, name, status, class, at string }{
		{id, affected.Name, "failed", "reconciliation_required", "2026-01-01T00:00:00Z"},
		{"legacy-resolved", unaffected.Name, "failed", "reconciliation_required", "2026-01-01T00:00:00Z"},
		{"legacy-success", unaffected.Name, "succeeded", "", "2026-01-02T00:00:00Z"},
	} {
		if _, err := db.Exec(`INSERT INTO jobs(id,kind,server_name,actor,status,phase,error_class,created_at,updated_at)
			VALUES(?,'update',?,'legacy-admin',?,'complete',?,?,?)`, job.id, job.name, job.status, job.class, job.at, job.at); err != nil {
			t.Fatal(err)
		}
	}
	if err := ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	v := app.Deps.RestoreAPTValidation
	if err := v.admit(unaffected); err != nil {
		t.Fatalf("resolved historical incident blocked an unrelated host: %v", err)
	}
	if res := identityRequest(app, "POST", "/api/autoremove/"+affected.Name, admin, "", nil); res.Code != 409 ||
		!strings.Contains(res.Body.String(), "restore_apt_validation_required") {
		t.Fatalf("manual admission=%d: %s", res.Code, res.Body.String())
	}
	run := scheduledRunLifecycleFromComposedApp(app.Deps).HandleScheduledRun(policypkg.ScheduledRunRequest{
		Policy: policypkg.Policy{ID: 1, Name: "Legacy fence", ExecutionMode: policypkg.ExecutionAutoApply},
		Server: affected, ScheduledForUTC: app.Deps.Now().UTC().Format(time.RFC3339Nano),
	})
	if run.Err != nil || run.JobID != "" || run.RunID == 0 {
		t.Fatalf("scheduled admission=%+v", run)
	}
	stored, err := app.Deps.PolicyRepository.GetRun(run.RunID)
	if err != nil || stored.Status != policypkg.RunSkipped || !strings.Contains(stored.Summary, "APT validation is required") {
		t.Fatalf("scheduled admission reason=%+v, %v", stored, err)
	}

	facts := v.hostFacts(t.Context(), app.Deps.ServerState, app.Deps.Now(), false)
	if len(facts) != 1 || facts[0].State != "legacy_target_unverified" ||
		facts[0].LegacyReportURL != "/api/reports/jobs/"+id {
		t.Fatalf("legacy incident is missing from installation health: %+v", facts)
	}
	var calls int
	outcome := "dpkg_incomplete"
	v.sessions = HostMaintenanceSessionFactoryFunc(func(_ context.Context, req HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		calls++
		req.OnConnectedHostKey("SHA256:synthetic-verified-key")
		return &HostMaintenanceSessionFuncs{CheckRestoredAPTFunc: func(context.Context) string { return outcome }}, nil
	})
	check := gin.H{"name": affected.Name, "identity": affected.Identity, "target_revision": facts[0].TargetRevision}
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", check); res.Code != 409 || calls != 0 {
		t.Fatalf("unconfirmed legacy request=%d with %d probes", res.Code, calls)
	}
	check["confirm_legacy_job_id"] = id
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", check); res.Code != 200 ||
		!strings.Contains(res.Body.String(), `"state":"dpkg_incomplete"`) || !errors.Is(v.admit(affected), serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("unhealthy probe lifted the fence: %d %s", res.Code, res.Body.String())
	}
	outcome = "healthy"
	if res := identityRequest(app, "POST", restoreAPTValidationRoute, admin, "", check); res.Code != http.StatusOK ||
		!strings.Contains(res.Body.String(), `"state":"approved"`) {
		t.Fatalf("healthy probe=%d: %s", res.Code, res.Body.String())
	}
	var auditedJob, auditedIdentity string
	if err := db.QueryRow(`
		SELECT json_extract(meta_json, '$.job_id'), json_extract(meta_json, '$.server_identity')
		FROM audit_events WHERE action='apt.legacy_incident.resolve' AND status='success' AND target_name=?`,
		affected.Name).Scan(&auditedJob, &auditedIdentity); err != nil || auditedJob != id || auditedIdentity != affected.Identity {
		t.Fatalf("audit record does not describe the resolved incident: %q %q %v", auditedJob, auditedIdentity, err)
	}
	if err := ensureSchema(db); err != nil {
		t.Fatal(err)
	}
	if key, err := v.authorize(t.Context(), affected); err != nil || key != "SHA256:synthetic-verified-key" {
		t.Fatalf("repeat migration lost the resolution proof: %q %v", key, err)
	}
	if err := v.admit(Server{Name: affected.Name, Identity: affected.Identity, Host: "other.example.test", User: "root"}); !errors.Is(err, serverpkg.ErrRestoreAPTValidation) {
		t.Fatalf("recorded proof authorized a different endpoint: %v", err)
	}
}
