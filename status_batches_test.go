package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

func TestStatusBatchSurvivesLostResponseAndTracksRunnerOutcome(t *testing.T) {
	t.Setenv(retryMaxAttemptsEnv, "1")
	app, cookie := newActionAPITestApp(t, filepath.Join(t.TempDir(), "status-batch.db"))
	names := []string{"synthetic-batch", "replacement"}
	for _, name := range names {
		created := identityRequest(app, "POST", "/api/servers", cookie, "", gin.H{
			"name": name, "host": name + ".invalid", "user": "root", "pass": "synthetic",
		})
		if created.Code != http.StatusCreated {
			t.Fatalf("create %s: %d %s", name, created.Code, created.Body.String())
		}
	}
	conn := &scriptedSSHConnection{responses: map[string]scriptedResponse{
		precheckDiskSpaceCmd: {stdout: "2048000\n2097152\n"},
		precheckLocksCmd:     {err: fakeExitStatusError{code: 1, msg: "no process found"}},
		precheckDpkgAuditCmd: {}, precheckAptCheckCmd: {}, aptUpdateCmd: {},
		aptListUpgradableCmd: {stdout: "Inst pkg/stable [1.1] (1.2 Debian:13 [amd64])\n"},
		aptUpgradeCmd:        {}, postcheckRebootRequiredCmd: {}, postcheckFailedUnitsCmd: {},
	}}
	original := getDialSSHConnection()
	setDialSSHConnection(func(Server, *ssh.ClientConfig) (sshConnection, error) { return conn, nil })
	t.Cleanup(func() {
		state := app.Deps.ServerState
		state.Lock()
		if status := state.StatusMap()[names[0]]; status != nil && status.Status == "pending_approval" {
			status.Status = "cancelled"
		}
		state.Unlock()
		waitForUpdateRunners()
		setDialSSHConnection(original)
	})

	server, ok := app.Deps.ServerState.FindByName(names[0])
	if !ok || server.Identity == "" {
		t.Fatal("missing stable Server identity")
	}
	target := statusBatchTarget{Name: server.Name, ServerIdentity: server.Identity}
	id := strings.Repeat("a", 32)
	command := statusBatchRequest{
		ID: id, Action: "update", SkippedCount: 1,
		Targets: []statusBatchTarget{target, {Name: names[1], ServerIdentity: "replaced-inventory-entry"}},
	}
	post := func(req statusBatchRequest) *httptest.ResponseRecorder {
		return identityRequest(app, "POST", "/api/status/batches", cookie, "", req)
	}
	// Treat the first successful response as lost and retry the identical command.
	for range 2 {
		if rec := post(command); rec.Code != http.StatusAccepted {
			t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
		}
	}
	changed := command
	changed.SkippedCount++
	if rec := post(changed); rec.Code != http.StatusConflict {
		t.Fatalf("reused key accepted different input: %d", rec.Code)
	}
	read := func(endpoint string) statusBatch {
		t.Helper()
		rec := identityRequest(app, "GET", endpoint, cookie, "", nil)
		var response struct{ Batch statusBatch }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
			t.Fatalf("read %s: %d %s", endpoint, rec.Code, rec.Body.String())
		}
		return response.Batch
	}
	waitForCondition(t, 10*time.Second, func() bool {
		batch := read("/api/status/batches/latest")
		return len(batch.Hosts) == 2 && batch.Hosts[0].State == "refused" && batch.Hosts[1].State == "waiting_approval"
	}, "durable mixed batch outcomes after reload")
	batch := read("/api/status/batches/" + id)
	if batch.ID != id || batch.SkippedCount != 1 || !batch.Hosts[0].Retryable || batch.Hosts[1].JobID == "" {
		t.Fatalf("wrong persisted batch: %+v", batch)
	}
	replaced, _ := app.Deps.ServerState.FindByName(names[1])
	retryWithNewIdentity := statusBatchRequest{
		ID: strings.Repeat("b", 32), Action: "update", RetryOf: id,
		Targets: []statusBatchTarget{{Name: names[1], ServerIdentity: replaced.Identity}},
	}
	if rec := post(retryWithNewIdentity); rec.Code != http.StatusConflict {
		t.Fatalf("retry accepted a replacement Server: %d %s", rec.Code, rec.Body.String())
	}

	approval := approvalIdentityForTest(app.Deps.ServerState, names[0])
	approvalBatchID := strings.Repeat("d", 32)
	if rec := post(statusBatchRequest{
		ID: approvalBatchID, Action: "approve_all",
		Targets: []statusBatchTarget{
			{Name: server.Name, ServerIdentity: server.Identity, Approval: approval},
			{Name: replaced.Name, ServerIdentity: replaced.Identity},
		},
	}); rec.Code != http.StatusAccepted {
		t.Fatalf("incomplete approval identity prevented admission of the valid host: %d %s", rec.Code, rec.Body.String())
	}
	waitForUpdateRunners()
	approvalBatch := read("/api/status/batches/" + approvalBatchID)
	if len(approvalBatch.Hosts) != 2 {
		t.Fatalf("expected two independent approval outcomes: %+v", approvalBatch.Hosts)
	}
	if host := approvalBatch.Hosts[0]; host.State != "refused" || host.Message == "" || host.JobID != "" || !host.Retryable {
		t.Fatalf("expected a refusal for the host without an approval identity: %+v", host)
	}
	if host := approvalBatch.Hosts[1]; host.State != "completed" || host.JobStatus != jobStatusSucceeded || host.JobID != approval.JobID {
		t.Fatalf("expected the valid approval batch host to complete: %+v", host)
	}
	batch = read("/api/status/batches/" + id)
	if host := batch.Hosts[1]; host.State != "completed" || host.JobStatus != jobStatusSucceeded {
		t.Fatalf("batch did not follow completed job: %+v", host)
	}

	var jobs, audits, completions int
	db := app.Deps.DB()
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = ? AND server_name IN (?, ?)`, jobKindUpdate, names[0], names[1]).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("expected one runner, jobs=%d err=%v", jobs, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'update.start'
		AND target_name = ? AND json_extract(meta_json, '$.maintenance_batch_id') = ?
		AND json_extract(meta_json, '$.job_id') = ?`, names[0], id, batch.Hosts[1].JobID).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("missing batch audit correlation, audits=%d err=%v", audits, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'update.complete'
		AND target_name = ? AND json_extract(meta_json, '$.execution_duration_ms') IS NOT NULL`, names[0]).Scan(&completions); err != nil || completions != 1 {
		t.Fatalf("missing runner audit outcome, completions=%d err=%v", completions, err)
	}
	if _, err := db.Exec(`UPDATE status_action_batch_hosts SET state = 'admitting', job_id = '' WHERE batch_id = ? AND name = ?`, id, names[0]); err != nil {
		t.Fatal(err)
	}
	recovered := read("/api/status/batches/" + id).Hosts[1]
	if recovered.State != "completed" || recovered.JobID != batch.Hosts[1].JobID || recovered.Retryable {
		t.Fatalf("batch lost audit recovery after interrupted recording: %+v", recovered)
	}

	created := identityRequest(app, "POST", "/api/auth/accounts", cookie, "", gin.H{
		"username": "batch-reader", "password": testPasswordStrong, "role": "reader",
	})
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body.String())
	}
	reader := identityLogin(t, app, "batch-reader")
	if rec := identityRequest(app, "GET", "/api/status/batches/"+id, reader, "", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("reader accessed another account's batch: %d", rec.Code)
	}
	if rec := identityRequest(app, "POST", "/api/status/batches", reader, "", command); rec.Code != http.StatusForbidden {
		t.Fatalf("reader submitted a maintenance batch: %d", rec.Code)
	}
	created = identityRequest(app, "POST", "/api/auth/accounts", cookie, "", gin.H{
		"username": "batch-operator", "password": testPasswordStrong, "role": "operator",
	})
	if created.Code != http.StatusCreated {
		t.Fatal(created.Code, created.Body.String())
	}
	other := identityLogin(t, app, "batch-operator")
	if rec := identityRequest(app, "GET", "/api/status/batches/"+id, other, "", nil); rec.Code != http.StatusNotFound {
		t.Fatalf("operator accessed another account's batch: %d", rec.Code)
	}
}

func TestStatusBatchShowsFailedHostFactsCollection(t *testing.T) {
	state := newServerState()
	updateDeps := testUpdateServiceDeps(t)
	updateDeps.ServerState = state
	updateDeps.HostMaintenanceSessions = HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
		return nil, errors.New("SSH connection refused")
	})
	app := newTestAppWithDeps(t, filepath.Join(t.TempDir(), "failed-facts-batch.db"), AppDeps{
		ServerState:   state,
		UpdateService: NewUpdateService(updateDeps),
	})
	cookie := app.authenticate(t)
	const name = "synthetic-facts"
	if rec := identityRequest(app, "POST", "/api/servers", cookie, "", gin.H{
		"name": name, "host": name + ".invalid", "user": "root", "pass": "synthetic",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create host: %d %s", rec.Code, rec.Body.String())
	}
	server, ok := state.FindByName(name)
	if !ok || server.Identity == "" {
		t.Fatal("missing stable Server identity")
	}
	id := strings.Repeat("c", 32)
	rec := identityRequest(app, "POST", "/api/status/batches", cookie, "", statusBatchRequest{
		ID: id, Action: "refresh_facts",
		Targets: []statusBatchTarget{{Name: name, ServerIdentity: server.Identity}},
	})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit: %d %s", rec.Code, rec.Body.String())
	}
	waitForUpdateRunners()
	readHost := func() statusBatchHost {
		t.Helper()
		rec := identityRequest(app, "GET", "/api/status/batches/"+id, cookie, "", nil)
		var response struct{ Batch statusBatch }
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || len(response.Batch.Hosts) != 1 {
			t.Fatalf("read failed collection: %d %s", rec.Code, rec.Body.String())
		}
		return response.Batch.Hosts[0]
	}
	if host := readHost(); host.State != "failed" || !strings.Contains(host.Message, "SSH connection refused") || host.Retryable {
		t.Fatalf("failed collection was treated as unconfirmed admission: %+v", host)
	}
	// Model a worker stopping after it wrote the audit outcome.
	if _, err := app.Deps.DB().Exec(`UPDATE status_action_batch_hosts SET state = 'admitting', message = '' WHERE batch_id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if host := readHost(); host.State != "failed" || host.Message != "Facts refresh failed" || host.Retryable {
		t.Fatalf("failed collection was not recovered from its audit: %+v", host)
	}
}
