package main

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/postchecks"
	"debian-updater/internal/vulnerabilities"
	"github.com/gin-gonic/gin"
)

func personalAPIResponseSchema(t *testing.T, path string, rec *httptest.ResponseRecorder) string {
	t.Helper()
	data, err := os.ReadFile("docs/openapi-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]struct {
					Schema struct {
						Ref string `json:"$ref"`
					} `json:"schema"`
				} `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	response, ok := spec.Paths[path]["post"].Responses[strconv.Itoa(rec.Code)]
	if !ok {
		t.Fatalf("HTTP %d from POST %s is absent from OpenAPI", rec.Code, path)
	}
	mediaType, _, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	content, ok := response.Content[mediaType]
	if !ok {
		t.Fatalf("HTTP %d media type %q is absent from OpenAPI", rec.Code, mediaType)
	}
	return content.Schema.Ref
}

func TestPersonalAPIVulnerabilityScanAcceptedResponseContract(t *testing.T) {
	app := vulnerabilityTestApp(t, installedScannerFixture{scan: func(context.Context) (vulnerabilities.Scan, error) {
		return vulnerabilities.Scan{State: "complete", Inventoried: 1, Evaluated: 1}, nil
	}})
	_ = app.authenticate(t)
	_, secret, err := app.Deps.AuthService.CreatePersonalToken(1, "scan contract", []string{"maintenance:write"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	rec := identityRequest(app, "POST", "/api/servers/one/vulnerabilities/scan", nil, secret, nil)
	waitForUpdateRunners()
	if rec.Code != http.StatusAccepted {
		t.Fatal(rec.Code, rec.Body.String())
	}
	if schema := personalAPIResponseSchema(t, "/api/servers/{name}/vulnerabilities/scan", rec); schema != "#/components/schemas/VulnerabilityScanAccepted" {
		t.Fatalf("accepted scan has no typed response schema: %q", schema)
	}
	response := decodeContractJSON(t, rec)
	if response["status"] != "running" || response["scope"] != "installed_packages" {
		t.Fatal("accepted scan lost its asynchronous response facts", response)
	}
}

func TestPersonalAPIRemoteFailureResponseContracts(t *testing.T) {
	for _, action := range []string{"facts/refresh", "postchecks/test"} {
		t.Run(action, func(t *testing.T) {
			state := newServerState()
			opens := 0
			service := NewUpdateService(UpdateServiceDeps{ServerState: state, HostMaintenanceSessions: HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
				opens++
				return nil, errors.New("synthetic SSH transport unavailable")
			})})
			app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "gateway-contract.db"), AppDeps{ServerState: state, UpdateService: service})
			_ = app.authenticate(t)
			if _, err := app.Deps.ServerInventoryService.Create(Server{Name: "contract", Host: "synthetic.invalid", User: "root"}); err != nil {
				t.Fatal(err)
			}
			if _, err := postchecks.Save(t.Context(), app.Deps.DB(), postchecks.Config{Profiles: []postchecks.Profile{{ID: "contract", Name: "Contract", TargetServers: []string{"contract"}, Checks: []postchecks.Check{{ID: "service", Name: "Service", Kind: "systemd_active", Unit: "synthetic.service", TimeoutSeconds: 1, IntervalSeconds: 1, Mode: "blocking"}}}}}); err != nil {
				t.Fatal(err)
			}
			_, secret, err := app.Deps.AuthService.CreatePersonalToken(1, "remote failure contract", []string{"maintenance:write"}, time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			rec := identityRequest(app, "POST", "/api/servers/contract/"+action, nil, secret, nil)
			if rec.Code != http.StatusBadGateway || opens != 1 {
				t.Fatalf("expected one failed SSH attempt, got %d opens and HTTP %d: %s", opens, rec.Code, rec.Body.String())
			}
			if schema := personalAPIResponseSchema(t, "/api/servers/{name}/"+action, rec); schema != "#/components/schemas/Error" {
				t.Fatalf("gateway failure has no typed error schema: %q", schema)
			}
			if message, ok := decodeContractJSON(t, rec)["error"].(string); !ok || message == "" {
				t.Fatal("gateway failure lost its error message")
			}
		})
	}
}

func TestPersonalAPIReportMediaTypesMatchHTTPResponses(t *testing.T) {
	data, err := os.ReadFile("docs/openapi-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Paths map[string]map[string]struct {
			Responses map[string]struct {
				Content map[string]json.RawMessage `json:"content"`
			} `json:"responses"`
		} `json:"paths"`
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	issued := identityRequest(app, "POST", "/api/auth/tokens", admin, "", gin.H{"name": "report contract", "scopes": []string{"audit:read", "logs:read"}, "expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339)})
	if issued.Code != 201 {
		t.Fatal(issued.Code, issued.Body.String())
	}
	secret := decodeContractJSON(t, issued)["secret"].(string)
	job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{Kind: jobKindUpdate, ServerName: "contract", Actor: "account:1", Status: jobStatusSucceeded, Summary: "Synthetic report", LogsText: "Synthetic command output"})
	if err != nil {
		t.Fatal(err)
	}
	var auditID int64
	if err = app.Deps.DB().QueryRow("SELECT max(id) FROM audit_events").Scan(&auditID); err != nil {
		t.Fatal(err)
	}
	for _, report := range []struct{ path, id string }{{"/api/reports/jobs/{id}", job.ID}, {"/api/reports/audit/{id}", formatAccountID(auditID)}} {
		t.Run(report.path, func(t *testing.T) {
			rec := identityRequest(app, "GET", strings.Replace(report.path, "{id}", report.id, 1), nil, secret, nil)
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			mediaType, _, err := mime.ParseMediaType(rec.Header().Get("Content-Type"))
			if err != nil {
				t.Fatal(err)
			}
			if _, documented := spec.Paths[report.path]["get"].Responses["200"].Content[mediaType]; !documented {
				t.Errorf("HTTP success media type %q is absent from OpenAPI", mediaType)
			}
		})
	}
}

func TestPersonalAPIServerCollectionTypesMatchHTTPStates(t *testing.T) {
	data, err := os.ReadFile("docs/openapi-v1.json")
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct{ Type any } `json:"properties"`
			} `json:"schemas"`
		} `json:"components"`
	}
	if err = json.Unmarshal(data, &spec); err != nil {
		t.Fatal(err)
	}
	app := newIsolatedTestApp(t)
	admin := app.authenticate(t)
	created := identityRequest(app, "POST", "/api/servers", admin, "", gin.H{"name": "contract", "host": "synthetic.invalid", "user": "root", "pass": "synthetic"})
	if created.Code != 201 {
		t.Fatal(created.Code, created.Body.String())
	}
	_, secret, err := app.Deps.AuthService.CreatePersonalToken(1, "collection contract", []string{"operational:read"}, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{"idle", "pending_approval", "cancelled"} {
		t.Run(state, func(t *testing.T) {
			switch state {
			case "pending_approval":
				app.Deps.ServerState.Lock()
				status := app.Deps.ServerState.StatusMap()["contract"]
				status.Status = state
				status.Upgradable = []string{"synthetic-package"}
				status.PendingUpdates = []PendingUpdate{{Package: "synthetic-package"}}
				app.Deps.ServerState.Unlock()
			case "cancelled":
				if exists, cancelled := app.Deps.ServerState.CancelPendingUpdate("contract"); !exists || !cancelled {
					t.Fatal("pending update was not cancelled")
				}
			}
			rec := identityRequest(app, "GET", "/api/servers", nil, secret, nil)
			if rec.Code != 200 {
				t.Fatal(rec.Code, rec.Body.String())
			}
			var statuses []map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &statuses); err != nil || len(statuses) != 1 {
				t.Fatal("invalid status response", err, rec.Body.String())
			}
			for _, name := range []string{"upgradable", "pending_updates"} {
				value := statuses[0][name]
				wireType := "array"
				if string(value) == "null" {
					wireType = "null"
				} else if len(value) == 0 || value[0] != '[' {
					t.Fatalf("unexpected %s value: %s", name, value)
				}
				documented := spec.Components.Schemas["ServerStatus"].Properties[name].Type
				matches := documented == wireType
				if types, ok := documented.([]any); ok {
					for _, allowed := range types {
						matches = matches || allowed == wireType
					}
				}
				if !matches {
					t.Errorf("HTTP %s is %s, excluded by OpenAPI type %v", name, wireType, documented)
				}
			}
		})
	}
}
