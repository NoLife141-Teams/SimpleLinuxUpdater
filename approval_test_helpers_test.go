package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	serverpkg "debian-updater/internal/servers"
)

func approvalIdentityForTest(state *serverpkg.State, name string) serverActionApprovalIdentity {
	snapshot := state.CurrentStatusSnapshot(name)
	if snapshot == nil {
		return serverActionApprovalIdentity{}
	}
	return serverActionApprovalIdentity{JobID: snapshot.JobID, Generation: snapshot.ApprovalGeneration}
}

func pendingDecisionRequestForTest(t *testing.T, path string, state *serverpkg.State, name string, confirm bool) *http.Request {
	t.Helper()
	body, err := json.Marshal(serverActionApprovalRequest{serverActionApprovalIdentity: approvalIdentityForTest(state, name), ConfirmRemovals: confirm})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	return req
}

// Older route fixtures seed pending state and jobs separately. Explicitly join
// them as the production runner does before issuing their HTTP decisions.
func bindLatestPendingApprovalFixture(t *testing.T, name string) {
	t.Helper()
	job, err := currentJobManager().FindLatestActiveJobByServerAndKind(name, jobKindUpdate)
	id := "missing-pending-job"
	if err == nil && job != nil {
		id = job.ID
	}
	mu.Lock()
	defer mu.Unlock()
	if status := statusMap[name]; status != nil {
		if status.JobID != id || status.ApprovalGeneration == 0 {
			status.JobID = id
			status.ApprovalGeneration++
		}
	}
}
