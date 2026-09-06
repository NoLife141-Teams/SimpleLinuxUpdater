package policies

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	maintenancepkg "debian-updater/internal/maintenance"
	"debian-updater/internal/servers"
)

const (
	RunReasonSchedulerMissed        = "scheduler_missed"
	DefaultSchedulerRecoveryHorizon = 7 * 24 * time.Hour
)

type SchedulerWatermarkStore struct {
	Load func() (time.Time, bool, error)
	Save func(time.Time) error
}

type missedScheduledSlot struct {
	At        time.Time
	PolicyIDs map[int64]struct{}
}

// StartSchedulerWithRecovery starts the policy scheduler with a durable
// watermark. Missed policy occurrences are recorded as skips; only the current
// tick is eligible to launch scheduled work.
func (s *Service) StartSchedulerWithRecovery(ctx context.Context, options SchedulerOptions, store SchedulerWatermarkStore) {
	deps := s.EnsureDeps()
	options = options.WithDefaults()
	s.schedulerOnce.Do(func() {
		done := make(chan struct{})
		s.schedulerMu.Lock()
		s.schedulerDone = done
		s.schedulerMu.Unlock()
		if deps.MarkInterruptedRuns != nil {
			if err := deps.MarkInterruptedRuns(); err != nil {
				deps.Logf("failed to mark interrupted policy runs: %v", err)
			}
		}
		if err := s.ProcessDueWithRecovery(deps.Now(), store); err != nil {
			deps.Logf("scheduled policy tick failed: %v", err)
		}
		go func() {
			defer close(done)
			ticker := time.NewTicker(options.TickInterval)
			defer ticker.Stop()
			for {
				select {
				case tick := <-ticker.C:
					if err := s.ProcessDueWithRecovery(tick, store); err != nil {
						deps.Logf("scheduled policy tick failed: %v", err)
					}
				case <-ctx.Done():
					return
				}
			}
		}()
	})
}

// ProcessDueWithRecovery processes the current tick and persists a monotonic
// scheduler watermark. When the watermark shows that one or more policy
// occurrences were missed, at most the latest missed occurrence per enabled
// policy is materialized, bounded by DefaultSchedulerRecoveryHorizon. Those
// historical occurrences are always skipped rather than executed.
func (s *Service) ProcessDueWithRecovery(now time.Time, store SchedulerWatermarkStore) error {
	deps := s.EnsureDeps()
	s.tickMu.Lock()
	defer s.tickMu.Unlock()

	if deps.Maintenance != nil {
		lease, decision := deps.Maintenance.TryShared(maintenancepkg.WorkScheduled)
		if !decision.Allowed {
			s.RememberMissedTick(now)
			return nil
		}
		defer lease.Close()
	}

	if store.Load == nil || store.Save == nil {
		return s.processDueWithoutDurableRecovery(now)
	}

	currentUTC := now.UTC().Truncate(time.Minute)
	watermark, found, err := store.Load()
	if err != nil {
		return fmt.Errorf("load policy scheduler watermark: %w", err)
	}
	watermark = watermark.UTC().Truncate(time.Minute)

	pending := s.PendingMissedTicks()
	pendingByKey := make(map[string]struct{}, len(pending))
	for _, tick := range pending {
		pendingByKey[MissedTickKey(tick, deps.TimestampLayout)] = struct{}{}
	}

	if found && watermark.Before(currentUTC) {
		slots, err := s.latestMissedScheduledSlots(watermark, currentUTC)
		if err != nil {
			return err
		}
		for _, slot := range slots {
			reason := RunReasonSchedulerMissed
			if _, ok := pendingByKey[MissedTickKey(slot.At, deps.TimestampLayout)]; ok {
				reason = RunReasonMaintenance
			}
			if err := s.processMissedDueSlot(slot.At, reason, slot.PolicyIDs); err != nil {
				return err
			}
		}
	}

	if err := s.ProcessDueSlot(ScheduleRequest{Now: now, Admitted: true}); err != nil {
		return err
	}

	nextWatermark := currentUTC
	if found && watermark.After(nextWatermark) {
		nextWatermark = watermark
	}
	if err := store.Save(nextWatermark); err != nil {
		return fmt.Errorf("save policy scheduler watermark: %w", err)
	}
	for _, tick := range pending {
		if !tick.UTC().Truncate(time.Minute).After(currentUTC) {
			s.ForgetMissedTick(tick)
		}
	}
	return nil
}

func (s *Service) processDueWithoutDurableRecovery(now time.Time) error {
	for _, missedTick := range s.PendingMissedTicks() {
		if err := s.ProcessDueSlot(ScheduleRequest{Now: missedTick, MaintenanceActive: true, Admitted: true}); err != nil {
			return err
		}
		s.ForgetMissedTick(missedTick)
	}
	return s.ProcessDueSlot(ScheduleRequest{Now: now, Admitted: true})
}

// latestMissedScheduledSlots returns one latest missed policy occurrence per
// enabled policy. Each slot carries the policy IDs whose latest occurrence is
// that slot so another daily policy cannot be backfilled merely because a
// weekly policy contributed an older slot. It intentionally does not enumerate
// every minute or every old daily occurrence after a long outage.
func (s *Service) latestMissedScheduledSlots(watermarkUTC, currentUTC time.Time) ([]missedScheduledSlot, error) {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil {
		return nil, errors.New("policy service dependencies are incomplete")
	}
	policies, err := deps.ListPolicies()
	if err != nil {
		return nil, err
	}
	if len(policies) == 0 {
		return []missedScheduledSlot{}, nil
	}

	currentUTC = currentUTC.UTC().Truncate(time.Minute)
	lowerBound := watermarkUTC.UTC().Truncate(time.Minute)
	horizonStart := currentUTC.Add(-DefaultSchedulerRecoveryHorizon)
	if lowerBound.Before(horizonStart) {
		lowerBound = horizonStart
	}
	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.Local
	}
	currentLocal := currentUTC.In(loc)
	currentDay := time.Date(currentLocal.Year(), currentLocal.Month(), currentLocal.Day(), 0, 0, 0, 0, loc)
	maxDaysBack := int(DefaultSchedulerRecoveryHorizon/(24*time.Hour)) + 1

	byKey := make(map[string]*missedScheduledSlot)
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		for daysBack := 0; daysBack <= maxDaysBack; daysBack++ {
			dayStart := currentDay.AddDate(0, 0, -daysBack)
			slotLocal, ok := s.policySlotForDay(policy, dayStart)
			if !ok || !s.PolicyDueAt(policy, slotLocal) {
				continue
			}
			slotUTC := slotLocal.UTC().Truncate(time.Minute)
			if !slotUTC.After(lowerBound) || !slotUTC.Before(currentUTC) {
				continue
			}
			key := MissedTickKey(slotUTC, deps.TimestampLayout)
			entry := byKey[key]
			if entry == nil {
				entry = &missedScheduledSlot{At: slotUTC, PolicyIDs: map[int64]struct{}{}}
				byKey[key] = entry
			}
			entry.PolicyIDs[policy.ID] = struct{}{}
			break
		}
	}

	slots := make([]missedScheduledSlot, 0, len(byKey))
	for _, slot := range byKey {
		slots = append(slots, *slot)
	}
	sort.Slice(slots, func(i, j int) bool { return slots[i].At.Before(slots[j].At) })
	return slots, nil
}

// processMissedDueSlot records an exact scheduled occurrence without ever
// launching its work. Blackout and supersedence semantics stay authoritative;
// canary-and-wave policies materialize only their canary batch for a wholly
// missed occurrence so a previously-started rollout can still continue from
// persisted history on the current tick.
func (s *Service) processMissedDueSlot(slot time.Time, missedReason string, policyIDs map[int64]struct{}) error {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil || deps.LoadOverrides == nil || deps.LoadGlobalBlackouts == nil || deps.SnapshotServers == nil || deps.HandleScheduledRun == nil {
		return errors.New("policy service dependencies are incomplete")
	}
	policies, err := deps.ListPolicies()
	if err != nil {
		return err
	}
	if len(policies) == 0 {
		return nil
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return err
	}
	slotLocal := slot.In(deps.CurrentLocation()).Truncate(time.Minute)
	if deps.ApplicationTime != nil {
		occurrence := deps.ApplicationTime.Current().ResolveLocal(slotLocal, slotLocal.Hour(), slotLocal.Minute())
		if occurrence.Kind == apptimepkg.OccurrenceNonexistent {
			return nil
		}
	}
	serversSnapshot := deps.SnapshotServers()

	var queueErrs []error
	recordSkipped := func(policy Policy, server servers.Server, scheduledForUTC, reason string) {
		result := s.handleScheduledRun(policy, server, scheduledForUTC, reason, true)
		if result.Err == nil {
			return
		}
		queueErrs = append(queueErrs, fmt.Errorf(
			"record missed scheduled run failed: policy_id=%d policy_name=%q server=%q scheduled_for_utc=%q reason=%q: %w",
			policy.ID,
			policy.Name,
			server.Name,
			scheduledForUTC,
			reason,
			result.Err,
		))
	}

	candidatesByServer := make(map[string][]ScheduledCandidate)
	for _, policy := range policies {
		if _, wanted := policyIDs[policy.ID]; !wanted {
			continue
		}
		if !policy.Enabled || !s.PolicyDueAt(policy, slotLocal) {
			continue
		}
		scheduledForUTC := CanonicalScheduledForUTC(slotLocal, deps.TimestampLayout, deps.CurrentLocation)
		matchedServers := make([]servers.Server, 0)
		for _, server := range serversSnapshot {
			if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
				matchedServers = append(matchedServers, server)
			}
		}
		sort.Slice(matchedServers, func(i, j int) bool {
			return strings.ToLower(matchedServers[i].Name) < strings.ToLower(matchedServers[j].Name)
		})
		if policy.RolloutMode == RolloutCanaryWaves && len(matchedServers) > 0 {
			names := make([]string, 0, len(matchedServers))
			serverByName := make(map[string]servers.Server, len(matchedServers))
			for _, server := range matchedServers {
				names = append(names, server.Name)
				serverByName[server.Name] = server
			}
			batches := BuildRolloutBatches(policy, names)
			matchedServers = matchedServers[:0]
			if len(batches) > 0 {
				for _, name := range batches[0].Servers {
					matchedServers = append(matchedServers, serverByName[name])
				}
			}
		}
		for _, server := range matchedServers {
			if missedReason == RunReasonMaintenance {
				recordSkipped(policy, server, scheduledForUTC, RunReasonMaintenance)
				continue
			}
			if s.BlackoutApplies(slotLocal, globalBlackouts) || s.BlackoutApplies(slotLocal, policy.PolicyBlackouts) {
				recordSkipped(policy, server, scheduledForUTC, RunReasonBlackout)
				continue
			}
			candidatesByServer[server.Name] = append(candidatesByServer[server.Name], ScheduledCandidate{
				Policy:          policy,
				Server:          server,
				ScheduledForUTC: scheduledForUTC,
			})
		}
	}

	serverNames := make([]string, 0, len(candidatesByServer))
	for name := range candidatesByServer {
		serverNames = append(serverNames, name)
	}
	sort.Slice(serverNames, func(i, j int) bool {
		left := strings.ToLower(serverNames[i])
		right := strings.ToLower(serverNames[j])
		if left == right {
			return serverNames[i] < serverNames[j]
		}
		return left < right
	})
	for _, serverName := range serverNames {
		candidates := candidatesByServer[serverName]
		if len(candidates) == 0 {
			continue
		}
		sort.Slice(candidates, func(i, j int) bool {
			return s.ComparePolicyCandidates(candidates[i], candidates[j])
		})
		winner := candidates[0]
		for _, skipped := range candidates[1:] {
			recordSkipped(skipped.Policy, skipped.Server, skipped.ScheduledForUTC, RunReasonSuperseded)
		}
		recordSkipped(winner.Policy, winner.Server, winner.ScheduledForUTC, missedReason)
	}
	if len(queueErrs) > 0 {
		return fmt.Errorf("missed scheduled policy processing encountered %d error(s): %w", len(queueErrs), errors.Join(queueErrs...))
	}
	return nil
}
