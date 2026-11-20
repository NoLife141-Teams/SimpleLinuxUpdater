package main

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
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
	history := backupStorageHistory{Binding: binding}
	if earlier := previous.storageHealth(binding, at); earlier != nil {
		history.LastAvailableAt = earlier.LastAvailableAt
		history.UnavailableAt = earlier.UnavailableAt
	}
	if o.Storage == "available" {
		history.LastAvailableAt = o.CheckedAt
		history.UnavailableAt = ""
	} else if history.UnavailableAt == "" {
		history.UnavailableAt = o.CheckedAt
	}
	o.StorageHistory = history
}

func (o backupArchiveObservation) storageHealth(binding string, now time.Time) *backupStorageHealth {
	if binding == "" {
		return nil
	}
	health := &backupStorageHealth{State: "not_observed"}
	checked := parseBackupStamp(o.CheckedAt)
	if o.StorageHistory.Binding != binding || checked.IsZero() || checked.After(now) {
		return health
	}
	health.LastAvailableAt = o.StorageHistory.LastAvailableAt
	health.UnavailableAt = o.StorageHistory.UnavailableAt
	switch {
	case now.Sub(checked) >= backupStorageStaleAfter:
		health.State = "stale"
		if health.UnavailableAt == "" {
			health.UnavailableAt = backupStamp(checked.Add(backupStorageStaleAfter))
		}
	case o.Storage != "available":
		health.State = "unavailable"
	case o.Process == backupArchiveObservationProcess:
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
	if readErr != nil {
		for _, condition := range previous {
			if condition.Fact.ID == fact.ID && condition.Fact.Active {
				suspended := condition.Fact
				suspended.Suspended = true
				return []notificationpkg.ConditionFact{suspended}
			}
		}
		return nil
	}
	if health := observation.storageHealth(binding, now); health.AlertDue {
		fact.Active = true
		fact.Generation = binding + "/" + health.UnavailableAt
		fact.CreatedAt = health.UnavailableAt
	}
	return []notificationpkg.ConditionFact{fact}
}
