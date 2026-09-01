package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"sync"

	healthpkg "debian-updater/internal/health"
	maintenancepkg "debian-updater/internal/maintenance"
	serverpkg "debian-updater/internal/servers"
)

const (
	automaticHostFactsRefreshEnabledEnv = "DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED"
	automaticHostFactsRefreshSource     = "automatic_periodic"
	automaticHostFactsRefreshDial       = "automatic_facts_refresh.ssh_dial"
)

// hostFactsRefreshAdmission gives scheduled maintenance admission priority over
// low-priority automatic fact collection without changing manual-action rules.
type hostFactsRefreshAdmission struct {
	mu       sync.Mutex
	byServer map[string]*sync.Mutex
}

func newHostFactsRefreshAdmission() *hostFactsRefreshAdmission {
	return &hostFactsRefreshAdmission{byServer: map[string]*sync.Mutex{}}
}

func (g *hostFactsRefreshAdmission) serverGate(name string) *sync.Mutex {
	if g == nil {
		return nil
	}
	key := strings.ToLower(strings.TrimSpace(name))
	g.mu.Lock()
	defer g.mu.Unlock()
	gate := g.byServer[key]
	if gate == nil {
		gate = &sync.Mutex{}
		g.byServer[key] = gate
	}
	return gate
}

func (g *hostFactsRefreshAdmission) TryRefresh(name string) (func(), bool) {
	gate := g.serverGate(name)
	if gate == nil || !gate.TryLock() {
		return func() {}, false
	}
	return gate.Unlock, true
}

func (g *hostFactsRefreshAdmission) AcquireScheduled(name string) func() {
	gate := g.serverGate(name)
	if gate == nil {
		return func() {}
	}
	gate.Lock()
	return gate.Unlock
}

func newAutomaticHostFactsRefreshWorker(deps AppDeps) *healthpkg.RefreshWorker {
	return healthpkg.NewRefreshWorker(healthpkg.RefreshWorkerDeps{
		Now: deps.Now,
		SnapshotServers: func() []Server {
			if deps.ServerState == nil {
				return nil
			}
			return deps.ServerState.CloneServers()
		},
		LatestFacts: func() (map[string]healthpkg.CollectedFacts, error) {
			if deps.HostHealthObservation == nil {
				return map[string]healthpkg.CollectedFacts{}, nil
			}
			return deps.HostHealthObservation.Latest()
		},
		Refresh: func(ctx context.Context, server Server) healthpkg.RefreshAttempt {
			return automaticHostFactsRefreshAttempt(ctx, deps, server)
		},
		Logf: log.Printf,
	}, healthpkg.RefreshWorkerOptions{})
}

func automaticHostFactsRefreshAttempt(ctx context.Context, deps AppDeps, candidate Server) healthpkg.RefreshAttempt {
	if ctx == nil {
		ctx = context.Background()
	}
	if deps.HostFactsRefreshAdmission != nil {
		release, admitted := deps.HostFactsRefreshAdmission.TryRefresh(candidate.Name)
		if !admitted {
			return healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptDeferred, Reason: "scheduled maintenance is being admitted", ReasonCode: "scheduled_admission"}
		}
		defer release()
	}
	if deps.MaintenanceCoordinator == nil {
		return healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptDeferred, Reason: "maintenance coordination is unavailable", ReasonCode: "maintenance_unavailable"}
	}
	lease, decision := deps.MaintenanceCoordinator.TryShared(maintenancepkg.WorkScheduled)
	if !decision.Allowed {
		return healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptDeferred, Reason: "exclusive application maintenance is active", ReasonCode: "maintenance_active"}
	}
	defer lease.Close()

	if deps.ServerState == nil || deps.UpdateService == nil {
		attempt := healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptFailed, Reason: "automatic host facts refresh is unavailable", ReasonCode: "runtime_unavailable"}
		recordAutomaticHostFactsRefreshAudit(deps, candidate, attempt)
		return attempt
	}
	server, found := deps.ServerState.FindByName(candidate.Name)
	if !found {
		attempt := healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptDeferred, Reason: "server no longer exists", ReasonCode: "server_missing"}
		recordAutomaticHostFactsRefreshAudit(deps, candidate, attempt)
		return attempt
	}
	if deps.MaintenanceReadiness != nil {
		readiness := deps.MaintenanceReadiness([]Server{server})[server.Name]
		if !readiness.Ready {
			attempt := healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptDeferred, Reason: readiness.Message, ReasonCode: readiness.Code}
			recordAutomaticHostFactsRefreshAudit(deps, server, attempt)
			return attempt
		}
	}
	server, previousStatus, err := deps.ServerState.BeginTransientAction(server.Name, "facts_refresh")
	if err != nil {
		attempt := healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptFailed, Reason: err.Error(), ReasonCode: "admission_failed", Err: err}
		if errors.Is(err, serverpkg.ErrActionInProgress) {
			attempt.State = healthpkg.RefreshAttemptDeferred
			attempt.Reason = "another server action is active"
			attempt.ReasonCode = "busy"
		} else if errors.Is(err, sql.ErrNoRows) {
			attempt.State = healthpkg.RefreshAttemptDeferred
			attempt.Reason = "server no longer exists"
			attempt.ReasonCode = "server_missing"
		}
		recordAutomaticHostFactsRefreshAudit(deps, server, attempt)
		return attempt
	}
	defer deps.ServerState.RestoreStatusSnapshot(server.Name, previousStatus)

	record, err := deps.UpdateService.RefreshServerFacts(ctx, server, automaticHostFactsRefreshDial)
	attempt := healthpkg.RefreshAttempt{State: healthpkg.RefreshAttemptSucceeded, Facts: record}
	if err != nil {
		attempt.State = healthpkg.RefreshAttemptFailed
		attempt.Reason = "automatic host facts refresh failed"
		attempt.ReasonCode = "refresh_failed"
		attempt.Err = err
	} else if !healthpkg.FactsHealthComplete(record) {
		attempt.State = healthpkg.RefreshAttemptIncomplete
		attempt.Reason = "host facts refresh returned incomplete disk or APT health data"
		attempt.ReasonCode = "incomplete_health"
	}
	recordAutomaticHostFactsRefreshAudit(deps, server, attempt)
	if err == nil && deps.NotifyDashboardEvent != nil {
		deps.NotifyDashboardEvent("host-facts-refreshed")
	}
	return attempt
}

func recordAutomaticHostFactsRefreshAudit(deps AppDeps, server Server, attempt healthpkg.RefreshAttempt) {
	if deps.AuditService == nil || strings.TrimSpace(server.Name) == "" {
		return
	}
	status := "success"
	message := "Automatic host facts refreshed"
	switch attempt.State {
	case healthpkg.RefreshAttemptIncomplete:
		status = "warning"
		message = "Automatic host facts refresh returned incomplete health data"
	case healthpkg.RefreshAttemptDeferred:
		status = "ignored"
		message = "Automatic host facts refresh deferred"
	case healthpkg.RefreshAttemptFailed:
		status = "failure"
		message = "Automatic host facts refresh failed"
	}
	meta := map[string]any{
		"source":      automaticHostFactsRefreshSource,
		"reason":      attempt.Reason,
		"reason_code": attempt.ReasonCode,
	}
	if attempt.Err != nil {
		meta["error"] = attempt.Err.Error()
	}
	if strings.TrimSpace(attempt.Facts.CollectedAt) != "" {
		meta["collected_at"] = attempt.Facts.CollectedAt
		meta["disk_status"] = attempt.Facts.DiskStatus
		meta["apt_status"] = attempt.Facts.AptStatus
		meta["reboot_required"] = attempt.Facts.RebootRequired
	}
	if err := deps.AuditService.Record("system", "", serverFactsRefreshAction, "server", server.Name, status, message, meta); err != nil {
		log.Printf("automatic host facts refresh audit failed for %q: %v", server.Name, err)
	}
}
