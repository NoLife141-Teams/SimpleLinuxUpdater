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
)

const serverShutdownTimeout = 10 * time.Second
const notificationShutdownTimeout = 10 * time.Second

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

func shutdownApplication(server *http.Server, waitForScheduler func(), closeNotifications func(context.Context) error) {
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
	if err := waitForUpdateRunnersContext(runnerCtx); err != nil {
		log.Printf("Action runners exceeded the shutdown grace period; continuing shutdown: %v", err)
	}
	cancelRunners()

	if closeNotifications != nil {
		deliveryCtx, cancelDelivery := context.WithTimeout(context.Background(), notificationShutdownTimeout)
		if err := closeNotifications(deliveryCtx); err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			log.Printf("Failed to drain notification delivery cleanly: %v", err)
		}
		cancelDelivery()
	}
}

func main() {
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
	deps := (AppDeps{ScheduledRunReconciliationContext: shutdownCtx}).withDefaults()
	r, err := setupRouterWithDeps(deps)
	if err != nil {
		log.Fatalf("Failed to setup router: %v", err)
	}
	seedVariantCDemoIfRequested(deps)
	startAuditPruner(shutdownCtx)
	startJobLogPruner(shutdownCtx, deps.CurrentJobManager)
	startPolicyScheduler(deps.PolicyService, shutdownCtx, PolicySchedulerOptions{})
	if parseBoolEnvWithDefault(automaticHostFactsRefreshEnabledEnv, true) && deps.HostFactsRefreshWorker != nil {
		deps.HostFactsRefreshWorker.Start(shutdownCtx)
	}
	defer StopAuthRateLimiters()
	server := &http.Server{
		Addr:         listenAddr,
		Handler:      sessionHandler(r),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 60 * time.Second,
		IdleTimeout:  120 * time.Second,
	}
	shutdownDone := make(chan struct{})
	go func() {
		<-shutdownCtx.Done()
		shutdownApplication(server, func() {
			deps.PolicyService.WaitScheduler()
			if deps.HostFactsRefreshWorker != nil {
				deps.HostFactsRefreshWorker.Wait()
			}
		}, func(deliveryCtx context.Context) error {
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
