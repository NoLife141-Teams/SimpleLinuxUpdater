package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

func TestDemoSeedResetEnabled(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want bool
	}{
		{name: "empty disabled", raw: "", want: false},
		{name: "arbitrary disabled", raw: "variant-b", want: false},
		{name: "one enabled", raw: "1", want: true},
		{name: "true enabled", raw: "true", want: true},
		{name: "yes enabled", raw: "yes", want: true},
		{name: "reset enabled", raw: "reset", want: true},
		{name: "variant c enabled", raw: "variant-c", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := demoSeedResetEnabled(tt.raw); got != tt.want {
				t.Fatalf("demoSeedResetEnabled(%q) = %v, want %v", tt.raw, got, tt.want)
			}
		})
	}
}

func TestSeedVariantCDemoRuntimeRestoresStateOnSaveFailure(t *testing.T) {
	state := newServerState()
	originalServers := []Server{{Name: "existing", Host: "existing.example.test", Port: 22, User: "root", Tags: []string{"prod"}}}
	originalStatuses := map[string]*ServerStatus{
		"existing": {Name: "existing", Host: "existing.example.test", Port: 22, User: "root", Status: "idle", Tags: []string{"prod"}},
	}
	state.Lock()
	state.SetServers(cloneServers(originalServers))
	state.SetStatusMap(cloneStatusMap(originalStatuses))
	state.Unlock()

	saveErr := errors.New("save failed")
	service := serverpkg.NewService(serverpkg.ServiceDeps{
		State:      state,
		Repository: failingDemoSeedRepository{err: saveErr},
	})

	err := seedVariantCDemoRuntime(AppDeps{
		ServerState:            state,
		ServerInventoryService: service,
		DB:                     func() *sql.DB { return nil },
	})
	if !errors.Is(err, saveErr) {
		t.Fatalf("seedVariantCDemoRuntime() error = %v, want %v", err, saveErr)
	}
	if got := state.CloneServers(); !reflect.DeepEqual(got, originalServers) {
		t.Fatalf("servers after failed seed = %+v, want %+v", got, originalServers)
	}
	if got := state.CloneStatusMap(); !reflect.DeepEqual(got, originalStatuses) {
		t.Fatalf("status map after failed seed = %+v, want %+v", got, originalStatuses)
	}
}

type failingDemoSeedRepository struct {
	err error
}

func (r failingDemoSeedRepository) Load() ([]serverpkg.Server, error) {
	return nil, nil
}

func (r failingDemoSeedRepository) Save([]serverpkg.Server, serverpkg.TxHook) error {
	return r.err
}

func (r failingDemoSeedRepository) UpdateServerKey(string, string) error {
	return nil
}

func TestDemoSeedPendingApprovalsAcceptDisplayedIdentity(t *testing.T) {
	for _, action := range []string{"approve", "approve-security", "cancel"} {
		t.Run(action, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			cookie := app.authenticate(t)
			t.Setenv("DEBIAN_UPDATER_DEMO_SEED", "variant-c")
			t.Setenv("DEBIAN_UPDATER_DEMO_RESET", "1")
			seedVariantCDemoIfRequested(app.Deps)
			for _, name := range []string{"edge-cache-03", "prod-web-01"} {
				req := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
				req.AddCookie(cookie)
				rec := httptest.NewRecorder()
				app.Handler.ServeHTTP(rec, req)
				var statuses []ServerStatus
				if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &statuses) != nil {
					t.Fatalf("load displayed demo status: %d %s", rec.Code, rec.Body.String())
				}
				var displayed *ServerStatus
				for i := range statuses {
					if statuses[i].Name == name {
						displayed = &statuses[i]
					}
				}
				if displayed == nil || displayed.Status != "pending_approval" {
					t.Fatalf("missing pending demo host %s: %+v", name, displayed)
				}
				body, err := json.Marshal(serverActionApprovalIdentity{JobID: displayed.JobID, Generation: displayed.ApprovalGeneration})
				if err != nil {
					t.Fatal(err)
				}
				req = httptest.NewRequest(http.MethodPost, "/api/"+action+"/"+name, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				markSameOriginAuthRequest(req)
				rec = httptest.NewRecorder()
				app.Handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Errorf("seeded %s %s rejected: %d %s", name, action, rec.Code, rec.Body.String())
				}
				job, err := app.Deps.CurrentJobManager().GetJob(displayed.JobID)
				wantStatus := jobStatusRunning
				if action == "cancel" {
					wantStatus = jobStatusCancelled
				}
				if err != nil || job.Status != wantStatus || job.ServerName != name {
					t.Errorf("seeded decision did not transition matching job: %+v %v", job, err)
				}
			}
		})
	}
}
