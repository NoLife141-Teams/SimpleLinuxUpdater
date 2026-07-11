package main

import (
	"crypto/rand"
	"log"
	"time"

	healthpkg "debian-updater/internal/health"
	observabilitypkg "debian-updater/internal/observability"
	serverpkg "debian-updater/internal/servers"

	"github.com/alexedwards/argon2id"
)

type observabilitySummaryResponse = observabilitypkg.SummaryResponse
type dashboardHealthInfo = observabilitypkg.DashboardHealthInfo
type dashboardServerSummary = observabilitypkg.DashboardServerSummary
type dashboardSummaryResponse = observabilitypkg.DashboardSummaryResponse
type healthTrendResponse = observabilitypkg.HealthTrendResponse

type ObservabilityServiceDeps = observabilitypkg.ServiceDeps
type ObservabilityService = observabilitypkg.Service
type MetricsAccessCredentialDeps = observabilitypkg.MetricsAccessCredentialDeps
type MetricsAccessCredential = observabilitypkg.MetricsAccessCredential

func NewObservabilityService(deps ObservabilityServiceDeps) *ObservabilityService {
	return observabilitypkg.NewService(observabilityServiceDepsWithDefaults(deps))
}

func defaultObservabilityService() *ObservabilityService {
	return NewObservabilityService(ObservabilityServiceDeps{})
}

func NewMetricsAccessCredential(deps MetricsAccessCredentialDeps) MetricsAccessCredential {
	return observabilitypkg.NewMetricsAccessCredential(metricsAccessCredentialDepsWithDefaults(deps))
}

func observabilityServiceDepsWithDefaults(deps ObservabilityServiceDeps) ObservabilityServiceDeps {
	if deps.DB == nil {
		deps.DB = getDB
	}
	if deps.DBPath == nil {
		deps.DBPath = dbPath
	}
	if deps.CurrentTimezone == nil {
		deps.CurrentTimezone = currentAppTimezone
	}
	if deps.CurrentLocation == nil {
		deps.CurrentLocation = currentAppLocation
	}
	if deps.FormatTimestamp == nil {
		deps.FormatTimestamp = formatTimestampForAppDisplayWithTimezone
	}
	if deps.ServerSnapshot == nil {
		deps.ServerSnapshot = observabilityServerSnapshot
	}
	if deps.HostHealthObservation == nil {
		deps.HostHealthObservation = healthpkg.SQLiteObservation{DB: getDB}
	}
	if deps.ProjectPolicySchedule == nil {
		deps.ProjectPolicySchedule = defaultPolicyService().ProjectSchedule
	}
	if deps.ParseAppTimestamp == nil {
		deps.ParseAppTimestamp = parseAppTimestamp
	}
	if deps.HealthStatusFromResult == nil {
		deps.HealthStatusFromResult = healthStatusFromResult
	}
	if deps.DiskFreeKBFromOutput == nil {
		deps.DiskFreeKBFromOutput = diskFreeKBFromOutput
	}
	if deps.DiskFreeTotalKBFromOutput == nil {
		deps.DiskFreeTotalKBFromOutput = diskFreeTotalKBFromOutput
	}
	if deps.RebootResultRequiresRestart == nil {
		deps.RebootResultRequiresRestart = rebootResultRequiresRestart
	}
	if deps.UpdateCompleteAction == "" {
		deps.UpdateCompleteAction = updateCompleteAction
	}
	if deps.JobTimestampLayout == "" {
		deps.JobTimestampLayout = jobTimestampLayout
	}
	if deps.MetricsCacheTTL <= 0 {
		deps.MetricsCacheTTL = observabilitypkg.DefaultMetricsCacheTTL
	}
	if deps.Logf == nil {
		deps.Logf = log.Printf
	}
	return deps
}

func metricsAccessCredentialDepsWithDefaults(deps MetricsAccessCredentialDeps) MetricsAccessCredentialDeps {
	if deps.Store == nil {
		deps.Store = observabilitypkg.SQLiteMetricsCredentialStore{
			DB:         getDB,
			SettingKey: metricsBearerTokenHashSetting,
		}
	}
	if deps.RandomRead == nil {
		deps.RandomRead = rand.Read
	}
	if deps.HashPassword == nil {
		deps.HashPassword = func(token string) (string, error) {
			return argon2id.CreateHash(token, argon2id.DefaultParams)
		}
	}
	if deps.ComparePasswordAndHash == nil {
		deps.ComparePasswordAndHash = argon2id.ComparePasswordAndHash
	}
	if deps.EntropyBytes <= 0 {
		deps.EntropyBytes = metricsBearerTokenEntropyBytes
	}
	return deps
}

func observabilityServerSnapshot() ([]Server, map[string]*ServerStatus) {
	mu.Lock()
	defer mu.Unlock()
	serversSnapshot := cloneServers(servers)
	statusByName := map[string]*ServerStatus{}
	for _, server := range serversSnapshot {
		if status := statusMap[server.Name]; status != nil {
			statusByName[server.Name] = serverpkg.CloneServerStatus(status)
		}
	}
	return serversSnapshot, statusByName
}

func buildObservabilitySummary(rawWindow string, now time.Time) (observabilitySummaryResponse, error) {
	return defaultObservabilityService().BuildSummary(rawWindow, now)
}

func updateHealthFromResults(health *dashboardHealthInfo, results []updatePrecheckResult, source, collectedAt string) {
	observabilitypkg.UpdateHealthFromResults(health, results, source, collectedAt, observabilityServiceDepsWithDefaults(ObservabilityServiceDeps{}))
}

func buildDashboardSummary(rawWindow string, now time.Time) (dashboardSummaryResponse, error) {
	return defaultObservabilityService().BuildDashboardSummary(rawWindow, now)
}
