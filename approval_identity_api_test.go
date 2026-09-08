package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPendingDecisionRoutesRequireTheDisplayedApprovalIdentity(t *testing.T) {
	for _, action := range []string{"approve", "approve-security", "approve-security-kept-back", "approve-full", "cancel"} {
		t.Run(action, func(t *testing.T) {
			app := newIsolatedTestApp(t)
			cookie := app.authenticate(t)
			server, err := app.Deps.ServerInventoryService.Create(Server{Name: "approval-host", Host: "192.0.2.50", User: "root"})
			if err != nil {
				t.Fatal(err)
			}
			full := strings.Repeat("retained metadata output\n", 6000)
			job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{Kind: jobKindUpdate, ServerName: server.Name, Status: jobStatusWaitingApproval, Phase: jobPhaseApprovalWait, LogsText: full})
			if err != nil {
				t.Fatal(err)
			}
			app.Deps.ServerState.RestoreStatusSnapshot(server.Name, &ServerStatus{
				Name: server.Name, Status: "pending_approval", JobID: job.ID, ApprovalGeneration: 7, Logs: full[len(full)-32768:],
				PendingUpdates: []PendingUpdate{{Package: "openssl", Security: true}, {Package: "linux-image", Security: true, KeptBack: true, RequiresFull: true}},
				UpgradePlan:    UpgradePlan{FullUpgradePlanAvailable: true, KeptBackSecurityPlanAvailable: true, FullUpgradeRemovedPackages: []string{"obsolete"}, KeptBackSecurityRemovedPackages: []string{"obsolete"}},
			})
			// The status endpoint exposes the identity clients must capture with the plan.
			get := httptest.NewRequest(http.MethodGet, "/api/servers", nil)
			get.AddCookie(cookie)
			getRec := httptest.NewRecorder()
			app.Handler.ServeHTTP(getRec, get)
			if getRec.Code != http.StatusOK || !strings.Contains(getRec.Body.String(), `"approval_generation":7`) {
				t.Fatalf("approval identity missing from status: %s", getRec.Body.String())
			}
			for _, id := range []serverActionApprovalIdentity{{}, {JobID: job.ID, Generation: 6}, {JobID: "other-job", Generation: 7}} {
				body, _ := json.Marshal(serverActionApprovalRequest{serverActionApprovalIdentity: id, ConfirmRemovals: true})
				req := httptest.NewRequest(http.MethodPost, "/api/"+action+"/"+server.Name, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.AddCookie(cookie)
				markSameOriginAuthRequest(req)
				rec := httptest.NewRecorder()
				app.Handler.ServeHTTP(rec, req)
				if rec.Code != http.StatusConflict {
					t.Fatalf("identity %+v accepted: HTTP %d %s", id, rec.Code, rec.Body.String())
				}
				saved, err := app.Deps.CurrentJobManager().GetJobWithLogs(job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if saved.Status != jobStatusWaitingApproval || saved.LogsText != full {
					t.Fatal("rejected decision modified persisted job or full output")
				}
			}
			req := pendingDecisionRequestForTest(t, "/api/"+action+"/"+server.Name, app.Deps.ServerState, server.Name, true)
			req.AddCookie(cookie)
			markSameOriginAuthRequest(req)
			rec := httptest.NewRecorder()
			app.Handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("current decision rejected: %d %s", rec.Code, rec.Body.String())
			}
			saved, err := app.Deps.CurrentJobManager().GetJobWithLogs(job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if saved.LogsText != full || saved.LogsTruncated {
				t.Fatal("successful decision replaced or truncated persisted output")
			}
			var metaJSON string
			if err := app.Deps.DB().QueryRow("SELECT meta_json FROM audit_events WHERE action = ? AND status = 'success' ORDER BY id DESC LIMIT 1", map[bool]string{true: "update.cancel", false: "update.approve"}[action == "cancel"]).Scan(&metaJSON); err != nil {
				t.Fatal(err)
			}
			var meta map[string]any
			if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
				t.Fatal(err)
			}
			if meta["job_id"] != job.ID || meta["approval_generation"] != float64(7) {
				t.Fatalf("audit lacks accepted plan identity: %s", metaJSON)
			}
		})
	}
}

func TestConcurrentPendingDecisionsCommitOnlyOne(t *testing.T) {
	server := Server{Name: "concurrent", Host: "example.org", User: "root"}
	h := newLifecycleTestHarness(t, server, &ServerStatus{Name: server.Name, Status: "pending_approval"})
	job := h.createPendingUpdateJob(t, server.Name)
	identity := approvalIdentityForTest(h.state, server.Name)
	l := h.lifecycle()
	l.audit = nil
	start := make(chan struct{})
	results := make(chan serverActionLifecycleResult, 2)
	go func() { <-start; results <- l.ApproveAll(server.Name, identity) }()
	go func() { <-start; results <- l.Cancel(server.Name, identity) }()
	close(start)
	first, second := <-results, <-results
	if !((first.statusCode == http.StatusOK && second.statusCode == http.StatusConflict) || (second.statusCode == http.StatusOK && first.statusCode == http.StatusConflict)) {
		t.Fatalf("decisions=%+v / %+v", first, second)
	}
	current := h.state.CurrentStatusSnapshot(server.Name)
	saved, err := h.jobManager.GetJob(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if (current.Status == "approved" && saved.Status != jobStatusRunning) || (current.Status == "cancelled" && saved.Status != jobStatusCancelled) {
		t.Fatalf("runtime/persistence diverged: %s / %s", current.Status, saved.Status)
	}
}
