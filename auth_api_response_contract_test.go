package main

import (
	"encoding/json"
	"mime"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

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
