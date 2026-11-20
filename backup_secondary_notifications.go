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
	StartedAt        string `json:"started_at,omitempty"`
	CheckedAt        string `json:"checked_at,omitempty"`
	FirstAvailableAt string `json:"first_available_at,omitempty"`
	LastAvailableAt  string `json:"last_available_at,omitempty"`
	UnavailableSince string `json:"unavailable_since,omitempty"`
}

func (o *backupSecondaryObservation) start(now time.Time) bool {
	if o.Process == backupArchiveObservationProcess {
		return false
	}
	o.Process = backupArchiveObservationProcess
	o.StartedAt = backupStamp(now)
	o.CheckedAt = ""
	return true
}

func (o *backupSecondaryObservation) record(now time.Time, available bool) {
	if !available {
		since := o.unobservableSince(now)
		if since.IsZero() {
			// A new process can confirm a known outage before its first probe is overdue.
			since = parseBackupStamp(o.UnavailableSince)
		}
		if since.IsZero() || since.After(now) {
			since = now
		}
		o.UnavailableSince = backupStamp(since)
	}
	o.Process = backupArchiveObservationProcess
	o.CheckedAt = backupStamp(now)
	if available {
		o.UnavailableSince = ""
		o.LastAvailableAt = o.CheckedAt
		if first := parseBackupStamp(o.FirstAvailableAt); first.IsZero() || first.After(now) {
			o.FirstAvailableAt = o.CheckedAt
		}
	}
}

// A filesystem call can outlive the worker's context. Until the worker
// responds, treat overdue checks as unobservable storage, without making
// claims about individual archives or starting another filesystem probe.
func (o backupSecondaryObservation) unobservableSince(now time.Time) time.Time {
	checked := parseBackupStamp(o.CheckedAt)
	if checked.IsZero() {
		started := parseBackupStamp(o.StartedAt)
		if started.IsZero() || now.Before(started.Add(2*backupSecondaryPollInterval)) {
			return time.Time{}
		}
		checked = started.Add(2 * backupSecondaryPollInterval)
	} else if checked.After(now) {
		return time.Time{}
	} else if o.UnavailableSince == "" {
		checked = checked.Add(backupSecondaryObservationMaxAge)
	}
	if since := parseBackupStamp(o.UnavailableSince); !since.IsZero() && !since.After(now) {
		return since
	}
	if checked.After(now) {
		return time.Time{}
	}
	return checked
}

func secondaryBackupConditions(pathBinding string, state backupSecondaryState, now time.Time, previous []notificationpkg.Condition, readErr error) []notificationpkg.ConditionFact {
	const target = "Off-host backup"
	facts := []notificationpkg.ConditionFact{
		{
			ID: "backup/secondary/storage", EventType: notificationpkg.EventBackupSecondaryUnavailable,
			TargetType: "backup", TargetName: target,
			Message: "Off-host backup storage has been unobservable for at least 30 minutes. Check the secondary mount and its approval in Admin.",
		},
		{
			ID: "backup/secondary/coverage", EventType: notificationpkg.EventBackupSecondaryCoverage,
			TargetType: "backup", TargetName: target,
			Message: "No recent verified off-host recovery point is available. Review secondary copy integrity and transfer retries in Admin.",
		},
	}
	if pathBinding == "" {
		return facts
	}

	generation := pathBinding
	if readErr == nil && state.Binding.Path == pathBinding {
		generation = fmt.Sprintf("%s/%x", pathBinding, sha256.Sum256([]byte(state.Binding.Directory+"\x00"+state.Binding.Owner)))
	}
	since := state.Observation.unobservableSince(now)
	checked := parseBackupStamp(state.Observation.CheckedAt)
	if readErr != nil || !state.Approved || state.Binding.Path != pathBinding || state.Observation.Process != backupArchiveObservationProcess ||
		(since.IsZero() && (checked.IsZero() || checked.After(now))) {
		for i := range facts {
			facts[i] = suspendSecondaryCondition(facts[i], generation, previous)
		}
		return facts
	}

	if !since.IsZero() {
		outageGeneration := generation + "/" + state.Observation.LastAvailableAt
		if now.Sub(since) >= backupStorageAlertAfter {
			facts[0].Active = true
			facts[0].Generation = outageGeneration
			facts[0].CreatedAt = backupStamp(since)
		} else {
			facts[0] = suspendSecondaryCondition(facts[0], outageGeneration, previous)
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
