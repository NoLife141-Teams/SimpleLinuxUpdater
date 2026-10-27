package installationhealth

import (
	"errors"
	"testing"
	"time"
)

func TestWorkerObservationsUseInjectedInstants(t *testing.T) {
	var worker Worker
	now := time.Date(2026, 10, 27, 1, 2, 3, 0, time.UTC)
	worker.Tick(now)
	if got := worker.Snapshot(true); !got.Running || !got.LastTick.Equal(now) || !got.LastProgress.IsZero() {
		t.Fatal(got)
	}
	failed := now.Add(time.Second)
	worker.Finish(failed, errors.New("private URL password path"))
	if got := worker.Snapshot(false); got.Enabled || got.Running || got.Errors != 1 || !got.LastError.Equal(failed) || !got.LastProgress.IsZero() {
		t.Fatal(got)
	}
	progressed := now.Add(2 * time.Second)
	worker.Tick(progressed)
	worker.Finish(progressed, nil)
	if got := worker.Snapshot(true); got.Errors != 1 || !got.LastProgress.Equal(progressed) || !got.LastError.Equal(failed) {
		t.Fatal(got)
	}
}
