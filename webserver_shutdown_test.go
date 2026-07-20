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

	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		shutdownApplication(nil, nil, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-notificationsClosed:
		t.Fatal("notification delivery closed before the active action runner finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseRunner)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after the action runner completed")
	}
}

func TestShutdownApplicationHonorsRunnerGracePeriod(t *testing.T) {
	waitForUpdateRunners()
	originalTimeout := actionRunnerShutdownTimeout
	actionRunnerShutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { actionRunnerShutdownTimeout = originalTimeout })

	releaseRunner := make(chan struct{})
	released := false
	t.Cleanup(func() {
		if !released {
			close(releaseRunner)
		}
		waitForUpdateRunners()
	})
	startTrackedActionRunner(func() {
		<-releaseRunner
	})

	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})
	go func() {
		shutdownApplication(nil, nil, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown exceeded the action runner grace period")
	}
	select {
	case <-notificationsClosed:
	default:
		t.Fatal("notification shutdown was not attempted after the runner grace period")
	}
	released = true
	close(releaseRunner)
	waitForUpdateRunners()
}

func TestShutdownApplicationJoinsSchedulerBeforeDrainingActionRunners(t *testing.T) {
	waitForUpdateRunners()
	allowAdmission := make(chan struct{})
	releaseRunner := make(chan struct{})
	schedulerJoined := make(chan struct{})
	notificationsClosed := make(chan struct{})
	shutdownDone := make(chan struct{})

	go func() {
		shutdownApplication(nil, func() {
			<-allowAdmission
			startTrackedActionRunner(func() {
				<-releaseRunner
			})
			close(schedulerJoined)
		}, func(context.Context) error {
			close(notificationsClosed)
			return nil
		})
		close(shutdownDone)
	}()

	select {
	case <-notificationsClosed:
		t.Fatal("notification delivery closed before the scheduler joined")
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
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseRunner)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after the runner completed")
	}
}
