// Package installationhealth contains observations of local background work.
// It never infers progress from a timer or exports internal error text.
package installationhealth

import (
	"sync"
	"time"
)

type WorkerSnapshot struct {
	Enabled      bool      `json:"enabled"`
	Running      bool      `json:"running"`
	LastTick     time.Time `json:"last_tick,omitempty"`
	LastProgress time.Time `json:"last_progress,omitempty"`
	LastError    time.Time `json:"last_error,omitempty"`
	Errors       uint64    `json:"errors"`
}

type Worker struct {
	mu    sync.Mutex
	value WorkerSnapshot
}

func (w *Worker) Tick(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.value.LastTick = now.UTC()
	w.value.Running = true
}
func (w *Worker) Finish(now time.Time, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.value.Running = false
	if err != nil {
		w.value.LastError = now.UTC()
		w.value.Errors++
	} else {
		w.value.LastProgress = now.UTC()
	}
}
func (w *Worker) Error(now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.value.LastError = now.UTC()
	w.value.Errors++
}
func (w *Worker) Snapshot(enabled bool) WorkerSnapshot {
	w.mu.Lock()
	defer w.mu.Unlock()
	v := w.value
	v.Enabled = enabled
	return v
}

func (w *Worker) Stop() { w.mu.Lock(); defer w.mu.Unlock(); w.value.Running = false }
