package main

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/events"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/scs/v2"
	_ "modernc.org/sqlite"
)

type failingInventoryRepository struct {
	err error
}

func (r failingInventoryRepository) Load() ([]serverpkg.Server, error)             { return nil, r.err }
func (failingInventoryRepository) Save([]serverpkg.Server, serverpkg.TxHook) error { return nil }
func (failingInventoryRepository) UpdateServerKey(string, string) error            { return nil }

func TestRuntimeCompositionReloadRestoredStateHonorsCancellation(t *testing.T) {
	composition := newRuntimeComposition(AppDeps{})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := composition.ReloadRestoredState(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ReloadRestoredState() error = %v, want context canceled", err)
	}
}

func TestRuntimeCompositionReloadRestoredStateLabelsSessionFailureWithoutPublishing(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reload-session-failure.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	composition := newRuntimeComposition(AppDeps{DB: func() *sql.DB { return db }, DBPath: func() string { return dbPath }})
	composition.resetCaches = func() {}
	deps := composition.Compose()
	initial := scs.New()
	deps.SetSessionManager(initial)
	sessionErr := errors.New("session database unavailable")
	composition.deps.NewSessionManager = func(*sql.DB) (*scs.SessionManager, error) {
		return nil, sessionErr
	}

	err = composition.ReloadRestoredState(context.Background())
	if !errors.Is(err, sessionErr) || !strings.Contains(err.Error(), "rebuild restored auth session manager") {
		t.Fatalf("ReloadRestoredState() error = %v, want labelled session failure", err)
	}
	if got := deps.CurrentSessionManager(); got != initial {
		t.Fatalf("current session manager = %p, want original %p", got, initial)
	}
}

func TestRuntimeCompositionReloadRestoredStateReturnsInventoryFailure(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reload-inventory-failure.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	state := newServerState()
	inventoryErr := errors.New("restored inventory cannot be decrypted")
	inventory := serverpkg.NewService(serverpkg.ServiceDeps{
		State:      state,
		Repository: failingInventoryRepository{err: inventoryErr},
	})
	composition := newRuntimeComposition(AppDeps{
		DB:                     func() *sql.DB { return db },
		DBPath:                 func() string { return dbPath },
		ServerState:            state,
		ServerInventoryService: inventory,
	})
	composition.resetCaches = func() {}
	composition.Compose()

	err = composition.ReloadRestoredState(context.Background())
	if !errors.Is(err, inventoryErr) || !strings.Contains(err.Error(), "reload restored Server inventory") {
		t.Fatalf("ReloadRestoredState() error = %v, want labelled inventory failure", err)
	}
}

func TestRuntimeCompositionCompletesCoreDefaults(t *testing.T) {
	deps := AppDeps{}.withDefaults()

	if deps.DB == nil ||
		deps.DBPath == nil ||
		deps.AuditService == nil ||
		deps.AuthService == nil ||
		deps.AuthSessionCommands == nil ||
		deps.BackupService == nil ||
		deps.MaintenanceCoordinator == nil ||
		deps.NotificationService == nil ||
		deps.ServerState == nil ||
		deps.ServerInventoryService == nil ||
		deps.PolicyService == nil ||
		deps.PolicyRepository == nil ||
		deps.UpdateService == nil ||
		deps.ObservabilityService == nil ||
		deps.MetricsAccessCredential == nil ||
		deps.CurrentJobManager == nil ||
		deps.NewJobManager == nil ||
		deps.SetCurrentJobManager == nil ||
		deps.GlobalSSHCredential == nil ||
		deps.CurrentSessionManager == nil ||
		deps.NewSessionManager == nil ||
		deps.SetSessionManager == nil ||
		deps.TrustedProxies == nil ||
		deps.Now == nil ||
		deps.JobTimestampNow == nil ||
		deps.LoadRetryPolicy == nil ||
		deps.StartJobRunner == nil ||
		deps.StartScheduledRunReconciliation == nil ||
		deps.NotifyDashboardEvent == nil ||
		deps.DashboardEventBroker == nil ||
		deps.ApplicationTime == nil {
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

	if _, err := deps.MetricsAccessCredential.Rotate(context.Background()); err != nil {
		t.Fatalf("rotate composed Metrics Access Credential: %v", err)
	}
	var persistedHash string
	if err := db.QueryRow("SELECT value FROM settings WHERE key = ?", metricsBearerTokenHashSetting).Scan(&persistedHash); err != nil {
		t.Fatalf("read composed Metrics Access Credential: %v", err)
	}
	if strings.TrimSpace(persistedHash) == "" {
		t.Fatal("composed Metrics Access Credential did not use injected persistence")
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
	observabilityProjection, err := deps.ObservabilityService.EnsureDeps().ProjectPolicySchedule(PolicyScheduleProjectionRequest{
		Now:     time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
		Servers: []Server{{Name: "runtime-shared"}},
	})
	if err != nil {
		t.Fatalf("observability project policy schedule: %v", err)
	}
	if len(policyList) != 1 || policyList[0].Name != "shared policy" {
		t.Fatalf("policy service policies = %+v, want shared policy", policyList)
	}
	projected := observabilityProjection.Servers["runtime-shared"].NextRun
	if projected.State != "scheduled" || projected.PolicyName != "shared policy" {
		t.Fatalf("observability schedule projection = %+v, want shared scheduled policy", projected)
	}
}

func TestRuntimeCompositionPolicyScheduleProjectionUsesAppScopedRuns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "app-scoped-runs.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := ensureSchema(db); err != nil {
		t.Fatalf("ensure schema: %v", err)
	}

	now := time.Date(2026, 7, 5, 1, 0, 0, 0, time.UTC)
	appRunUTC := "2026-07-05T02:00:00.000000000Z"
	injectedRunUTC := "2026-07-05T01:30:00.000000000Z"
	server := Server{Name: "runtime-runs", Host: "192.0.2.81", Port: 22, User: "root"}

	state := newServerState()
	state.Lock()
	state.SetServers([]Server{server})
	state.SetStatusMap(map[string]*ServerStatus{
		server.Name: {Name: server.Name, Status: "idle"},
	})
	state.Unlock()

	injectedPolicy := UpdatePolicy{
		ID:            99,
		Name:          "injected policy",
		Enabled:       true,
		TargetServers: []string{server.Name},
		PackageScope:  updatePolicyPackageScopeSecurity,
		ExecutionMode: updatePolicyExecutionScanOnly,
		CadenceKind:   updatePolicyCadenceDaily,
		TimeLocal:     "03:00",
	}
	injectedService := NewPolicyService(PolicyServiceDeps{
		ListPolicies: func() ([]UpdatePolicy, error) {
			return []UpdatePolicy{injectedPolicy}, nil
		},
		LoadOverrides: func() (map[int64]map[string]bool, error) {
			return map[int64]map[string]bool{}, nil
		},
		LoadGlobalBlackouts: func() ([]UpdatePolicyBlackoutWindow, error) {
			return []UpdatePolicyBlackoutWindow{}, nil
		},
		ListRuns: func(int) ([]UpdatePolicyRun, error) {
			return []UpdatePolicyRun{{
				PolicyID:        injectedPolicy.ID,
				PolicyName:      "injected service run",
				ServerName:      server.Name,
				ScheduledForUTC: injectedRunUTC,
				Status:          updatePolicyRunQueued,
			}}, nil
		},
		CurrentLocation: func() *time.Location { return time.UTC },
		Now:             func() time.Time { return now },
	})

	deps := newRuntimeComposition(AppDeps{
		DB:            func() *sql.DB { return db },
		DBPath:        func() string { return dbPath },
		ServerState:   state,
		PolicyService: injectedService,
	}).Compose()
	if _, _, err := deps.PolicyRepository.CreateRun(UpdatePolicyRun{
		PolicyID:        7,
		PolicyName:      "app-scoped queued run",
		ServerName:      server.Name,
		ScheduledForUTC: appRunUTC,
		ExecutionMode:   updatePolicyExecutionScanOnly,
		PackageScope:    updatePolicyPackageScopeSecurity,
		UpgradeMode:     updatePolicyUpgradeModeStandard,
		Status:          updatePolicyRunQueued,
		Summary:         "App-scoped queued run",
	}); err != nil {
		t.Fatalf("create app-scoped run: %v", err)
	}

	projection, err := deps.ObservabilityService.EnsureDeps().ProjectPolicySchedule(PolicyScheduleProjectionRequest{
		Now:     now,
		Servers: []Server{server},
	})
	if err != nil {
		t.Fatalf("project policy schedule: %v", err)
	}
	nextRun := projection.Servers[server.Name].NextRun
	if nextRun.PolicyName != "app-scoped queued run" || nextRun.ScheduledForUTC != appRunUTC {
		t.Fatalf("next run = %+v, want app-scoped repository run at %s", nextRun, appRunUTC)
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

	deps := newRuntimeComposition(AppDeps{DB: func() *sql.DB { return jobDB }}).Compose()

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
