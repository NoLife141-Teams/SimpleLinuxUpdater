package main

import (
	"database/sql"
	"log"
	"time"

	auditpkg "debian-updater/internal/audit"
	notificationpkg "debian-updater/internal/notifications"
)

type AuditEvent = auditpkg.Event
type AuditService = auditpkg.Service
type AuditListFilter = auditpkg.ListFilter
type AuditListResult = auditpkg.ListResult
type AuditListError = auditpkg.ListError

type auditDBProvider func() *sql.DB

type auditNotifier func(string)

type auditTimezoneProvider func() (*time.Location, string)

func NewAuditService(db auditDBProvider, notify auditNotifier, timezone auditTimezoneProvider) *AuditService {
	return NewAuditServiceWithNotifications(db, notify, timezone, nil)
}

func NewAuditServiceWithNotifications(db auditDBProvider, notify auditNotifier, timezone auditTimezoneProvider, notifications *NotificationService) *AuditService {
	if db == nil {
		db = getDB
	}
	if timezone == nil {
		timezone = currentAppTimezone
	}
	var notifier auditpkg.Notifier
	if notify != nil {
		notifier = func(reason string) { notify(reason) }
	}
	var onRecord auditpkg.RecordCallback
	if notifications != nil {
		onRecord = func(evt auditpkg.Event) {
			notifications.NotifyAuditEvent(notificationpkg.AuditEvent{
				CreatedAt:  evt.CreatedAt,
				Actor:      evt.Actor,
				Action:     evt.Action,
				TargetType: evt.TargetType,
				TargetName: evt.TargetName,
				Status:     evt.Status,
				Message:    evt.Message,
				MetaJSON:   evt.MetaJSON,
				ClientIP:   evt.ClientIP,
			})
		}
	}
	return auditpkg.NewService(auditpkg.ServiceOptions{
		DB:            func() *sql.DB { return db() },
		Notify:        notifier,
		OnRecord:      onRecord,
		Timezone:      func() (*time.Location, string) { return timezone() },
		FormatDisplay: formatTimestampForAppDisplayWithTimezone,
		PruneGuard:    auditPruneGuard,
	})
}

func defaultAuditService() *AuditService {
	return NewAuditService(getDB, notifyDashboardEvent, currentAppTimezone)
}

func auditPruneGuard(prune func() error) error {
	// Check maintenance before taking backupRestoreMu to avoid unnecessary lock
	// contention, then re-check after backupRestoreMu.RLock() because maintenance
	// can become active in the gap between the first currentMaintenanceState()
	// read and acquiring backupRestoreMu.
	if currentMaintenanceState().Active {
		return nil
	}
	backupRestoreMu.RLock()
	defer backupRestoreMu.RUnlock()
	if currentMaintenanceState().Active {
		return nil
	}
	return prune()
}

func sanitizeAuditMeta(meta map[string]any) string {
	return auditpkg.SanitizeMeta(meta)
}

func writeAuditEvent(evt AuditEvent) error {
	return defaultAuditService().Write(evt)
}

func auditWithActor(actor, clientIP, action, targetType, targetName, status, message string, meta map[string]any) {
	if err := defaultAuditService().Record(actor, clientIP, action, targetType, targetName, status, message, meta); err != nil {
		log.Printf("audit write failed: action=%s target=%s err=%v", action, targetName, err)
	}
}

func pruneAuditEvents(retentionDays int) error {
	return defaultAuditService().Prune(retentionDays)
}
