package main

import (
	"database/sql"
	"log"
	"strings"
	"time"

	apptimepkg "debian-updater/internal/apptime"
	auditpkg "debian-updater/internal/audit"
	maintenancepkg "debian-updater/internal/maintenance"
	notificationpkg "debian-updater/internal/notifications"
	updatespkg "debian-updater/internal/updates"
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
	return newAuditServiceWithNotificationsAndClock(db, notify, timezone, notifications, nil)
}

func newAuditServiceWithNotificationsAndClock(db auditDBProvider, notify auditNotifier, timezone auditTimezoneProvider, notifications *NotificationService, now func() time.Time, coordinators ...*maintenancepkg.Coordinator) *AuditService {
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
	onRecord := func(evt auditpkg.Event) {
		recordHealthSnapshotFromAuditEvent(db, evt)
		if notifications != nil {
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
	pruneGuard := func(prune func() error) error { return prune() }
	if len(coordinators) > 0 && coordinators[0] != nil {
		coordinator := coordinators[0]
		pruneGuard = func(prune func() error) error {
			lease, decision := coordinator.TryShared(maintenancepkg.WorkAudit)
			if !decision.Allowed {
				return nil
			}
			defer lease.Close()
			return prune()
		}
	}
	opts := auditpkg.ServiceOptions{
		DB:       func() *sql.DB { return db() },
		Notify:   notifier,
		OnRecord: onRecord,
		Timezone: func() (*time.Location, string) { return timezone() },
		FormatDisplay: func(raw string, loc *time.Location, name string) (string, string) {
			return (apptimepkg.Interpretation{Location: loc, DisplayName: name}).Format(raw, jobTimestampLayout)
		},
		PruneGuard: pruneGuard,
	}
	if now != nil {
		opts.Now = now
	}
	return auditpkg.NewService(opts)
}

func recordHealthSnapshotFromAuditEvent(db auditDBProvider, evt auditpkg.Event) {
	completion, ok := maintenanceCompletionFromAuditEvent(evt)
	if db == nil || !ok {
		return
	}
	repo := updatespkg.SQLiteServerFactsRepository{DB: db}
	if err := repo.CaptureCompletion(completion); err != nil {
		log.Printf("health snapshot write failed: action=%s target=%s err=%v", evt.Action, evt.TargetName, err)
	}
}

func maintenanceCompletionFromAuditEvent(evt auditpkg.Event) (updatespkg.MaintenanceCompletion, bool) {
	if evt.TargetType != "server" || strings.TrimSpace(evt.TargetName) == "" || strings.TrimSpace(evt.TargetName) == "-" {
		return updatespkg.MaintenanceCompletion{}, false
	}
	var kind updatespkg.MaintenanceKind
	switch strings.TrimSpace(evt.Action) {
	case updateCompleteAction:
		kind = updatespkg.MaintenanceKindUpdate
	case "schedule.run.completed", "schedule.run.failed":
		kind = updatespkg.MaintenanceKindScheduledRun
	default:
		return updatespkg.MaintenanceCompletion{}, false
	}
	return updatespkg.MaintenanceCompletion{
		ServerName:  evt.TargetName,
		CompletedAt: evt.CreatedAt,
		Kind:        kind,
		Status:      evt.Status,
		RawJSON:     evt.MetaJSON,
	}, true
}

func defaultAuditService() *AuditService {
	return NewAuditService(getDB, notifyDashboardEvent, currentAppTimezone)
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
