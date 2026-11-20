package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	notificationpkg "debian-updater/internal/notifications"
)

const (
	backupStorageStaleAfter  = 2 * backupArchiveObservationInterval
	backupStorageAlertAfter  = 30 * time.Minute
	backupStorageConditionID = "backup/storage/unavailable"
)

type backupStorageHistory struct {
	Binding         string `json:"binding,omitempty"`
	LastAvailableAt string `json:"last_available_at,omitempty"`
	UnavailableAt   string `json:"unavailable_at,omitempty"`
}

type backupStorageHealth struct {
	State           string `json:"state"`
	LastAvailableAt string `json:"last_available_at,omitempty"`
	UnavailableAt   string `json:"unavailable_at,omitempty"`
	AlertDue        bool   `json:"alert_due"`
}

// The configured path remains stable while a mount disappears. Its binding
// stays in local persistence and is never projected to diagnostics.
func backupStorageBinding(directory string) string {
	if directory == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(directory))))
}

func (a *backupAutomation) recordArchiveObservation(state *backupAutomationState, observation backupArchiveObservation) {
	observation.recordStorageHistory(state.Observation, backupStorageBinding(a.deployment.Directory))
	state.Observation = observation
}

func (o *backupArchiveObservation) recordStorageHistory(previous backupArchiveObservation, binding string) {
	at := parseBackupStamp(o.CheckedAt)
	history, _ := previous.storageHistoryAsOf(binding, at)
	history.Binding = binding
	if o.Storage == "available" {
		history.LastAvailableAt = o.CheckedAt
		history.UnavailableAt = ""
	} else if history.UnavailableAt == "" {
		history.UnavailableAt = o.CheckedAt
	}
	o.StorageHistory = history
}

func (o backupArchiveObservation) storageHistoryAsOf(binding string, now time.Time) (backupStorageHistory, time.Time) {
	checked := parseBackupStamp(o.CheckedAt)
	if o.StorageHistory.Binding != binding || checked.IsZero() || checked.After(now) {
		return backupStorageHistory{}, time.Time{}
	}
	history := o.StorageHistory
	if history.UnavailableAt == "" && now.Sub(checked) >= backupStorageStaleAfter {
		history.UnavailableAt = backupStamp(checked.Add(backupStorageStaleAfter))
	}
	return history, checked
}

func (o backupArchiveObservation) storageHealth(binding string, now time.Time) *backupStorageHealth {
	if binding == "" {
		return nil
	}
	history, checked := o.storageHistoryAsOf(binding, now)
	health := &backupStorageHealth{State: "not_observed", LastAvailableAt: history.LastAvailableAt}
	// The observer and notification worker start independently. Evidence from
	// before this process preserves history but cannot confirm a current outage.
	if checked.IsZero() || o.Process != backupArchiveObservationProcess {
		return health
	}
	health.UnavailableAt = history.UnavailableAt
	switch {
	case now.Sub(checked) >= backupStorageStaleAfter:
		health.State = "stale"
	case o.Storage != "available":
		health.State = "unavailable"
	default:
		health.State = "available"
	}
	start := parseBackupStamp(health.UnavailableAt)
	health.AlertDue = !start.IsZero() && now.Sub(start) >= backupStorageAlertAfter
	return health
}

func backupStorageCondition(binding string, observation backupArchiveObservation, now time.Time, previous []notificationpkg.Condition, readErr error) []notificationpkg.ConditionFact {
	fact := notificationpkg.ConditionFact{
		ID: backupStorageConditionID, EventType: notificationpkg.EventBackupStorageUnavailable,
		TargetType: "backup", TargetName: "Application backup",
		Message: "Backup storage has been unobservable for at least 30 minutes. Review Installation Health and the backup recovery guide.",
	}
	if binding == "" {
		return []notificationpkg.ConditionFact{fact}
	}
	health := observation.storageHealth(binding, now)
	if readErr != nil || health.State == "not_observed" {
		for _, condition := range previous {
			if condition.Fact.ID == fact.ID && condition.Fact.Active &&
				strings.HasPrefix(condition.Fact.Generation, binding+"/") {
				suspended := condition.Fact
				suspended.Suspended = true
				return []notificationpkg.ConditionFact{suspended}
			}
		}
		return []notificationpkg.ConditionFact{fact}
	}
	if health.AlertDue {
		fact.Active = true
		fact.Generation = binding + "/" + health.UnavailableAt
		fact.CreatedAt = health.UnavailableAt
	}
	return []notificationpkg.ConditionFact{fact}
}
