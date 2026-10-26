package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	authpkg "debian-updater/internal/auth"
	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

func TestPersonalAPIOpenAPIContractMatchesRoutesScopesTypesErrorsAndPagination(t *testing.T) {
	data, err := os.ReadFile("docs/openapi-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		OpenAPI string `json:"openapi"`
		Info    struct {
			Version string `json:"version"`
		} `json:"info"`
		Paths map[string]map[string]struct {
			Scope      string `json:"x-slu-scope"`
			Parameters []struct {
				Name   string `json:"name"`
				Schema struct {
					Type string `json:"type"`
				} `json:"schema"`
			} `json:"parameters"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"paths"`
		Components struct {
			Schemas map[string]struct {
				Type       string `json:"type"`
				Properties map[string]struct {
					Type string `json:"type"`
				} `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	if spec.OpenAPI != "3.1.0" || spec.Info.Version != "1.0.0" {
		t.Fatal("unversioned API contract")
	}
	parameter := regexp.MustCompile(`:([a-z_]+)`)
	for key, permission := range routePermissions {
		if !permission.Bearer {
			continue
		}
		method, path, _ := strings.Cut(key, " ")
		path = parameter.ReplaceAllString(path, `{$1}`)
		operation, ok := spec.Paths[path][strings.ToLower(method)]
		if !ok {
			t.Errorf("bearer route missing from OpenAPI: %s", key)
			continue
		}
		if operation.Scope != permission.Scope {
			t.Errorf("scope drift: %s", key)
		}
		for _, status := range []string{"400", "401", "403", "404", "409", "413", "429", "500", "503"} {
			if _, ok := operation.Responses[status]; !ok {
				t.Errorf("%s missing error %s", key, status)
			}
		}
	}
	for path, methods := range spec.Paths {
		for method, operation := range methods {
			route := regexp.MustCompile(`\{([a-z_]+)\}`).ReplaceAllString(path, `:$1`)
			permission, ok := routePermissions[strings.ToUpper(method)+" "+route]
			if !ok || !permission.Bearer || permission.Scope != operation.Scope {
				t.Errorf("OpenAPI documents a non-bearer route: %s %s", method, path)
			}
		}
	}
	// The established approval identity is deliberately shared by cookie and PAT.
	approvalSchema := spec.Components.Schemas["ApprovalIdentity"]
	for _, typ := range []reflect.Type{reflect.TypeOf(serverActionApprovalIdentity{}), reflect.TypeOf(struct {
		ConfirmRemovals bool `json:"confirm_removals"`
	}{})} {
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			field, ok := approvalSchema.Properties[name]
			if !ok {
				t.Errorf("approval field absent: %s", name)
				continue
			}
			want := "string"
			if f.Type.Kind() == reflect.Uint64 {
				want = "integer"
			}
			if f.Type.Kind() == reflect.Bool {
				want = "boolean"
			}
			if field.Type != want {
				t.Errorf("%s has schema type %s want %s", name, field.Type, want)
			}
		}
	}
	if len(approvalSchema.Properties) != 4 {
		t.Fatal("approval identity gained undocumented fields")
	}
	for path, names := range map[string][]string{"/api/audit-events": {"page", "page_size"}, "/api/jobs/{id}/logs": {"after_seq", "limit"}} {
		op := spec.Paths[path]["get"]
		for _, name := range names {
			found := false
			for _, p := range op.Parameters {
				if p.Name == name && p.Schema.Type == "integer" {
					found = true
				}
			}
			if !found {
				t.Errorf("pagination missing %s/%s", path, name)
			}
		}
	}
	for _, name := range []string{"ServerStatus", "UpgradePlan", "ActionResult", "Error", "LogPage", "AuditPage"} {
		if spec.Components.Schemas[name].Type != "object" {
			t.Errorf("missing typed schema %s", name)
		}
	}
}

func TestPersonalAPIRealDiscoveryPlanApprovalRunnerAndAudit(t *testing.T) {
	t.Setenv(retryMaxAttemptsEnv, "1")
	t.Setenv(postchecksEnabledEnv, "true")
	app, admin := newActionAPITestApp(t, filepath.Join(t.TempDir(), "personal-api.db"))
	created := identityRequest(app, "POST", "/api/auth/accounts", admin, "", gin.H{"username": "automation", "password": testPasswordStrong, "role": "operator"})
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	var account authpkg.Account
	if err := json.Unmarshal(created.Body.Bytes(), &account); err != nil {
		t.Fatal(err)
	}
	cookie := identityLogin(t, app, "automation")
	issued := identityRequest(app, "POST", "/api/auth/tokens", cookie, "", gin.H{"name": "maintenance workflow", "scopes": []string{"operational:read", "maintenance:write", "approvals:write", "logs:read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if issued.Code != 201 {
		t.Fatal(issued.Code, issued.Body.String())
	}
	issuedJSON := decodeContractJSON(t, issued)
	secret := issuedJSON["secret"].(string)
	tokenID := issuedJSON["token"].(map[string]any)["id"].(string)
	createdServer := identityRequest(app, "POST", "/api/servers", admin, "", gin.H{"name": "synthetic", "host": "synthetic.invalid", "port": 22, "user": "root", "pass": "test"})
	if createdServer.Code != 201 {
		t.Fatal(createdServer.Code, createdServer.Body.String())
	}
	conn := &scriptedSSHConnection{responses: map[string]scriptedResponse{
		precheckDiskSpaceCmd: {stdout: "2048000\n2097152\n"}, precheckLocksCmd: {err: fakeExitStatusError{code: 1, msg: "no process found"}}, precheckDpkgAuditCmd: {}, precheckAptCheckCmd: {}, aptUpdateCmd: {}, aptListUpgradableCmd: {stdout: "Inst pkg/stable [1.1] (1.2 Debian:13 [amd64])\n"}, aptUpgradeCmd: {}, postcheckRebootRequiredCmd: {}, postcheckFailedUnitsCmd: {},
	}}
	original := getDialSSHConnection()
	setDialSSHConnection(func(Server, *ssh.ClientConfig) (sshConnection, error) { return conn, nil })
	t.Cleanup(func() {
		globalServerState().Lock()
		if status := globalServerState().StatusMap()["synthetic"]; status != nil && status.Status == "pending_approval" {
			status.Status = "cancelled"
		}
		globalServerState().Unlock()
		waitForUpdateRunners()
		setDialSSHConnection(original)
	})
	started := identityRequest(app, "POST", "/api/update/synthetic", nil, secret, nil)
	if started.Code != 200 {
		t.Fatal(started.Code, started.Body.String())
	}
	jobID := decodeContractJSON(t, started)["job_id"].(string)
	waitForCondition(t, 5*time.Second, func() bool {
		s := globalServerState().CurrentStatusSnapshot("synthetic")
		return s != nil && s.Status == "pending_approval"
	}, "real runner publishes approval plan")
	discovered := identityRequest(app, "GET", "/api/servers", nil, secret, nil)
	if discovered.Code != 200 {
		t.Fatal(discovered.Code)
	}
	var servers []ServerStatus
	if err := json.Unmarshal(discovered.Body.Bytes(), &servers); err != nil || len(servers) != 1 {
		t.Fatal(err)
	}
	plan := servers[0]
	if plan.JobID != jobID || plan.ApprovalGeneration == 0 || len(plan.PendingUpdates) != 1 {
		t.Fatalf("incomplete plan identity: %+v", plan)
	}
	decision := gin.H{"job_id": plan.JobID, "approval_generation": plan.ApprovalGeneration, "deferral_revision": plan.UpgradePlan.PackageDeferrals.Revision}
	_, readSecret, err := app.Deps.AuthService.CreatePersonalToken(account.ID, "read only", []string{"operational:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if rec := identityRequest(app, "POST", "/api/approve/synthetic", nil, readSecret, decision); rec.Code != 403 {
		t.Fatal("read-only token approved", rec.Code)
	}
	if rec := identityRequest(app, "POST", "/api/approve/synthetic", nil, secret, decision); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	waitForUpdateRunners()
	job, err := app.Deps.CurrentJobManager().GetJob(jobID)
	if err != nil || job.Status != jobStatusSucceeded {
		t.Fatalf("runner outcome %+v %v", job, err)
	}
	wantActor := "account:" + formatAccountID(account.ID) + "/token:" + tokenID
	if job.Actor != wantActor {
		t.Fatalf("job actor %s want %s", job.Actor, wantActor)
	}
	var actor, meta string
	if err = app.Deps.DB().QueryRow("SELECT actor,meta_json FROM audit_events WHERE action='update.complete' AND target_name='synthetic' ORDER BY id DESC LIMIT 1").Scan(&actor, &meta); err != nil {
		t.Fatal(err)
	}
	if actor != wantActor || !strings.Contains(meta, "duration_ms") || strings.Contains(meta, secret) || !strings.Contains(meta, `"account_id":`+formatAccountID(account.ID)) || !strings.Contains(meta, `"personal_token_id":"`+tokenID+`"`) {
		t.Fatalf("completion audit actor/meta lost: %s %s", actor, meta)
	}
	if err = app.Deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action='update.start' AND target_name='synthetic' ORDER BY id DESC LIMIT 1").Scan(&meta); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(meta, `"account_id":`+formatAccountID(account.ID)) || !strings.Contains(meta, `"personal_token_id":"`+tokenID+`"`) {
		t.Fatalf("audit identity metadata %s", meta)
	}
	if rec := identityRequest(app, "GET", "/api/servers", nil, readSecret, nil); strings.Contains(rec.Body.String(), "Starting Linux Updater") {
		t.Fatal("read-only token leaked logs")
	}
}
