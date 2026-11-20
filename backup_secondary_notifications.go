package main

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"time"

	"debian-updater/internal/backup"
	notificationpkg "debian-updater/internal/notifications"
)

const (
	backupSecondaryObservationMaxAge = time.Hour
	backupSecondaryCoverageGrace     = time.Hour
)

type backupSecondaryObservation struct {
	Process          string `json:"process,omitempty"`
	CheckedAt        string `json:"checked_at,omitempty"`
	FirstAvailableAt string `json:"first_available_at,omitempty"`
	UnavailableSince string `json:"unavailable_since,omitempty"`
}

func (o *backupSecondaryObservation) record(now time.Time, available bool) {
	o.Process = backupArchiveObservationProcess
	o.CheckedAt = backupStamp(now)
	if available {
		o.UnavailableSince = ""
		if first := parseBackupStamp(o.FirstAvailableAt); first.IsZero() || first.After(now) {
			o.FirstAvailableAt = o.CheckedAt
		}
	} else if since := parseBackupStamp(o.UnavailableSince); since.IsZero() || since.After(now) {
		o.UnavailableSince = o.CheckedAt
	}
}

func secondaryBackupConditions(pathBinding string, state backupSecondaryState, now time.Time, previous []notificationpkg.Condition, readErr error) []notificationpkg.ConditionFact {
	const target = "Off-host backup"
	facts := []notificationpkg.ConditionFact{
		{
			ID: "backup/secondary/storage", EventType: notificationpkg.EventBackupSecondaryUnavailable,
			TargetType: "backup", TargetName: target,
			Message: "Off-host backup storage has been unavailable for at least 30 minutes. Check the secondary mount and its approval in Admin.",
		},
		{
			ID: "backup/secondary/coverage", EventType: notificationpkg.EventBackupSecondaryCoverage,
			TargetType: "backup", TargetName: target,
			Message: "No recent verified off-host recovery point is available. Review secondary copy integrity and transfer retries in Admin.",
		},
	}
	if pathBinding == "" || (readErr == nil && state.Binding.Path != pathBinding) {
		return facts
	}

	generation := fmt.Sprintf("%s/%x", pathBinding, sha256.Sum256([]byte(state.Binding.Directory+"\x00"+state.Binding.Owner)))
	if readErr != nil {
		generation = pathBinding
	}
	checked := parseBackupStamp(state.Observation.CheckedAt)
	observed := readErr == nil && state.Approved && state.Observation.Process == backupArchiveObservationProcess &&
		!checked.IsZero() && !checked.After(now) && now.Sub(checked) < backupSecondaryObservationMaxAge
	if !observed {
		for i := range facts {
			facts[i] = suspendSecondaryCondition(facts[i], generation, previous)
		}
		return facts
	}

	if since := parseBackupStamp(state.Observation.UnavailableSince); !since.IsZero() {
		if now.Sub(since) >= backupStorageAlertAfter {
			facts[0].Active = true
			facts[0].Generation = generation + "/" + state.Observation.UnavailableSince
			facts[0].CreatedAt = state.Observation.UnavailableSince
		}
		facts[1] = suspendSecondaryCondition(facts[1], generation, previous)
		return facts
	}

	var newest time.Time
	for _, copy := range state.Copies {
		captured := parseBackupStamp(copy.Archive.CapturedAt)
		if captured.After(newest) && !captured.After(now) && secondaryIntegrityFresh(copy, now) {
			newest = captured
		}
	}
	facts[1].Generation = generation
	if newest.IsZero() {
		firstAvailable := parseBackupStamp(state.Observation.FirstAvailableAt)
		facts[1].Active = (!firstAvailable.IsZero() && now.Sub(firstAvailable) >= backupSecondaryCoverageGrace) ||
			suspendSecondaryCondition(facts[1], generation, previous).Active
	} else {
		facts[1].Active = now.Sub(newest) > backup.DefaultRecoveryStaleAfter
	}
	return facts
}

func suspendSecondaryCondition(fact notificationpkg.ConditionFact, generation string, previous []notificationpkg.Condition) notificationpkg.ConditionFact {
	for _, condition := range previous {
		current := condition.Fact
		if current.ID == fact.ID && current.Active &&
			(current.Generation == generation || strings.HasPrefix(current.Generation, generation+"/")) {
			current.Suspended = true
			return current
		}
	}
	return fact
}
