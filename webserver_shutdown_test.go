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
