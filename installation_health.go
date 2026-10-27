package main

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	installationhealthpkg "debian-updater/internal/installationhealth"
	"github.com/gin-gonic/gin"
)

const installationProbeBudget = 250 * time.Millisecond

type installationHealth struct {
	mu           sync.RWMutex
	phase        string
	auditPruner  installationhealthpkg.Worker
	jobLogPruner installationhealthpkg.Worker
}

func newInstallationHealth() *installationHealth { return &installationHealth{phase: "initializing"} }
func (h *installationHealth) setPhase(phase string) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.phase != "stopping" {
		h.phase = phase
	}
}
func (h *installationHealth) Phase() string {
	if h == nil {
		return "initializing"
	}
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.phase
}

// probe is read-only: no migrations, recovery, session lookup or service work.
func (h *installationHealth) probe(ctx context.Context, deps AppDeps) string {
	phase := h.Phase()
	if phase != "stopping" && deps.MaintenanceCoordinator.Snapshot().RecoveryRequired {
		return "recovery_required"
	}
	if phase != "ready" {
		return phase
	}
	lease, state, ok := deps.MaintenanceCoordinator.TryProbe()
	if !ok {
		if state.RecoveryRequired {
			return "recovery_required"
		}
		return "maintenance"
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(ctx, installationProbeBudget)
	defer cancel()
	if deps.DB == nil {
		return "unavailable"
	}
	db := deps.DB()
	if db == nil {
		return "unavailable"
	}
	// SELECT checks actual read access; Ping alone can succeed on a closed file or
	// an idle SQLite connection. No arbitrary SQL/error text reaches the caller.
	if !installationDatabaseReadable(ctx, db) {
		return "unavailable"
	}
	if h.Phase() != "ready" {
		return h.Phase()
	}
	return "ready"
}

// SQLite's busy handler can outlive a cancelled query context. Borrow one
// connection and disable its busy wait for this read, then restore its setting
// before returning it to the pool. These PRAGMAs only change connection-local
// configuration; they do not write persistence or affect concurrent borrowers.
func installationDatabaseReadable(ctx context.Context, db *sql.DB) bool {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	var busyTimeout int
	if err := conn.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busyTimeout); err != nil {
		return false
	}
	defer func() {
		restoreCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), installationProbeBudget)
		defer cancel()
		if _, err := conn.ExecContext(restoreCtx, fmt.Sprintf("PRAGMA busy_timeout=%d", busyTimeout)); err != nil {
			// Never return a connection with changed lock-wait behavior to work.
			_ = conn.Raw(func(any) error { return driver.ErrBadConn })
		}
	}()
	if _, err := conn.ExecContext(ctx, "PRAGMA busy_timeout=0"); err != nil {
		return false
	}
	var one int
	return conn.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_schema)").Scan(&one) == nil
}

func installationProbeHandler(deps AppDeps, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" && r.URL.Path != "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		state := "alive"
		if r.URL.Path == "/readyz" {
			state = deps.InstallationHealth.probe(r.Context(), deps)
		}
		if state != "alive" && state != "ready" {
			w.WriteHeader(http.StatusServiceUnavailable)
		} else {
			w.WriteHeader(http.StatusOK)
		}
		if r.Method != http.MethodHead {
			_, _ = fmt.Fprintf(w, "{\"status\":%q}\n", state)
		}
	})
}

type installationStorage struct {
	DBBytes        *int64  `json:"db_bytes"`
	WALBytes       *int64  `json:"wal_bytes"`
	AvailableBytes *uint64 `json:"available_bytes"`
}
type installationNotificationFacts struct {
	Available bool `json:"available"`
	Enabled   bool `json:"enabled"`
	Pending   int  `json:"pending"`
	Retrying  int  `json:"retrying"`
	Failed    int  `json:"failed"`
}
type installationDiagnostics struct {
	CheckedAt                time.Time                                       `json:"checked_at"`
	State                    string                                          `json:"state"`
	Maintenance              bool                                            `json:"maintenance"`
	Workers                  map[string]installationhealthpkg.WorkerSnapshot `json:"workers"`
	BackupAvailable          bool                                            `json:"backup_available"`
	BackupState              string                                          `json:"backup_state"`
	LastVerifiedBackupAt     *time.Time                                      `json:"last_verified_backup_at"`
	VerifiedBackupAgeSeconds *float64                                        `json:"verified_backup_age_seconds"`
	Notifications            installationNotificationFacts                   `json:"notifications"`
	Storage                  installationStorage                             `json:"storage"`
}

func installationSnapshot(ctx context.Context, deps AppDeps) installationDiagnostics {
	now := deps.Now().UTC()
	out := installationDiagnostics{CheckedAt: now, State: deps.InstallationHealth.probe(ctx, deps), Maintenance: deps.MaintenanceCoordinator.Snapshot().Active, Workers: map[string]installationhealthpkg.WorkerSnapshot{}, BackupState: "unavailable"}
	out.Workers["audit_pruner"] = deps.InstallationHealth.auditPruner.Snapshot(true)
	out.Workers["job_log_pruner"] = deps.InstallationHealth.jobLogPruner.Snapshot(true)
	out.Workers["scheduler"] = deps.PolicyService.WorkerHealth()
	out.Workers["host_facts"] = deps.HostFactsRefreshWorker.WorkerHealth(parseBoolEnvWithDefault(automaticHostFactsRefreshEnabledEnv, true))
	out.Workers["vulnerabilities"] = deps.VulnerabilityScans.WorkerHealth()
	// A separate read-only lease prevents querying a persistence generation while
	// restore replaces it. No destination validation or secret-file reads occur.
	lease, state, ok := deps.MaintenanceCoordinator.TryProbe()
	if !ok || out.State != "ready" {
		if !ok && out.State == "ready" {
			out.State = "maintenance"
			if state.RecoveryRequired {
				out.State = "recovery_required"
			}
			out.Maintenance = state.Active
		}
		if lease != nil {
			lease.Close()
		}
		return out
	}
	defer lease.Close()
	ctx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	if deps.BackupAutomation != nil {
		facts, err := deps.BackupAutomation.installationFacts(ctx)
		if err == nil {
			out.Workers["backup"] = deps.BackupAutomation.health.Snapshot(facts.Config.Enabled)
		}
	}
	if service, ok := deps.NotificationService.(interface {
		InstallationHealth(context.Context) (installationhealthpkg.WorkerSnapshot, bool, int, int, int, error)
	}); ok {
		worker, enabled, pending, retrying, failed, err := service.InstallationHealth(ctx)
		if err == nil {
			out.Workers["notifications"] = worker
		}
		out.Notifications = installationNotificationFacts{err == nil, enabled, pending, retrying, failed}
	}
	if deps.BackupService != nil {
		health, err := deps.BackupService.RecoveryHealth(ctx)
		out.BackupAvailable = err == nil
		out.BackupState = health.State
		if at, err := time.Parse(time.RFC3339Nano, health.Verification.LastSuccessAt); err == nil {
			out.LastVerifiedBackupAt = &at
			age := max(0, now.Sub(at).Seconds())
			out.VerifiedBackupAgeSeconds = &age
		}
	}
	if deps.DBPath != nil {
		path := deps.DBPath()
		out.Storage = installationStorage{DBBytes: installationFileSize(path), WALBytes: installationFileSize(path + "-wal"), AvailableBytes: installationAvailableBytes(filepath.Dir(path))}
	}
	return out
}
func installationFileSize(path string) *int64 {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil
	}
	n := info.Size()
	return &n
}
func (a *backupAutomation) installationFacts(ctx context.Context) (backupAutomationState, error) {
	return a.load(ctx)
}
func handleInstallationDiagnostics(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	c.JSON(http.StatusOK, installationSnapshot(c.Request.Context(), deps))
}

// Names and the worker label vocabulary are fixed by the runtime owners. Missing
// observations are omitted, never synthesized as timestamp zero or a healthy age.
func installationMetrics(value installationDiagnostics) string {
	var b strings.Builder
	gauge := func(name, help string, v any) {
		fmt.Fprintf(&b, "# HELP slu_installation_%s %s\n# TYPE slu_installation_%s gauge\nslu_installation_%s %v\n", name, help, name, name, v)
	}
	bit := func(v bool) int {
		if v {
			return 1
		}
		return 0
	}
	gauge("ready", "Local persistence and runtime are ready.", bit(value.State == "ready"))
	gauge("maintenance", "Exclusive Maintenance Coordination is active.", bit(value.Maintenance))
	gauge("checked_timestamp_seconds", "Snapshot observation time.", float64(value.CheckedAt.UnixNano())/1e9)
	for _, metric := range []struct {
		name, help string
		get        func(installationhealthpkg.WorkerSnapshot) float64
	}{
		{"worker_enabled", "Worker enabled by configuration.", func(w installationhealthpkg.WorkerSnapshot) float64 { return float64(bit(w.Enabled)) }},
		{"worker_running", "Worker invocation is in progress.", func(w installationhealthpkg.WorkerSnapshot) float64 { return float64(bit(w.Running)) }},
		{"worker_errors", "Observed errors since process startup.", func(w installationhealthpkg.WorkerSnapshot) float64 { return float64(w.Errors) }},
	} {
		fmt.Fprintf(&b, "# HELP slu_installation_%s %s\n# TYPE slu_installation_%s gauge\n", metric.name, metric.help, metric.name)
		for _, name := range installationWorkerNames {
			if w, ok := value.Workers[name]; ok {
				fmt.Fprintf(&b, "slu_installation_%s{worker=%q} %g\n", metric.name, name, metric.get(w))
			}
		}
	}
	for _, kind := range []string{"tick", "progress", "error"} {
		fmt.Fprintf(&b, "# HELP slu_installation_worker_last_%s_timestamp_seconds Last observed worker %s time.\n# TYPE slu_installation_worker_last_%s_timestamp_seconds gauge\n", kind, kind, kind)
		for _, name := range installationWorkerNames {
			w, ok := value.Workers[name]
			if !ok {
				continue
			}
			at := w.LastTick
			if kind == "progress" {
				at = w.LastProgress
			}
			if kind == "error" {
				at = w.LastError
			}
			if !at.IsZero() {
				fmt.Fprintf(&b, "slu_installation_worker_last_%s_timestamp_seconds{worker=%q} %g\n", kind, name, float64(at.UnixNano())/1e9)
			}
		}
	}
	gauge("backup_evidence_available", "Backup evidence could be read.", bit(value.BackupAvailable))
	if value.LastVerifiedBackupAt != nil {
		gauge("backup_verified_timestamp_seconds", "Last accepted backup verification time.", float64(value.LastVerifiedBackupAt.UnixNano())/1e9)
	}
	if value.VerifiedBackupAgeSeconds != nil {
		gauge("backup_verified_age_seconds", "Age of last accepted backup verification.", *value.VerifiedBackupAgeSeconds)
	}
	gauge("notifications_available", "Outbox facts could be read.", bit(value.Notifications.Available))
	if value.Notifications.Available {
		gauge("notifications_enabled", "At least one notification destination enabled.", bit(value.Notifications.Enabled))
		gauge("notifications_pending", "Persisted pending and claimed outbox rows.", value.Notifications.Pending)
		gauge("notifications_retrying", "Persisted retrying outbox rows.", value.Notifications.Retrying)
		gauge("notifications_failed", "Failed outbox rows within retention.", value.Notifications.Failed)
	}
	if value.Storage.DBBytes != nil {
		gauge("db_bytes", "Observed SQLite main file size.", *value.Storage.DBBytes)
	}
	if value.Storage.WALBytes != nil {
		gauge("wal_bytes", "Observed SQLite WAL file size.", *value.Storage.WALBytes)
	}
	if value.Storage.AvailableBytes != nil {
		gauge("storage_available_bytes", "Available filesystem bytes for SQLite.", *value.Storage.AvailableBytes)
	}
	return b.String()
}

var installationWorkerNames = []string{"scheduler", "host_facts", "vulnerabilities", "backup", "notifications", "audit_pruner", "job_log_pruner"}

// The Gin boundary also covers embedded routers and disposable test compositions.
func installationProbeMiddleware(deps AppDeps) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.URL.Path == "/healthz" || c.Request.URL.Path == "/readyz" {
			installationProbeHandler(deps, http.NotFoundHandler()).ServeHTTP(c.Writer, c.Request)
			c.Abort()
			return
		}
		c.Next()
	}
}
func handleInstallationMetrics(c *gin.Context, deps AppDeps) {
	setNoStoreHeaders(c)
	body, err := deps.ObservabilityService.BuildMetrics(deps.Now().UTC())
	if err != nil {
		c.String(http.StatusInternalServerError, "failed to build metrics\n")
		return
	}
	c.Data(http.StatusOK, "text/plain; version=0.0.4", []byte(body+installationMetrics(installationSnapshot(c.Request.Context(), deps))))
}

type installationWorkerObservation struct {
	worker *installationhealthpkg.Worker
	now    func() time.Time
}

func (o installationWorkerObservation) run(work func() error) error {
	if o.worker == nil {
		return work()
	}
	o.worker.Tick(o.now())
	err := work()
	o.worker.Finish(o.now(), err)
	return err
}
func installationPrunerObservation(values []installationWorkerObservation) installationWorkerObservation {
	if len(values) > 0 {
		return values[0]
	}
	return installationWorkerObservation{}
}
