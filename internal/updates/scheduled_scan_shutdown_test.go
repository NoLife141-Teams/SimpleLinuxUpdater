package updates

import (
	"context"
	"errors"
	"testing"

	"debian-updater/internal/jobs"
)

func TestScheduledScanFailureStateMarksCancellationInterrupted(t *testing.T) {
	status, errorClass := scheduledScanFailureState(context.Canceled)
	if status != jobs.StatusInterrupted || errorClass != "interrupted" {
		t.Fatalf("scheduledScanFailureState(context.Canceled) = %q, %q; want %q, interrupted", status, errorClass, jobs.StatusInterrupted)
	}

	wrapped := errors.New("outer: " + context.Canceled.Error())
	status, errorClass = scheduledScanFailureState(wrapped)
	if status != jobs.StatusFailed || errorClass != "permanent" {
		t.Fatalf("scheduledScanFailureState(non-wrapping error) = %q, %q; want %q, permanent", status, errorClass, jobs.StatusFailed)
	}
}

func TestScheduledScanFailureStateKeepsOrdinaryFailuresFailed(t *testing.T) {
	status, errorClass := scheduledScanFailureState(errors.New("apt update failed"))
	if status != jobs.StatusFailed || errorClass != "permanent" {
		t.Fatalf("scheduledScanFailureState(error) = %q, %q; want %q, permanent", status, errorClass, jobs.StatusFailed)
	}
}
