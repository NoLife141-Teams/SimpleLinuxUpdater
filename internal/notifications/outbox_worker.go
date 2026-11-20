package notifications

import (
	"context"
	"slices"
	"time"
)

const maxConcurrentOutboxDeliveries = 3

// An occupied slot holds one distinct destination through outcome persistence.
// Empty slots become SQL NULLs in the claim, recovery, and wake queries.
type outboxDeliveries = [maxConcurrentOutboxDeliveries]*string

type outboxDeliveryResult struct {
	slot int
	ok   bool
}

func (s *Service) run(ctx context.Context) {
	defer close(s.done)
	defer s.health.Stop()
	pruneTicker := time.NewTicker(s.deps.PruneInterval)
	defer pruneTicker.Stop()
	var conditionC <-chan time.Time
	var conditionTicker *time.Ticker
	defer func() {
		if conditionTicker != nil {
			conditionTicker.Stop()
		}
	}()
	retryTimer := time.NewTimer(time.Hour)
	retryTimer.Stop()
	defer retryTimer.Stop()
	var retryC <-chan time.Time

	var active outboxDeliveries
	completed := make(chan outboxDeliveryResult, len(active))
	defer func() {
		// Close stops admissions but keeps the context of in-flight work until
		// it finishes or the shutdown caller cancels it.
		for _, destination := range active {
			if destination != nil {
				<-completed
			}
		}
	}()

	cycleOK := true
	for {
		prune := false
		evaluate := true
		select {
		case <-ctx.Done():
			return
		case <-conditionC:
		case <-s.wake:
		case <-retryC:
		case <-pruneTicker.C:
			prune = true
		case result := <-completed:
			evaluate = false
			active[result.slot] = nil
			cycleOK = cycleOK && result.ok
		}
		retryTimer.Stop()
		retryC = nil
		if !s.beginPersistence(ctx) {
			return
		}
		if s.restoreDeliveryBlocked() {
			s.endPersistence()
			if active == (outboxDeliveries{}) {
				s.health.Stop()
				cycleOK = true
			}
			retryTimer.Reset(s.deps.PollInterval)
			retryC = retryTimer.C
			continue
		}
		s.health.Tick(s.deps.Now())
		if evaluate && s.conditionMonitoringStarted() {
			if conditionTicker == nil {
				conditionTicker = time.NewTicker(s.deps.ConditionPollInterval)
				conditionC = conditionTicker.C
			}
			if err := s.reconcileConditions(ctx); err != nil {
				cycleOK = false
				s.health.Error(s.deps.Now())
				s.deps.Logf("notification condition reconciliation failed: %v", err)
			}
			cycleOK = cycleOK && !s.conditionHealth.degraded()
		}
		if prune {
			if _, err := pruneOutbox(ctx, s.database(), s.deps.Now(), s.deps.Retention); err != nil {
				cycleOK = false
				s.health.Error(s.deps.Now())
				s.deps.Logf("notification outbox pruning failed: %v", err)
			}
		}
		if err := recoverExpiredOutboxClaims(ctx, s.database(), s.deps.Now(), s.deps.ClaimTTL, active); err != nil {
			cycleOK = false
			s.health.Error(s.deps.Now())
			s.deps.Logf("notification outbox claim recovery failed: %v", err)
		}
		nextAt, scheduled, ok := s.pollOutbox(ctx, &active, completed)
		s.endPersistence()
		cycleOK = cycleOK && ok && !s.conditionHealth.degraded()
		if active == (outboxDeliveries{}) {
			if cycleOK {
				s.health.Finish(s.deps.Now(), nil)
			} else {
				s.health.Stop()
			}
			cycleOK = true
		}
		if !scheduled {
			continue
		}
		retryTimer.Reset(max(0, nextAt.Sub(s.deps.Now().UTC())))
		retryC = retryTimer.C
	}
}

func (s *Service) pollOutbox(ctx context.Context, active *outboxDeliveries, completed chan<- outboxDeliveryResult) (time.Time, bool, bool) {
	for slot, destination := range active {
		if destination != nil {
			continue
		}
		if s.queuedDeliveryBlocked(ctx) || !s.tryBeginPersistence() {
			// Resuming persistence wakes the worker. A restore may keep
			// delivery blocked after persistence resumes, so poll the gate.
			return s.deps.Now().Add(s.deps.PollInterval), true, true
		}
		row, err := claimNextOutboxRow(ctx, s.database(), s.deps.Now(), *active)
		if err != nil {
			s.endPersistence()
			s.health.Error(s.deps.Now())
			s.deps.Logf("notification outbox claim failed: %v", err)
			return s.deps.Now().Add(s.deps.PollInterval), true, false
		}
		if row == nil {
			s.endPersistence()
			break
		}
		active[slot] = &row.Destination
		go func() {
			ok := s.processOutboxRow(ctx, *row)
			s.endPersistence()
			completed <- outboxDeliveryResult{slot: slot, ok: ok}
		}()
	}
	if !slices.Contains(active[:], nil) {
		// Completion will make another destination eligible for claiming.
		return time.Time{}, false, true
	}
	nextAt, scheduled, err := s.deps.NextOutboxWake(ctx, s.database(), s.deps.ClaimTTL, *active)
	if err != nil {
		s.health.Error(s.deps.Now())
		s.deps.Logf("notification outbox wake scheduling failed: %v", err)
		return s.deps.Now().Add(s.deps.PollInterval), true, false
	}
	return nextAt, scheduled, true
}

func (s *Service) queuedDeliveryBlocked(ctx context.Context) bool {
	return ctx.Err() != nil || s.isClosing() || s.restoreDeliveryBlocked()
}

func (s *Service) restoreDeliveryBlocked() bool {
	return s.deps.RestoreDeliveryBlocked != nil && s.deps.RestoreDeliveryBlocked()
}
