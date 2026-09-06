package policies

import (
	"context"
	"crypto/sha256"
	"encoding/json"
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
	Load                 func() (time.Time, bool, error)
	Save                 func(time.Time) error
	LoadStateFingerprint func() (string, bool, error)
	SaveStateFingerprint func(string) error
	HasRecoveryScope     func(int64, string) (bool, error)
	MarkRecoveryScope    func(int64, string) error
}

type missedScheduledSlot struct {
	At        time.Time
	PolicyIDs map[int64]struct{}
}

type missedPolicyCandidate struct {
	ScheduledCandidate
	Selected bool
	Existing bool
}

type schedulerStateOverride struct {
	PolicyID   int64  `json:"policy_id"`
	ServerName string `json:"server_name"`
	Disabled   bool   `json:"disabled"`
}

type schedulerStateServer struct {
	Name string   `json:"name"`
	Tags []string `json:"tags"`
}

type schedulerStateFingerprintPayload struct {
	Policies        []Policy                 `json:"policies"`
	Overrides       []schedulerStateOverride `json:"overrides"`
	GlobalBlackouts []BlackoutWindow         `json:"global_blackouts"`
	Servers         []schedulerStateServer   `json:"servers"`
	Location        string                   `json:"location"`
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

	fingerprintEnabled := store.LoadStateFingerprint != nil && store.SaveStateFingerprint != nil
	currentFingerprint := ""
	historicalStateKnown := true
	if fingerprintEnabled {
		currentFingerprint, err = s.schedulerRecoveryStateFingerprint()
		if err != nil {
			return fmt.Errorf("fingerprint policy scheduler state: %w", err)
		}
		storedFingerprint, fingerprintFound, loadErr := store.LoadStateFingerprint()
		if loadErr != nil {
			return fmt.Errorf("load policy scheduler state fingerprint: %w", loadErr)
		}
		if found {
			historicalStateKnown = fingerprintFound && strings.TrimSpace(storedFingerprint) == currentFingerprint
		}
		if found && !historicalStateKnown {
			deps.Logf("policy scheduler state changed since the durable watermark; rebasing without historical reconstruction")
		}
	}

	pending := s.PendingMissedTicks()
	processedPending := make(map[string]struct{}, len(pending))
	if historicalStateKnown {
		// In-process maintenance ticks are explicit history. Replay them only
		// while the scheduling state still matches the durable checkpoint;
		// otherwise their per-server historical outcome cannot be reconstructed
		// safely from the current inventory/overrides/blackouts/timezone.
		for _, tick := range pending {
			tickUTC := tick.UTC().Truncate(time.Minute)
			if tickUTC.After(currentUTC) {
				continue
			}
			if err := s.ProcessDueSlot(ScheduleRequest{Now: tick, MaintenanceActive: true, Admitted: true}); err != nil {
				return err
			}
			processedPending[MissedTickKey(tickUTC, deps.TimestampLayout)] = struct{}{}
		}
	}

	if found && historicalStateKnown && watermark.Before(currentUTC) {
		slots, err := s.latestMissedScheduledSlots(watermark, currentUTC)
		if err != nil {
			return err
		}
		for _, slot := range slots {
			if _, maintenanceTick := processedPending[MissedTickKey(slot.At, deps.TimestampLayout)]; maintenanceTick {
				continue
			}
			if err := s.processMissedDueSlotWithStore(slot.At, RunReasonSchedulerMissed, slot.PolicyIDs, store); err != nil {
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
	if fingerprintEnabled {
		if err := store.SaveStateFingerprint(currentFingerprint); err != nil {
			return fmt.Errorf("save policy scheduler state fingerprint: %w", err)
		}
	}
	for _, tick := range pending {
		key := MissedTickKey(tick, deps.TimestampLayout)
		_, processed := processedPending[key]
		if processed || (!historicalStateKnown && !tick.UTC().Truncate(time.Minute).After(currentUTC)) {
			s.ForgetMissedTick(tick)
		}
	}
	return nil
}

func (s *Service) schedulerRecoveryStateFingerprint() (string, error) {
	deps := s.EnsureDeps()
	if deps.ListPolicies == nil || deps.LoadOverrides == nil || deps.LoadGlobalBlackouts == nil || deps.SnapshotServers == nil {
		return "", errors.New("policy service dependencies are incomplete")
	}
	policies, err := deps.ListPolicies()
	if err != nil {
		return "", err
	}
	overrides, err := deps.LoadOverrides()
	if err != nil {
		return "", err
	}
	globalBlackouts, err := deps.LoadGlobalBlackouts()
	if err != nil {
		return "", err
	}
	serversSnapshot := deps.SnapshotServers()

	overrideState := make([]schedulerStateOverride, 0)
	for policyID, perPolicy := range overrides {
		for serverName, disabled := range perPolicy {
			overrideState = append(overrideState, schedulerStateOverride{
				PolicyID:   policyID,
				ServerName: strings.TrimSpace(serverName),
				Disabled:   disabled,
			})
		}
	}
	sort.Slice(overrideState, func(i, j int) bool {
		if overrideState[i].PolicyID != overrideState[j].PolicyID {
			return overrideState[i].PolicyID < overrideState[j].PolicyID
		}
		left := strings.ToLower(overrideState[i].ServerName)
		right := strings.ToLower(overrideState[j].ServerName)
		if left == right {
			return overrideState[i].ServerName < overrideState[j].ServerName
		}
		return left < right
	})

	serverState := make([]schedulerStateServer, 0, len(serversSnapshot))
	for _, server := range serversSnapshot {
		serverState = append(serverState, schedulerStateServer{
			Name: strings.TrimSpace(server.Name),
			Tags: NormalizeStringList(server.Tags),
		})
	}
	sort.Slice(serverState, func(i, j int) bool {
		left := strings.ToLower(serverState[i].Name)
		right := strings.ToLower(serverState[j].Name)
		if left == right {
			return serverState[i].Name < serverState[j].Name
		}
		return left < right
	})

	loc := deps.CurrentLocation()
	locationName := ""
	if loc != nil {
		locationName = loc.String()
	}
	payload := schedulerStateFingerprintPayload{
		Policies:        policies,
		Overrides:       overrideState,
		GlobalBlackouts: globalBlackouts,
		Servers:         serverState,
		Location:        locationName,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%x", digest[:]), nil
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
// weekly policy contributed an older slot. Historical reconstruction is also
// bounded by the policy's latest persisted configuration timestamp so recovery
// never projects current configuration into a time before it was effective.
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
		effectiveAt, timestampsValid := policyRecoveryBoundary(policy, deps.TimestampLayout)
		if !timestampsValid {
			deps.Logf(
				"skipping missed occurrence recovery for policy %d because persisted timestamps are invalid: created_at=%q updated_at=%q",
				policy.ID,
				policy.CreatedAt,
				policy.UpdatedAt,
			)
			continue
		}
		for daysBack := 0; daysBack <= maxDaysBack; daysBack++ {
			dayStart := currentDay.AddDate(0, 0, -daysBack)
			slotLocal, ok := s.policySlotForDay(policy, dayStart)
			if !ok || !s.PolicyDueAt(policy, slotLocal) {
				continue
			}
			slotUTC := slotLocal.UTC().Truncate(time.Minute)
			if !effectiveAt.IsZero() && slotUTC.Before(effectiveAt) {
				continue
			}
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

// policyRecoveryBoundary returns the earliest instant at which the current
// persisted policy representation is safe to use for historical recovery. A
// policy edit replaces its configuration without version history, so UpdatedAt
// is a conservative effective boundary for schedule, target, priority and
// blackout semantics. CreatedAt still bounds never-edited policies.
func policyRecoveryBoundary(policy Policy, timestampLayout string) (time.Time, bool) {
	boundary := time.Time{}
	for _, raw := range []string{policy.CreatedAt, policy.UpdatedAt} {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		parsed, ok := parsePolicyInstant(raw, timestampLayout)
		if !ok {
			return time.Time{}, false
		}
		if boundary.IsZero() || parsed.After(boundary) {
			boundary = parsed
		}
	}
	return boundary, true
}

func parsePolicyInstant(raw, timestampLayout string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	layouts := []string{strings.TrimSpace(timestampLayout), time.RFC3339Nano, time.RFC3339}
	seen := map[string]struct{}{}
	for _, layout := range layouts {
		if layout == "" {
			continue
		}
		if _, exists := seen[layout]; exists {
			continue
		}
		seen[layout] = struct{}{}
		parsed, err := time.Parse(layout, raw)
		if err == nil {
			return parsed.UTC(), true
		}
	}
	return time.Time{}, false
}

// processMissedDueSlot records an exact historical occurrence without ever
// launching its work. All policies that were due and whose current persisted
// configuration already existed at the slot participate in priority selection,
// but only policies selected by the bounded recovery pass may materialize new
// Scheduled Run rows.
func (s *Service) processMissedDueSlot(slot time.Time, missedReason string, policyIDs map[int64]struct{}) error {
	return s.processMissedDueSlotWithStore(slot, missedReason, policyIDs, SchedulerWatermarkStore{})
}

func (s *Service) processMissedDueSlotWithStore(slot time.Time, missedReason string, policyIDs map[int64]struct{}, store SchedulerWatermarkStore) error {
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
	loc := deps.CurrentLocation()
	if loc == nil {
		loc = time.Local
	}
	slotLocal := slot.In(loc).Truncate(time.Minute)
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

	candidatesByServer := make(map[string][]missedPolicyCandidate)
	for _, policy := range policies {
		if !policy.Enabled {
			continue
		}
		rolloutSlot, rolloutDue := s.rolloutScheduledSlot(policy, slotLocal)
		if !rolloutDue {
			continue
		}
		originUTC := rolloutSlot.UTC().Truncate(time.Minute)
		effectiveAt, timestampsValid := policyRecoveryBoundary(policy, deps.TimestampLayout)
		if !timestampsValid {
			deps.Logf(
				"excluding policy %d from historical competition because persisted timestamps are invalid: created_at=%q updated_at=%q",
				policy.ID,
				policy.CreatedAt,
				policy.UpdatedAt,
			)
			continue
		}
		if !effectiveAt.IsZero() && originUTC.Before(effectiveAt) {
			continue
		}

		_, selectedByRecovery := policyIDs[policy.ID]
		scheduledForUTC := CanonicalScheduledForUTC(rolloutSlot, deps.TimestampLayout, deps.CurrentLocation)
		existingByServer := map[string]struct{}{}
		rolloutRuns := []Run{}
		recoveryManagedRollout := false
		if policy.RolloutMode == RolloutCanaryWaves {
			if deps.ListRolloutRuns == nil {
				return errors.New("policy rollout history dependency is incomplete")
			}
			rolloutRuns, err = deps.ListRolloutRuns([]RolloutRunScope{{PolicyID: policy.ID, ScheduledForUTC: scheduledForUTC}})
			if err != nil {
				return err
			}
			if selectedByRecovery {
				recoveryManagedRollout, err = schedulerRecoveryScopeOwned(store, policy.ID, scheduledForUTC, rolloutRuns)
				if err != nil {
					return err
				}
				if recoveryManagedRollout {
					for _, run := range rolloutRuns {
						existingByServer[strings.ToLower(strings.TrimSpace(run.ServerName))] = struct{}{}
					}
				}
			}
		}

		matchedServers := make([]servers.Server, 0)
		for _, server := range serversSnapshot {
			if s.PolicyMatchesServer(policy, server, MatchContext{Overrides: overrides}) {
				matchedServers = append(matchedServers, server)
			}
		}
		sort.Slice(matchedServers, func(i, j int) bool {
			return strings.ToLower(matchedServers[i].Name) < strings.ToLower(matchedServers[j].Name)
		})

		materializeSelected := selectedByRecovery
		if policy.RolloutMode == RolloutCanaryWaves {
			materializeSelected = selectedByRecovery && recoveryManagedRollout
		}

		// A selected rollout recovery intentionally closes every missing target
		// as historical output so a later current tick cannot manufacture
		// downstream rollout_gate rows. Rollouts that are not recovery-owned
		// instead participate only as historical competitors using their actual
		// persisted gate/batch state.
		if policy.RolloutMode == RolloutCanaryWaves && !materializeSelected {
			if s.BlackoutApplies(rolloutSlot, globalBlackouts) || s.BlackoutApplies(rolloutSlot, policy.PolicyBlackouts) {
				continue
			}
			serverByName := make(map[string]servers.Server, len(matchedServers))
			matchedNames := make([]string, 0, len(matchedServers))
			for _, server := range matchedServers {
				serverByName[server.Name] = server
				matchedNames = append(matchedNames, server.Name)
			}
			batches := BuildRolloutBatches(policy, matchedNames)
			runByKey := make(map[string]Run, len(rolloutRuns))
			for _, run := range rolloutRuns {
				runByKey[rolloutRunKey(run.PolicyID, run.ScheduledForUTC, run.ServerName)] = run
			}
			elapsedMinutes := int(slotLocal.Sub(rolloutSlot) / time.Minute)
			for batchIndex, batch := range batches {
				if elapsedMinutes < batch.ReleaseDelayMinutes {
					continue
				}
				if rolloutGateState(policy.ID, scheduledForUTC, batches[:batchIndex], runByKey) != "ready" {
					continue
				}
				for _, serverName := range batch.Servers {
					server := serverByName[serverName]
					if _, exists := runByKey[rolloutRunKey(policy.ID, scheduledForUTC, server.Name)]; exists {
						continue
					}
					candidatesByServer[server.Name] = append(candidatesByServer[server.Name], missedPolicyCandidate{
						ScheduledCandidate: ScheduledCandidate{
							Policy:          policy,
							Server:          server,
							ScheduledForUTC: scheduledForUTC,
						},
						Selected: false,
					})
				}
			}
			continue
		}

		for _, server := range matchedServers {
			_, existing := existingByServer[strings.ToLower(strings.TrimSpace(server.Name))]
			if missedReason == RunReasonMaintenance {
				if materializeSelected && !existing {
					recordSkipped(policy, server, scheduledForUTC, RunReasonMaintenance)
				}
				continue
			}
			if s.BlackoutApplies(rolloutSlot, globalBlackouts) || s.BlackoutApplies(rolloutSlot, policy.PolicyBlackouts) {
				if materializeSelected && !existing {
					recordSkipped(policy, server, scheduledForUTC, RunReasonBlackout)
				}
				continue
			}
			candidatesByServer[server.Name] = append(candidatesByServer[server.Name], missedPolicyCandidate{
				ScheduledCandidate: ScheduledCandidate{
					Policy:          policy,
					Server:          server,
					ScheduledForUTC: scheduledForUTC,
				},
				Selected: materializeSelected,
				Existing: existing,
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
			return s.ComparePolicyCandidates(candidates[i].ScheduledCandidate, candidates[j].ScheduledCandidate)
		})
		for index, candidate := range candidates {
			if !candidate.Selected || candidate.Existing {
				continue
			}
			reason := missedReason
			if index > 0 {
				reason = RunReasonSuperseded
			}
			recordSkipped(candidate.Policy, candidate.Server, candidate.ScheduledForUTC, reason)
		}
	}
	if len(queueErrs) > 0 {
		return fmt.Errorf("missed scheduled policy processing encountered %d error(s): %w", len(queueErrs), errors.Join(queueErrs...))
	}
	return nil
}

func schedulerRecoveryScopeOwned(store SchedulerWatermarkStore, policyID int64, scheduledForUTC string, existing []Run) (bool, error) {
	if store.HasRecoveryScope != nil && store.MarkRecoveryScope != nil {
		marked, err := store.HasRecoveryScope(policyID, scheduledForUTC)
		if err != nil {
			return false, fmt.Errorf("load scheduler recovery scope: %w", err)
		}
		if len(existing) > 0 {
			return marked, nil
		}
		if !marked {
			if err := store.MarkRecoveryScope(policyID, scheduledForUTC); err != nil {
				return false, fmt.Errorf("mark scheduler recovery scope: %w", err)
			}
		}
		return true, nil
	}
	if len(existing) == 0 {
		return true, nil
	}
	return rolloutHistoryIsRecoveryOnly(existing), nil
}

func rolloutHistoryIsRecoveryOnly(runs []Run) bool {
	if len(runs) == 0 {
		return false
	}
	for _, run := range runs {
		// This is a compatibility fallback for tests/custom stores that do not
		// persist recovery-scope provenance. Production recovery uses the durable
		// scope marker, which safely covers scheduler_missed, blackout and
		// superseded rows from an interrupted recovery attempt without confusing
		// genuine maintenance or normal scheduler history.
		if run.Status != RunSkipped || run.Reason != RunReasonSchedulerMissed {
			return false
		}
	}
	return true
}
