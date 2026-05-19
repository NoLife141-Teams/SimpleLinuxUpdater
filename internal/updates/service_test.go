package updates

import (
	"io"
	"reflect"
	"sync"
	"testing"
	"time"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

type fakeSession struct{}

func (fakeSession) SetStdin(io.Reader)  {}
func (fakeSession) SetStdout(io.Writer) {}
func (fakeSession) SetStderr(io.Writer) {}
func (fakeSession) Run(string) error    { return nil }
func (fakeSession) Close() error        { return nil }

type fakeConnection struct{}

func (fakeConnection) NewSession() (SSHSessionRunner, error) { return fakeSession{}, nil }
func (fakeConnection) Close() error                          { return nil }

func testState() (*servers.State, map[string]*servers.ServerStatus) {
	mu := &sync.Mutex{}
	inventory := []servers.Server{{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"}}
	statuses := map[string]*servers.ServerStatus{
		"srv": {Name: "srv", Status: "pending_approval", PendingUpdates: []servers.PendingUpdate{{Package: "openssl", Security: true}}},
	}
	return servers.NewState(mu, &inventory, &statuses, nil), statuses
}

func TestServiceApproveCancelUsesInjectedServerState(t *testing.T) {
	state, statuses := testState()
	service := NewService(ServiceDeps{ServerState: state})

	exists, approved := service.ApprovePendingUpdate("srv", "security")
	if !exists || !approved || statuses["srv"].Status != "approved" || statuses["srv"].ApprovalScope != "security" {
		t.Fatalf("ApprovePendingUpdate() exists=%t approved=%t status=%+v", exists, approved, statuses["srv"])
	}

	statuses["srv"].Status = "pending_approval"
	statuses["srv"].Logs = "pending"
	exists, cancelled := service.CancelPendingUpdate("srv")
	if !exists || !cancelled || statuses["srv"].Status != "cancelled" || statuses["srv"].Logs != "" || len(statuses["srv"].PendingUpdates) != 0 {
		t.Fatalf("CancelPendingUpdate() exists=%t cancelled=%t status=%+v", exists, cancelled, statuses["srv"])
	}
}

func TestRunScheduledScanJobRecordsCVEResultAndAudit(t *testing.T) {
	var auditActions []string
	var runUpdate policies.RunUpdate
	deps := ServiceDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSHWithRetry: func(servers.Server, *ssh.ClientConfig, RetryPolicy, string, *int) (SSHConnection, error) {
			return fakeConnection{}, nil
		},
		RunSSHOperationWithRetry: func(_ servers.Server, _ *ssh.ClientConfig, _ *SSHConnection, _ RetryPolicy, _ string, _ string, _ *int, operation func() error) error {
			return operation()
		},
		RunSSHCommandWithTimeout: func(SSHConnection, string, io.Reader, time.Duration) (string, string, error) {
			return "", "", nil
		},
		CurrentJobManager: func() *jobs.Manager { return nil },
		AuditWithActor: func(_, _, action, _, _, _, _ string, _ map[string]any) {
			auditActions = append(auditActions, action)
		},
		RunUpdatePrechecks: func(SSHConnection) PrecheckSummary {
			return PrecheckSummary{AllPassed: true}
		},
		GetUpgradable: func(SSHConnection, time.Duration) ([]servers.PendingUpdate, []string, error) {
			return []servers.PendingUpdate{{Package: "openssl", Security: true, Raw: "Inst openssl"}}, []string{"openssl"}, nil
		},
		QueryPackageCVEs: func(SSHConnection, string) ([]string, error) {
			return []string{"CVE-2026-0001"}, nil
		},
		UpdatePolicyRun: func(_ int64, update policies.RunUpdate) error {
			runUpdate = update
			return nil
		},
	}

	NewService(deps).RunScheduledScanJob(ScheduledScanRunRequest{
		RunID:           42,
		ScheduledForUTC: "2026-05-18T12:00:00.000000000Z",
		Server:          servers.Server{Name: "srv", Host: "127.0.0.1", Port: 22, User: "root"},
		Policy:          policies.Policy{ID: 7, Name: "daily", ExecutionMode: policies.ExecutionScanOnly, PackageScope: policies.PackageScopeSecurity},
		RetryPolicy:     RetryPolicy{MaxAttempts: 1, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond},
	})

	if runUpdate.Status == nil || *runUpdate.Status != policies.RunSucceeded {
		t.Fatalf("policy run status = %v, want succeeded", runUpdate.Status)
	}
	if runUpdate.ResultJSON == nil || !reflect.DeepEqual(auditActions, []string{"schedule.run.completed"}) {
		t.Fatalf("resultJSON=%v auditActions=%v, want result and completed audit", runUpdate.ResultJSON, auditActions)
	}
}
