package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"debian-updater/internal/installationlock"
)

const serverShutdownTimeout = 10 * time.Second
const serverReadHeaderTimeout = 15 * time.Second
const serverWriteTimeout = 60 * time.Second
const serverIdleTimeout = 120 * time.Second
const notificationShutdownTimeout = 10 * time.Second
const maintenanceCancellationDrainTimeout = 5 * time.Second

var actionRunnerShutdownTimeout = 30 * time.Second

type actionRunnerTracker struct {
	mu     sync.Mutex
	active int
	idle   chan struct{}
}

func newActionRunnerTracker() *actionRunnerTracker {
	idle := make(chan struct{})
	close(idle)
	return &actionRunnerTracker{idle: idle}
}

func (t *actionRunnerTracker) start(run func()) {
	t.mu.Lock()
	if t.active == 0 {
		t.idle = make(chan struct{})
	}
	t.active++
	t.mu.Unlock()
	go func() {
		defer t.done()
		run()
	}()
}

func (t *actionRunnerTracker) done() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.active--
	if t.active == 0 {
		close(t.idle)
	}
}

func (t *actionRunnerTracker) wait(ctx context.Context) error {
	t.mu.Lock()
	idle := t.idle
	t.mu.Unlock()
	select {
	case <-idle:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var updateRunners = newActionRunnerTracker()

func startTrackedActionRunner(run func()) { updateRunners.start(run) }

func waitForUpdateRunners() { _ = updateRunners.wait(context.Background()) }

func waitForUpdateRunnersContext(ctx context.Context) error { return updateRunners.wait(ctx) }

func newApplicationHTTPServer(listenAddr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              listenAddr,
		Handler:           applicationHTTPBudgetHandler(handler, defaultHTTPResourceBudgets),
		ReadHeaderTimeout: serverReadHeaderTimeout,
		ReadTimeout:       serverReadHeaderTimeout,
		WriteTimeout:      serverWriteTimeout,
		IdleTimeout:       serverIdleTimeout,
	}
}

func shutdownApplication(server *http.Server, waitForScheduler func(), cancelMaintenance func(), closeNotifications func(context.Context) error) {
	if server != nil {
		serverCtx, cancelServer := context.WithTimeout(context.Background(), serverShutdownTimeout)
		if err := server.Shutdown(serverCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("Failed to shutdown web server cleanly: %v", err)
		}
		cancelServer()
	}
	if waitForScheduler != nil {
		waitForScheduler()
	}

	runnerCtx, cancelRunners := context.WithTimeout(context.Background(), actionRunnerShutdownTimeout)
	runnerErr := waitForUpdateRunnersContext(runnerCtx)
	cancelRunners()
	if runnerErr != nil {
		log.Printf("Action runners exceeded the shutdown grace period; cancelling host maintenance: %v", runnerErr)
		if cancelMaintenance != nil {
			cancelMaintenance()
		}
		drainCtx, cancelDrain := context.WithTimeout(context.Background(), maintenanceCancellationDrainTimeout)
		if err := waitForUpdateRunnersContext(drainCtx); err != nil {
			log.Printf("Action runners did not stop after maintenance cancellation; continuing shutdown: %v", err)
		}
		cancelDrain()
	} else if cancelMaintenance != nil {
		cancelMaintenance()
	}

	if closeNotifications != nil {
		deliveryCtx, cancelDelivery := context.WithTimeout(context.Background(), notificationShutdownTimeout)
		if err := closeNotifications(deliveryCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Failed to drain notification delivery cleanly: %v", err)
		}
		cancelDelivery()
	}
}

func main() {
	if handled, code := runInstallationProbeCommand(os.Args[1:], os.Getenv); handled {
		os.Exit(code)
	}
	if handled, code := runHelperCommand(os.Args[1:], os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	if handled, err := runAdminCommand(os.Args[1:], os.Stdin, os.Stdout); handled {
		if err != nil {
			log.Print(err)
			os.Exit(1)
		}
		return
	}
	installation, err := installationlock.Acquire(dbPath(), true)
	if err != nil {
		log.Fatalf("Cannot acquire installation lock: %v", err)
	}
	defer installation.Close()
	if _, err := loadBackupAutomationDeployment(); err != nil {
		log.Fatalf("Invalid automated backup configuration: %v", err)
	}
	if _, err := loadVulnerabilityScanConfig(); err != nil {
		log.Fatalf("Invalid vulnerability scan configuration: %v", err)
	}
	listenAddr, err := resolveListenAddr(os.Getenv)
	if err != nil {
		log.Fatalf("Invalid HTTP listen configuration: %v", err)
	}
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		log.Fatalf("Failed to bind HTTP listener on %s: %v", listenAddr, err)
	}
	defer listener.Close()
	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	maintenanceCtx, cancelMaintenance := context.WithCancel(context.Background())
	defer cancelMaintenance()
	restoreMaintenanceContext := setApplicationMaintenanceContext(maintenanceCtx)
	defer restoreMaintenanceContext()
	deps := (AppDeps{ScheduledRunReconciliationContext: shutdownCtx}).withDefaults()
	r, err := setupRouterWithDeps(deps)
	if err != nil {
		log.Fatalf("Failed to setup router: %v", err)
	}
	seedVariantCDemoIfRequested(deps)
	if monitor, ok := deps.NotificationService.(interface{ StartConditionMonitoring() }); ok {
		monitor.StartConditionMonitoring()
	}
	startAuditPruner(shutdownCtx, installationWorkerObservation{&deps.InstallationHealth.auditPruner, deps.Now})
	startJobLogPruner(shutdownCtx, deps.CurrentJobManager, installationWorkerObservation{&deps.InstallationHealth.jobLogPruner, deps.Now})
	startPolicyScheduler(deps.PolicyService, deps.PolicyRepository, shutdownCtx, PolicySchedulerOptions{})
	if parseBoolEnvWithDefault(automaticHostFactsRefreshEnabledEnv, true) && deps.HostFactsRefreshWorker != nil {
		deps.HostFactsRefreshWorker.Start(shutdownCtx)
	}
	if deps.VulnerabilityScans != nil {
		deps.VulnerabilityScans.Start(shutdownCtx)
	}
	if deps.BackupAutomation != nil {
		deps.BackupAutomation.Start(shutdownCtx)
	}
	defer StopAuthRateLimiters()
	server := newApplicationHTTPServer(listenAddr, installationProbeHandler(deps, sessionHandler(r)))
	shutdownDone := make(chan struct{})
	go func() {
		<-shutdownCtx.Done()
		deps.InstallationHealth.setPhase("stopping")
		shutdownApplication(server, func() {
			deps.PolicyService.WaitScheduler()
			if deps.BackupAutomation != nil {
				deps.BackupAutomation.Wait()
			}
			if deps.VulnerabilityScans != nil {
				deps.VulnerabilityScans.Wait()
			}
			if deps.HostFactsRefreshWorker != nil {
				deps.HostFactsRefreshWorker.Wait()
			}
		}, cancelMaintenance, func(deliveryCtx context.Context) error {
			return closeNotificationDelivery(deliveryCtx, deps.NotificationService)
		})
		close(shutdownDone)
	}()
	log.Printf("Starting web server on %s", listenAddr)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("Failed to run web server: %v", err)
	}
	if shutdownCtx.Err() != nil {
		<-shutdownDone
	}
}
