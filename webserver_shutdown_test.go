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
		shutdownApplication(nil, func(context.Context) error {
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

func TestShutdownApplicationContinuesWaitingAfterRunnerGracePeriod(t *testing.T) {
	waitForUpdateRunners()
	originalTimeout := actionRunnerShutdownTimeout
	actionRunnerShutdownTimeout = 20 * time.Millisecond
	t.Cleanup(func() { actionRunnerShutdownTimeout = originalTimeout })

	releaseRunner := make(chan struct{})
	startTrackedActionRunner(func() {
		<-releaseRunner
	})

	shutdownDone := make(chan struct{})
	go func() {
		shutdownApplication(nil, nil)
		close(shutdownDone)
	}()

	select {
	case <-shutdownDone:
		t.Fatal("application shutdown returned while an action runner was still active")
	case <-time.After(75 * time.Millisecond):
	}

	close(releaseRunner)
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("application shutdown did not finish after the runner completed")
	}
}
