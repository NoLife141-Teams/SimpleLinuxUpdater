package main

import (
	"context"
	"testing"
	"time"
)

func TestShutdownApplicationWaitsForActionRunnersBeforeClosingNotifications(t *testing.T) {
	waitForUpdateRunners()
	releaseRunner := make(chan struct{})
	startTrackedActionRunner(func() {
		<-releaseRunner
	})

	maintenanceCancelled := make(chan struct{})
	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		shutdownApplication(nil, nil, func() { close(maintenanceCancelled) }, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-notificationsClosed:
		t.Fatal("notification delivery closed before the active action runner finished")
	case <-maintenanceCancelled:
		t.Fatal("maintenance was cancelled before the active action runner finished within its grace period")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRunner)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after the action runner completed")
	}
	select {
	case <-maintenanceCancelled:
	default:
		t.Fatal("maintenance lifecycle was not closed after runners drained")
	}
}

func TestShutdownApplicationCancelsMaintenanceAfterRunnerGracePeriod(t *testing.T) {
	waitForUpdateRunners()
	originalTimeout := actionRunnerShutdownTimeout
	actionRunnerShutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { actionRunnerShutdownTimeout = originalTimeout })

	maintenanceCtx, cancelMaintenance := context.WithCancel(context.Background())
	startTrackedActionRunner(func() {
		<-maintenanceCtx.Done()
	})
	t.Cleanup(waitForUpdateRunners)

	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})
	started := time.Now()
	go func() {
		shutdownApplication(nil, nil, cancelMaintenance, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-maintenanceCtx.Done():
		if elapsed := time.Since(started); elapsed < actionRunnerShutdownTimeout {
			t.Fatalf("maintenance cancelled after %s, before runner grace %s", elapsed, actionRunnerShutdownTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("maintenance was not cancelled after the runner grace period")
	}
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after cooperative runner cancellation")
	}
	select {
	case <-notificationsClosed:
	default:
		t.Fatal("notification shutdown was not attempted after cooperative runner cancellation")
	}
}

func TestShutdownApplicationJoinsSchedulerBeforeDrainingActionRunners(t *testing.T) {
	waitForUpdateRunners()
	allowAdmission := make(chan struct{})
	releaseRunner := make(chan struct{})
	schedulerJoined := make(chan struct{})
	maintenanceCancelled := make(chan struct{})
	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})

	go func() {
		shutdownApplication(nil, func() {
			<-allowAdmission
			startTrackedActionRunner(func() {
				<-releaseRunner
			})
			close(schedulerJoined)
		}, func() { close(maintenanceCancelled) }, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-notificationsClosed:
		t.Fatal("notification delivery closed before the scheduler joined")
	case <-maintenanceCancelled:
		t.Fatal("maintenance cancelled before the scheduler joined")
	case <-time.After(50 * time.Millisecond):
	}
	close(allowAdmission)
	select {
	case <-schedulerJoined:
	case <-time.After(time.Second):
		t.Fatal("scheduler did not join")
	}
	select {
	case <-notificationsClosed:
		t.Fatal("notification delivery closed before the scheduler-admitted runner finished")
	case <-maintenanceCancelled:
		t.Fatal("maintenance cancelled before the scheduler-admitted runner finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRunner)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after the runner completed")
	}
}
