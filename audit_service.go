package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

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
		DB:            func() *sql.DB { return db() },
		Notify:        notifier,
		OnRecord:      onRecord,
		Timezone:      func() (*time.Location, string) { return timezone() },
		FormatDisplay: formatTimestampForAppDisplayWithTimezone,
		PruneGuard:    pruneGuard,
	}
	if now != nil {
		opts.Now = now
	}
	return auditpkg.NewService(opts)
}

func auditMetaInt(meta map[string]any, keys ...string) int {
	for _, key := range keys {
		raw, ok := meta[key]
		if !ok || raw == nil {
			continue
		}
		switch v := raw.(type) {
		case float64:
			if v >= 0 {
				return int(v)
			}
		case int:
			if v >= 0 {
				return v
			}
		case string:
			parsed, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil && parsed >= 0 {
				return parsed
			}
		}
	}
	return 0
}

func auditMetaString(meta map[string]any, key string) string {
	raw, ok := meta[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	default:
		return strings.TrimSpace(fmt.Sprintf("%v", v))
	}
}

func auditMetaMap(meta map[string]any, key string) map[string]any {
	raw, ok := meta[key]
	if !ok || raw == nil {
		return nil
	}
	asMap, ok := raw.(map[string]any)
	if ok {
		return asMap
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil
	}
	return parsed
}

func auditPrecheckResults(meta map[string]any, key string) []updatePrecheckResult {
	raw, ok := meta[key]
	if !ok || raw == nil {
		return nil
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var results []updatePrecheckResult
	if err := json.Unmarshal(data, &results); err != nil {
		return nil
	}
	return results
}

func recordHealthSnapshotFromAuditEvent(db auditDBProvider, evt auditpkg.Event) {
	if db == nil || evt.TargetType != "server" || strings.TrimSpace(evt.TargetName) == "" || strings.TrimSpace(evt.TargetName) == "-" {
		return
	}
	action := strings.TrimSpace(evt.Action)
	if action != updateCompleteAction && action != "schedule.run.completed" && action != "schedule.run.failed" {
		return
	}
	meta := map[string]any{}
	if strings.TrimSpace(evt.MetaJSON) != "" {
		if err := json.Unmarshal([]byte(evt.MetaJSON), &meta); err != nil {
			meta = map[string]any{}
		}
	}
	packageCount := auditMetaInt(meta, "pending_package_count", "approved_package_count")
	securityCount := auditMetaInt(meta, "security_package_count")
	if securityCount == 0 && strings.HasPrefix(auditMetaString(meta, "approval_scope"), "security") {
		securityCount = auditMetaInt(meta, "approved_package_count")
	}
	record := updatespkg.HealthSnapshotRecord{
		ServerName:    evt.TargetName,
		CapturedAt:    evt.CreatedAt,
		Source:        "audit",
		DiskStatus:    "unknown",
		AptStatus:     "unknown",
		RawJSON:       evt.MetaJSON,
		PackageCount:  packageCount,
		SecurityCount: securityCount,
	}
	if discovery := auditMetaMap(meta, "discovery"); discovery != nil {
		if record.PackageCount == 0 {
			record.PackageCount = auditMetaInt(discovery, "pending_package_count")
		}
		if record.SecurityCount == 0 {
			record.SecurityCount = auditMetaInt(discovery, "security_package_count")
		}
	}
	switch action {
	case updateCompleteAction:
		record.LastUpdateStatus = strings.TrimSpace(evt.Status)
	case "schedule.run.completed", "schedule.run.failed":
		record.LastScanStatus = strings.TrimSpace(evt.Status)
	}
	health := dashboardHealthInfo{DiskStatus: "unknown", AptStatus: "unknown"}
	results := auditPrecheckResults(meta, "precheck_results")
	results = append(results, auditPrecheckResults(meta, "postcheck_results")...)
	updateHealthFromResults(&health, results, "audit", evt.CreatedAt)
	record.DiskStatus = health.DiskStatus
	record.DiskFreeKB = health.DiskFreeKB
	record.DiskTotalKB = health.DiskTotalKB
	record.AptStatus = health.AptStatus
	record.RebootRequired = health.RebootRequired
	if strings.TrimSpace(record.RawJSON) == "" {
		record.RawJSON = "{}"
	}
	repo := updatespkg.SQLiteServerFactsRepository{DB: db}
	if err := repo.SaveHealthSnapshot(record); err != nil {
		log.Printf("health snapshot write failed: action=%s target=%s err=%v", action, evt.TargetName, err)
	}
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
