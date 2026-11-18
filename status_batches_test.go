package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/ssh"
)

func TestStatusBatchSurvivesProcessExitAfterAdmission(t *testing.T) {
	const (
		dbEnv     = "SLU_TEST_BATCH_ADMISSION_DB"
		actionEnv = "SLU_TEST_BATCH_ADMISSION_ACTION"
		exitCode  = 71
		hostName  = "crash-batch-host"
		batchID   = "b56962f607fa4171adaada1fedea0592"
	)
	if path := os.Getenv(dbEnv); path != "" {
		action := os.Getenv(actionEnv)
		state := newServerState()
		var app *testApp
		updateDeps := testUpdateServiceDeps(t)
		updateDeps.ServerState = state
		updateDeps.CurrentJobManager = func() *JobManager {
			return app.Deps.CurrentJobManager()
		}
		updateDeps.HostMaintenanceSessions = HostMaintenanceSessionFactoryFunc(func(context.Context, HostMaintenanceSessionRequest) (HostMaintenanceSession, error) {
			return nil, errors.New("fixture host unavailable")
		})
		app = newTestAppWithDeps(t, path, AppDeps{
			ServerState: state, UpdateService: NewUpdateService(updateDeps),
			NotifyDashboardEvent: func(reason string) {
				if (action == "update" && reason == "job.create") || (action == "refresh_facts" && reason == serverFactsRefreshAction) {
					os.Exit(exitCode)
				}
			},
			NotifyDashboardJob: func(_ string, job JobRecord) {
				if action == "cancel" && job.Status == jobStatusCancelled {
					os.Exit(exitCode)
				}
			},
		})
		cookie := app.authenticate(t)
		if rec := identityRequest(app, "POST", "/api/servers", cookie, "", gin.H{
			"name": hostName, "host": hostName + ".invalid", "user": "root", "pass": "fixture",
		}); rec.Code != http.StatusCreated {
			t.Fatalf("create host: %d %s", rec.Code, rec.Body.String())
		}
		server, ok := state.FindByName(hostName)
		if !ok {
			t.Fatal("missing fixture server")
		}
		target := statusBatchTarget{Name: hostName, ServerIdentity: server.Identity}
		if action == "cancel" {
			job, err := app.Deps.CurrentJobManager().CreateJob(JobCreateParams{
				Kind: jobKindUpdate, ServerName: hostName, Status: jobStatusWaitingApproval,
			})
			if err != nil {
				t.Fatal(err)
			}
			status := state.CurrentStatusSnapshot(hostName)
			status.Status, status.JobID, status.ApprovalGeneration = "pending_approval", job.ID, 1
			state.RestoreStatusSnapshot(hostName, status)
			target.Approval = approvalIdentityForTest(state, hostName)
		}
		if rec := identityRequest(app, "POST", "/api/status/batches", cookie, "", statusBatchRequest{
			ID: batchID, Action: action, Targets: []statusBatchTarget{target},
		}); rec.Code != http.StatusAccepted {
			t.Fatalf("submit batch: %d %s", rec.Code, rec.Body.String())
		}
		waitForUpdateRunners()
		t.Fatal("fixture reached the end of the worker without crossing the crash checkpoint")
	}

	for _, tc := range []struct {
		action, state, jobStatus string
	}{
		{"update", "completed", jobStatusInterrupted},
		{"cancel", "completed", jobStatusCancelled},
		{"refresh_facts", "failed", ""},
	} {
		t.Run(tc.action, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crash-batch.db")
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			child := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStatusBatchSurvivesProcessExitAfterAdmission$")
			child.Env = append(os.Environ(), dbEnv+"="+path, actionEnv+"="+tc.action)
			output, err := child.CombinedOutput()
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) || exitErr.ExitCode() != exitCode {
				t.Fatalf("fixture did not stop at the admission checkpoint: %v\n%s", err, output)
			}
			app := newTestAppWithDeps(t, path, AppDeps{ServerState: newServerState()})
			db := app.Deps.DB()
			if _, err := db.Exec("DELETE FROM audit_events"); err != nil {
				t.Fatal(err)
			}
			cookie := identityLogin(t, app, "admin")
			rec := identityRequest(app, "GET", "/api/status/batches/"+batchID, cookie, "", nil)
			var response struct{ Batch statusBatch }
			if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &response) != nil || len(response.Batch.Hosts) != 1 {
				t.Fatalf("read recovered batch: %d %s", rec.Code, rec.Body.String())
			}
			host := response.Batch.Hosts[0]
			if host.State != tc.state || host.JobStatus != tc.jobStatus || (host.JobID != "") != (tc.jobStatus != "") || host.Retryable {
				t.Fatalf("wrong result after process exit and audit purge: %+v", host)
			}
			var jobs int
			if err := db.QueryRow("SELECT COUNT(*) FROM jobs WHERE server_name = ?", hostName).Scan(&jobs); err != nil || jobs > 1 {
				t.Fatalf("job count after restart: %d, %v", jobs, err)
			}
		})
	}
}

func TestStatusBatchRetryAdmitsOnlyOneRunner(t *testing.T) {
	t.Setenv(retryMaxAttemptsEnv, "1")
	app, cookie := newActionAPITestApp(t, filepath.Join(t.TempDir(), "retry-batch.db"))
	const name = "retry-host"
	if rec := identityRequest(app, "POST", "/api/servers", cookie, "", gin.H{
		"name": name, "host": name + ".invalid", "user": "root", "pass": "synthetic",
	}); rec.Code != http.StatusCreated {
		t.Fatalf("create host: %d %s", rec.Code, rec.Body.String())
	}
	server, _ := app.Deps.ServerState.FindByName(name)
	target := statusBatchTarget{Name: name, ServerIdentity: server.Identity}
	db := app.Deps.DB()
	sourceID := strings.Repeat("a", 32)
	seedStatusBatchRetryHistory(t, db, sourceID, "", "update", "queued", target)

	conn := &scriptedSSHConnection{responses: map[string]scriptedResponse{
		precheckDiskSpaceCmd: {stdout: "2048000\n2097152\n"},
		precheckLocksCmd:     {err: fakeExitStatusError{code: 1, msg: "no process found"}},
		precheckDpkgAuditCmd: {}, precheckAptCheckCmd: {}, aptUpdateCmd: {}, aptListUpgradableCmd: {},
	}}
	oldDial := getDialSSHConnection()
	setDialSSHConnection(func(Server, *ssh.ClientConfig) (sshConnection, error) { return conn, nil })
	t.Cleanup(func() {
		waitForUpdateRunners()
		setDialSSHConnection(oldDial)
	})

	requests := []statusBatchRequest{
		{ID: strings.Repeat("b", 32), Action: "update", RetryOf: sourceID, Targets: []statusBatchTarget{target}},
		{ID: strings.Repeat("c", 32), Action: "update", RetryOf: sourceID, Targets: []statusBatchTarget{target}},
	}
	post := func(req statusBatchRequest) *httptest.ResponseRecorder {
		return identityRequest(app, "POST", "/api/status/batches", cookie, "", req)
	}
	start := make(chan struct{})
	type submitResult struct {
		request  statusBatchRequest
		response *httptest.ResponseRecorder
	}
	results := make(chan submitResult, len(requests))
	for _, request := range requests {
		go func() {
			<-start
			results <- submitResult{request, post(request)}
		}()
	}
	close(start)
	winner, other := <-results, <-results
	if winner.response.Code != http.StatusAccepted {
		winner, other = other, winner
	}
	winnerID := winner.request.ID
	assertConflict := func(rec *httptest.ResponseRecorder) {
		t.Helper()
		var reply struct {
			RetryBatchID string `json:"retry_batch_id"`
		}
		if rec.Code != http.StatusConflict || json.Unmarshal(rec.Body.Bytes(), &reply) != nil || reply.RetryBatchID != winnerID {
			t.Fatalf("expected conflict identifying %s: %d %s", winnerID, rec.Code, rec.Body.String())
		}
	}
	if winner.response.Code != http.StatusAccepted {
		t.Fatalf("neither request admitted: %d %s", winner.response.Code, winner.response.Body.String())
	}
	assertConflict(other.response)
	waitForUpdateRunners()
	assertConflict(post(other.request))
	if replay := post(winner.request); replay.Code != http.StatusAccepted || !strings.Contains(replay.Body.String(), winnerID) {
		t.Fatalf("lost response did not recover exact committed retry: %d %s", replay.Code, replay.Body.String())
	}

	// An empty worker registry models the same durable records after restart.
	reopened := &statusBatchService{}
	source, err := reopened.readBatch(t.Context(), db, sourceID, 1)
	if err != nil || len(source.Hosts) != 1 || source.Hosts[0].Retryable || source.Hosts[0].RetryBatchID != winnerID {
		t.Fatalf("reopened source did not link to admitted attempt: %+v %v", source, err)
	}
	retry, err := reopened.readBatch(t.Context(), db, winnerID, 1)
	if err != nil || len(retry.Hosts) != 1 || retry.Hosts[0].JobStatus != jobStatusSucceeded || retry.Hosts[0].JobID == "" {
		t.Fatalf("retry did not retain its successful job: %+v %v", retry, err)
	}
	var jobs, audited int
	if err := db.QueryRow(`SELECT COUNT(*) FROM jobs WHERE kind = ? AND server_name = ?`, jobKindUpdate, name).Scan(&jobs); err != nil || jobs != 1 {
		t.Fatalf("expected one update job, got %d: %v", jobs, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM audit_events WHERE action = 'update.start' AND target_name = ?
		AND json_extract(meta_json, '$.maintenance_batch_id') = ?
		AND json_extract(meta_json, '$.maintenance_batch_retry_of') = ?
		AND json_extract(meta_json, '$.job_id') = ?`, name, winnerID, sourceID, retry.Hosts[0].JobID).Scan(&audited); err != nil || audited != 1 {
		t.Fatalf("expected an admission audit linking both lots and the job, got %d: %v", audited, err)
	}
}

func TestStatusBatchRetryChecksPersistedHistoryAndApproval(t *testing.T) {
	db := conditionSourceDB(t)
	approve := statusBatchTarget{
		Name: "approval-host", ServerIdentity: "inventory-original",
		Approval: serverActionApprovalIdentity{JobID: "job", Generation: 2, DeferralRevision: "revision", RemediationDigest: "digest"},
	}
	seedStatusBatchRetryHistory(t, db, "source", "", "approve_all", "refused", approve)
	seedStatusBatchRetryHistory(t, db, "first", "source", "approve_all", "refused", approve)
	seedStatusBatchRetryHistory(t, db, "uncertain", "", "approve_all", "admitting", approve)
	seedStatusBatchRetryHistory(t, db, "fork", "", "approve_all", "refused", approve)
	seedStatusBatchRetryHistory(t, db, "fork-a", "fork", "approve_all", "accepted", approve)
	seedStatusBatchRetryHistory(t, db, "fork-b", "fork", "approve_all", "refused", approve)
	changed := approve
	changed.Approval.Generation++
	recreated := approve
	recreated.ServerIdentity = "inventory-replacement"
	for _, tc := range []struct {
		name, source string
		account      int64
		target       statusBatchTarget
		wantError    error
	}{
		{"original redirects to its existing attempt", "source", 1, approve, &statusBatchRetryConflict{batchID: "first"}},
		{"another account cannot follow the attempt", "source", 2, approve, &statusBatchRetryConflict{}},
		{"uncertain outcome requires verification", "uncertain", 1, approve, &statusBatchRetryConflict{}},
		{"old competing attempts require verification", "fork-b", 1, approve, &statusBatchRetryConflict{}},
		{"a replaced plan requires new review", "first", 1, changed, errBatchRetryPlanChanged},
		{"a recreated inventory entry is rejected", "first", 1, recreated, &statusBatchRetryConflict{}},
		{"refused attempt may be retried", "first", 1, approve, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := statusBatchRequest{ID: "next-" + tc.name, Action: "approve_all", RetryOf: tc.source, Targets: []statusBatchTarget{tc.target}}
			created, err := storeStatusBatch(t.Context(), db, request, "fingerprint", tc.account, "actor", "2026-11-18T12:00:01Z", false)
			if expected, ok := tc.wantError.(*statusBatchRetryConflict); ok {
				var actual *statusBatchRetryConflict
				if created || !errors.As(err, &actual) || *actual != *expected {
					t.Fatalf("created=%t, conflict=%+v, err=%v", created, actual, err)
				}
			} else if !errors.Is(err, tc.wantError) || created != (tc.wantError == nil) {
				t.Fatalf("created=%t, err=%v, expected %v", created, err, tc.wantError)
			}
		})
	}
}

func seedStatusBatchRetryHistory(t *testing.T, db *sql.DB, id, retryOf, action, state string, target statusBatchTarget) {
	t.Helper()
	if _, err := db.Exec(`INSERT INTO status_action_batches (id, account_id, actor, action, created_at, fingerprint, skipped_count, retry_of)
		VALUES (?, 1, 'account:1', ?, '2026-11-18T12:00:00Z', 'fixture', 0, ?)`, id, action, retryOf); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO status_action_batch_hosts
		(batch_id, name, server_identity, approval_job_id, approval_generation, deferral_revision, remediation_digest, state)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, id, target.Name, target.ServerIdentity, target.Approval.JobID,
		target.Approval.Generation, target.Approval.DeferralRevision, target.Approval.RemediationDigest, state); err != nil {
		t.Fatal(err)
	}
}

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
	if host := approvalBatch.Hosts[0]; host.State != "refused" || host.Message == "" || host.JobID != "" || host.Retryable {
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
	if _, err := db.Exec(`UPDATE status_action_batch_hosts SET state = 'admitting', job_id = '' WHERE batch_id = ? AND name = ?`, approvalBatchID, server.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE audit_events SET meta_json = json_set(meta_json, '$.approval_generation', ?)
		WHERE action = 'update.approve' AND status = 'success' AND json_extract(meta_json, '$.maintenance_batch_id') = ?`,
		approval.Generation+1, approvalBatchID); err != nil {
		t.Fatal(err)
	}
	if host := read("/api/status/batches/" + approvalBatchID).Hosts[1]; host.State != "uncertain" || host.JobID != "" || host.Retryable {
		t.Fatalf("a different approval generation was accepted as proof: %+v", host)
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
	if _, err := app.Deps.DB().Exec(`DELETE FROM audit_events WHERE json_extract(meta_json, '$.maintenance_batch_id') = ?`, id); err != nil {
		t.Fatal(err)
	}
	if host := readHost(); host.State != "failed" || host.Message != "Facts refresh failed" || host.Retryable {
		t.Fatalf("reconciled collection outcome was lost after audit retention: %+v", host)
	}
	historyRec := identityRequest(app, "GET", "/api/status/batches", cookie, "", nil)
	var history statusBatchHistoryPage
	if historyRec.Code != http.StatusOK || json.Unmarshal(historyRec.Body.Bytes(), &history) != nil || len(history.Batches) != 1 {
		t.Fatalf("read batch history: %d %s", historyRec.Code, historyRec.Body.String())
	}
	if batch := history.Batches[0]; batch.Terminal != 1 || batch.NeedsVerification != 0 {
		t.Fatalf("history did not count the audit-recovered failure as finished: %+v", batch)
	}
}
