package main

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	internalbackup "debian-updater/internal/backup"
	"debian-updater/internal/events"

	"github.com/alexedwards/scs/v2"
	_ "modernc.org/sqlite"
)

func TestRuntimeCompositionCompletesCoreDefaults(t *testing.T) {
	deps := AppDeps{}.withDefaults()

	if deps.DB == nil ||
		deps.DBPath == nil ||
		deps.AuditService == nil ||
		deps.AuthService == nil ||
		deps.BackupService == nil ||
		deps.BackupBarrier == nil ||
		deps.NotificationService == nil ||
		deps.ServerState == nil ||
		deps.ServerInventoryService == nil ||
		deps.PolicyService == nil ||
		deps.PolicyRepository == nil ||
		deps.UpdateService == nil ||
		deps.ObservabilityService == nil ||
		deps.MetricsTokenService == nil ||
		deps.CurrentJobManager == nil ||
		deps.NewJobManager == nil ||
		deps.SetCurrentJobManager == nil ||
		deps.GetGlobalKey == nil ||
		deps.SetGlobalKey == nil ||
		deps.ClearGlobalKey == nil ||
		deps.HasGlobalKey == nil ||
		deps.CurrentSessionManager == nil ||
		deps.NewSessionManager == nil ||
		deps.SetSessionManager == nil ||
		deps.TrustedProxies == nil ||
		deps.InitializeMaintenanceState == nil ||
		deps.Now == nil ||
		deps.JobTimestampNow == nil ||
		deps.LoadRetryPolicy == nil ||
		deps.StartJobRunner == nil ||
		deps.StartScheduledRunReconciliation == nil ||
		deps.NotifyDashboardEvent == nil ||
		deps.DashboardEventBroker == nil ||
		deps.CurrentAppTimezone == nil ||
		deps.CurrentAppLocation == nil ||
		deps.AppTimezoneDisplayName == nil ||
		deps.AppTimezoneResolvedName == nil {
		t.Fatalf("runtime composition did not populate complete core defaults: %+v", deps)
	}
}

func TestRuntimeCompositionDefaultsDashboardNotificationToBroker(t *testing.T) {
	broker := events.NewBroker()
	deps := newRuntimeComposition(AppDeps{
		DashboardEventBroker: broker,
	}).Compose()

	ch := broker.Subscribe()
	t.Cleanup(func() { broker.Unsubscribe(ch) })

	deps.NotifyDashboardEvent("runtime-composed")

	select {
	case got := <-ch:
		if got != "runtime-composed" {
			t.Fatalf("dashboard event = %q, want runtime-composed", got)
		}
	case <-time.After(time.Second):
		t.Fatalf("dashboard event was not published through composed notify callback")
	}
}

func TestRuntimeCompositionInjectsDBPathIntoRuntimeServices(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "runtime-services.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	deps := newRuntimeComposition(AppDeps{
		DB:     func() *sql.DB { return db },
		DBPath: func() string { return dbPath },
	}).Compose()

	metricsDeps := deps.MetricsTokenService.EnsureDeps()
	if got := metricsDeps.DB(); got != db {
		t.Fatalf("metrics token DB = %p, want injected %p", got, db)
	}
	if got := metricsDeps.DBPath(); got != dbPath {
		t.Fatalf("metrics token DBPath = %q, want %q", got, dbPath)
	}
	if got := deps.BackupService.Status().DBPath; got != dbPath {
		t.Fatalf("backup service DBPath = %q, want %q", got, dbPath)
	}
}

func TestRuntimeCompositionSharesStateAndPolicyRepositoryAcrossServices(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "shared-policy.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	state := newServerState()
	state.Lock()
	state.SetServers([]Server{{
		Name: "runtime-shared",
		Host: "192.0.2.80",
		Port: 22,
		User: "root",
		Tags: []string{"prod"},
	}})
	state.SetStatusMap(map[string]*ServerStatus{
		"runtime-shared": {
			Name:   "runtime-shared",
			Status: "idle",
			Tags:   []string{"prod"},
		},
	})
	state.Unlock()

	deps := newRuntimeComposition(AppDeps{
		DB:          func() *sql.DB { return db },
		DBPath:      func() string { return dbPath },
		ServerState: state,
	}).Compose()

	if got := deps.UpdateService.EnsureDeps().ServerState; got != state {
		t.Fatalf("update service server state = %p, want %p", got, state)
	}
	policyDeps := deps.PolicyService.EnsureDeps()
	if snapshot := policyDeps.SnapshotServers(); len(snapshot) != 1 || snapshot[0].Name != "runtime-shared" {
		t.Fatalf("policy snapshot = %+v, want composed server state", snapshot)
	}
	summary, err := deps.ObservabilityService.BuildDashboardSummary("24h", time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("BuildDashboardSummary() error = %v", err)
	}
	if len(summary.Servers) != 1 || summary.Servers[0].Name != "runtime-shared" {
		t.Fatalf("observability servers = %+v, want composed server state", summary.Servers)
	}

	if _, err := deps.PolicyRepository.CreatePolicy(UpdatePolicy{
		Name:                   "shared policy",
		Enabled:                true,
		TargetServers:          []string{"runtime-shared"},
		PackageScope:           updatePolicyPackageScopeSecurity,
		ExecutionMode:          updatePolicyExecutionScanOnly,
		CadenceKind:            updatePolicyCadenceDaily,
		TimeLocal:              "02:30",
		ApprovalTimeoutMinutes: defaultScheduledApprovalTimeoutMinutes,
	}); err != nil {
		t.Fatalf("create policy: %v", err)
	}
	policyList, err := policyDeps.ListPolicies()
	if err != nil {
		t.Fatalf("policy service list policies: %v", err)
	}
	observabilityList, err := deps.ObservabilityService.EnsureDeps().ListPolicies()
	if err != nil {
		t.Fatalf("observability list policies: %v", err)
	}
	if len(policyList) != 1 || policyList[0].Name != "shared policy" {
		t.Fatalf("policy service policies = %+v, want shared policy", policyList)
	}
	if len(observabilityList) != 1 || observabilityList[0].Name != "shared policy" {
		t.Fatalf("observability policies = %+v, want shared policy", observabilityList)
	}
}

func TestRuntimeCompositionJobAndSessionSettersStayAppScoped(t *testing.T) {
	jobDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "job-session.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = jobDB.Close() })
	if err := ensureJobSchema(jobDB); err != nil {
		t.Fatalf("ensure job schema: %v", err)
	}

	deps := newRuntimeComposition(AppDeps{
		DB:            func() *sql.DB { return jobDB },
		BackupBarrier: internalbackup.NewBarrier(),
	}).Compose()

	jm := newJobManager(jobDB)
	deps.SetCurrentJobManager(jm)
	if got := deps.CurrentJobManager(); got != jm {
		t.Fatalf("current job manager = %p, want app-scoped %p", got, jm)
	}

	sm := scs.New()
	deps.SetSessionManager(sm)
	if got := deps.CurrentSessionManager(); got != sm {
		t.Fatalf("current session manager = %p, want app-scoped %p", got, sm)
	}
}
