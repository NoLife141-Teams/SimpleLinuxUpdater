package policies

import (
	"testing"
	"time"
)

func TestResumedSchedulerSkipsOverdueAcceptedWavesAndAllowsNewOccurrences(t *testing.T) {
	h := newSchedule472Harness(t, "2026-11-19T02:00:00Z")
	origin := h.now.Add(time.Hour)
	overdue := h.policy(t, func(p *Policy) {
		p.RolloutMode = RolloutCanaryWaves
		p.WaveSize = 1
		p.WaveDelayMinutes = 5
	})
	resumeAt := origin.Add(6 * time.Minute)
	current := h.policy(t, func(p *Policy) {
		p.Name = "First policy after isolation"
		p.TimeLocal = resumeAt.Format("15:04")
	})
	h.tick(t, origin)
	isolated := true
	deps := h.service.EnsureDeps()
	deps.ScheduledWork = func(at time.Time) (time.Time, bool) {
		return resumeAt, isolated || at.Before(resumeAt)
	}
	h.service = NewService(deps)
	h.now = origin.Add(5 * time.Minute)
	store := SchedulerWatermarkStore{Load: h.repo.LoadSchedulerWatermark, Save: h.repo.SaveSchedulerWatermark}
	if err := h.service.ProcessDueWithRecovery(h.now, store); err != nil {
		t.Fatal(err)
	}
	if runs := h.runs(t); len(runs) != 1 || runs[0].PolicyID != overdue.ID || runs[0].Status != RunSucceeded {
		t.Fatalf("unexpected runs while isolated: %+v", runs)
	}
	isolated = false
	h.now = resumeAt
	// Even a watermark behind the isolated wave must not dispatch it.
	if err := h.repo.SaveSchedulerWatermark(origin); err != nil {
		t.Fatal(err)
	}
	if err := h.service.ProcessDueWithRecovery(h.now, store); err != nil {
		t.Fatal(err)
	}
	var missedWave, currentRuns int
	for _, run := range h.runs(t) {
		switch {
		case run.PolicyID == overdue.ID && run.ServerName == "b" &&
			run.Status == RunSkipped && run.Reason == RunReasonSchedulerMissed:
			missedWave++
		case run.PolicyID == current.ID && run.Status == RunSucceeded:
			currentRuns++
		}
	}
	if missedWave != 1 || currentRuns != 3 {
		t.Fatalf("want one missed wave and three current runs, got %+v", h.runs(t))
	}
}
