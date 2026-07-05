package main

import (
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	policypkg "debian-updater/internal/policies"
	serverpkg "debian-updater/internal/servers"

	"github.com/gin-gonic/gin"
)

const (
	updatePolicyExecutionScanOnly         = policypkg.ExecutionScanOnly
	updatePolicyExecutionApprovalRequired = policypkg.ExecutionApprovalRequired
	updatePolicyExecutionAutoApply        = policypkg.ExecutionAutoApply

	updatePolicyPackageScopeSecurity = policypkg.PackageScopeSecurity
	updatePolicyPackageScopeFull     = policypkg.PackageScopeFull
	updatePolicyUpgradeModeStandard  = policypkg.UpgradeModeStandard
	updatePolicyUpgradeModeFull      = policypkg.UpgradeModeFull

	updatePolicyCadenceDaily  = policypkg.CadenceDaily
	updatePolicyCadenceWeekly = policypkg.CadenceWeekly

	updatePolicyRunQueued          = policypkg.RunQueued
	updatePolicyRunRunning         = policypkg.RunRunning
	updatePolicyRunWaitingApproval = policypkg.RunWaitingApproval
	updatePolicyRunSucceeded       = policypkg.RunSucceeded
	updatePolicyRunFailed          = policypkg.RunFailed
	updatePolicyRunSkipped         = policypkg.RunSkipped
	updatePolicyRunCancelled       = policypkg.RunCancelled
	updatePolicyRunInterrupted     = policypkg.RunInterrupted

	updatePolicyRunReasonBlackout    = policypkg.RunReasonBlackout
	updatePolicyRunReasonBusy        = policypkg.RunReasonBusy
	updatePolicyRunReasonSuperseded  = policypkg.RunReasonSuperseded
	updatePolicyRunReasonRestart     = policypkg.RunReasonRestart
	updatePolicyRunReasonNoMatch     = policypkg.RunReasonNoMatch
	updatePolicyRunReasonMissing     = policypkg.RunReasonMissing
	updatePolicyRunReasonMaintenance = policypkg.RunReasonMaintenance
	updatePolicyRunReasonPersistence = policypkg.RunReasonPersistence

	updatePolicyGlobalBlackoutsSetting     = policypkg.GlobalBlackoutsSetting
	defaultScheduledApprovalTimeoutMinutes = policypkg.DefaultApprovalTimeoutMinutes
	defaultUpdatePolicyRunsLimit           = policypkg.DefaultRunsLimit
	maxUpdatePolicyRunsLimit               = policypkg.MaxRunsLimit
	updatePolicyTickInterval               = policypkg.DefaultSchedulerTickInterval
)

var errUpdatePolicyValidation = errors.New("update policy validation")

func wrapUpdatePolicyValidationError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", errUpdatePolicyValidation, err)
}

func isUpdatePolicyValidationError(err error) bool {
	return errors.Is(err, errUpdatePolicyValidation)
}

type UpdatePolicyBlackoutWindow = policypkg.BlackoutWindow
type UpdatePolicy = policypkg.Policy
type UpdatePolicyOverride = policypkg.Override
type UpdatePolicyRun = policypkg.Run
type UpdatePolicySettingsResponse = policypkg.SettingsResponse
type UpdatePolicyPreviewServer = policypkg.PreviewServer
type UpdatePolicyPreviewResponse = policypkg.PreviewResponse
type UpdatePolicyCalendarResponse = policypkg.CalendarResponse
type UpdatePolicyCalendarPolicy = policypkg.CalendarPolicy
type updatePolicyRunUpdate = policypkg.RunUpdate

func defaultPolicyRepository() *policypkg.SQLiteRepository {
	return policypkg.NewSQLiteRepository(policypkg.SQLiteRepositoryDeps{
		DB:          getDB,
		NowString:   jobTimestampNow,
		MarshalJSON: marshalJobJSON,
	})
}

func ensureUpdatePolicySchema(db *sql.DB) error {
	return policypkg.EnsureSchema(db)
}

func getSettingValue(key string) (string, error) {
	var value string
	err := getDB().QueryRow("SELECT value FROM settings WHERE key = ?", strings.TrimSpace(key)).Scan(&value)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return value, err
}

func upsertSettingValue(key, value string) error {
	_, err := getDB().Exec(
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
		strings.TrimSpace(key),
		value,
	)
	return err
}

func loadGlobalUpdatePolicyBlackouts() ([]UpdatePolicyBlackoutWindow, error) {
	return defaultPolicyRepository().LoadGlobalBlackouts()
}

func saveGlobalUpdatePolicyBlackoutsWithRepository(repo policypkg.Repository, windows []UpdatePolicyBlackoutWindow) ([]UpdatePolicyBlackoutWindow, error) {
	if repo == nil {
		repo = defaultPolicyRepository()
	}
	normalized, err := policypkg.NormalizeBlackouts(windows)
	if err != nil {
		return nil, wrapUpdatePolicyValidationError(err)
	}
	return repo.SaveGlobalBlackouts(normalized)
}

func parseTimeLocalMinutes(raw string) (int, error) {
	return policypkg.ParseTimeLocalMinutes(raw)
}

func listUpdatePolicies() ([]UpdatePolicy, error) {
	return defaultPolicyRepository().ListPolicies()
}

func getUpdatePolicy(id int64) (UpdatePolicy, error) {
	return defaultPolicyRepository().GetPolicy(id)
}

func createUpdatePolicy(policy UpdatePolicy) (UpdatePolicy, error) {
	return createUpdatePolicyWithRepository(defaultPolicyService(), defaultPolicyRepository(), policy)
}

func createUpdatePolicyWithRepository(service *PolicyService, repo policypkg.Repository, policy UpdatePolicy) (UpdatePolicy, error) {
	if service == nil {
		service = defaultPolicyService()
	}
	if repo == nil {
		repo = defaultPolicyRepository()
	}
	if err := service.NormalizePolicy(&policy); err != nil {
		return UpdatePolicy{}, wrapUpdatePolicyValidationError(err)
	}
	return repo.CreatePolicy(policy)
}

func updateUpdatePolicyWithRepository(service *PolicyService, repo policypkg.Repository, id int64, policy UpdatePolicy) (UpdatePolicy, error) {
	if id <= 0 {
		return UpdatePolicy{}, sql.ErrNoRows
	}
	policy.ID = id
	if service == nil {
		service = defaultPolicyService()
	}
	if repo == nil {
		repo = defaultPolicyRepository()
	}
	if err := service.NormalizePolicy(&policy); err != nil {
		return UpdatePolicy{}, wrapUpdatePolicyValidationError(err)
	}
	return repo.UpdatePolicy(id, policy)
}

func listUpdatePolicyOverrides(policyID int64) ([]UpdatePolicyOverride, error) {
	return defaultPolicyRepository().ListOverrides(policyID)
}

func loadAllUpdatePolicyOverrides() (map[int64]map[string]bool, error) {
	return defaultPolicyRepository().LoadAllOverrides()
}

func setUpdatePolicyOverride(policyID int64, serverName string, disabled bool) (UpdatePolicyOverride, error) {
	return defaultPolicyRepository().SetOverride(policyID, serverName, disabled)
}

func renameUpdatePolicyOverridesServerTx(tx *sql.Tx, oldServerName, newServerName string) error {
	return defaultPolicyRepository().RenameOverridesServerTx(tx, oldServerName, newServerName)
}

func renameUpdatePolicyTargetServersTx(tx *sql.Tx, oldServerName, newServerName string) error {
	return defaultPolicyRepository().RenameTargetServersTx(tx, oldServerName, newServerName)
}

func pruneUpdatePolicyOverridesForServersTx(tx *sql.Tx, activeServers []Server) error {
	return defaultPolicyRepository().PruneOverridesForServersTx(tx, activeServers)
}

func createUpdatePolicyRun(run UpdatePolicyRun) (UpdatePolicyRun, bool, error) {
	return defaultPolicyRepository().CreateRun(run)
}

func getUpdatePolicyRun(id int64) (UpdatePolicyRun, error) {
	return defaultPolicyRepository().GetRun(id)
}

func updateUpdatePolicyRun(id int64, update updatePolicyRunUpdate) error {
	return defaultPolicyRepository().UpdateRun(id, update)
}

func listUpdatePolicyRuns(limit int) ([]UpdatePolicyRun, error) {
	return defaultPolicyRepository().ListRuns(limit)
}

func markInterruptedUpdatePolicyRuns() error {
	return defaultPolicyRepository().MarkInterruptedRuns()
}

func snapshotServers() []Server {
	mu.Lock()
	defer mu.Unlock()
	return cloneServers(servers)
}

func serverExistsByName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, server := range snapshotServers() {
		if server.Name == name {
			return true
		}
	}
	return false
}

func serverExistsByNameInState(state *serverpkg.State, name string) bool {
	name = strings.TrimSpace(name)
	if state == nil {
		return serverExistsByName(name)
	}
	for _, server := range state.CloneServers() {
		if server.Name == name {
			return true
		}
	}
	return false
}

func policyMatchesServer(policy UpdatePolicy, server Server, overrides map[int64]map[string]bool) bool {
	return defaultPolicyService().PolicyMatchesServer(policy, server, PolicyMatchContext{Overrides: overrides})
}

func enrichPoliciesWithMatchesUsing(service *PolicyService, policies []UpdatePolicy) []UpdatePolicy {
	if service == nil {
		service = defaultPolicyService()
	}
	return service.EnrichPoliciesWithMatches(policies)
}

func policyDueAt(policy UpdatePolicy, slotLocal time.Time) bool {
	return defaultPolicyService().PolicyDueAt(policy, slotLocal)
}

func canonicalScheduledForUTC(slotLocal time.Time) string {
	return policypkg.CanonicalScheduledForUTC(slotLocal, jobTimestampLayout, currentAppLocation)
}

func blackoutApplies(slotLocal time.Time, windows []UpdatePolicyBlackoutWindow) bool {
	return defaultPolicyService().BlackoutApplies(slotLocal, windows)
}

func buildScheduledJobMeta(policy UpdatePolicy, scheduledForUTC string) scheduledJobMeta {
	return newScheduledRunLifecycle(globalRuntimeAppDeps()).buildScheduledJobMeta(policy, scheduledForUTC)
}

func createServerActionJobWithMetaAndState(jm *JobManager, state *serverpkg.State, kind, serverName, actor, clientIP string, policy RetryPolicy, meta any) (JobRecord, error) {
	if jm == nil {
		return JobRecord{}, errors.New("job manager is not initialized")
	}
	var snapshot *ServerStatus
	if state != nil {
		snapshot = state.CurrentStatusSnapshot(serverName)
	} else {
		snapshot = currentStatusSnapshot(serverName)
	}
	initialLogs := ""
	if snapshot != nil {
		initialLogs = snapshot.Logs
	}
	return jm.CreateJob(JobCreateParams{
		Kind:            kind,
		ServerName:      serverName,
		Actor:           actor,
		ClientIP:        clientIP,
		Status:          jobStatusQueued,
		LogsText:        initialLogs,
		RetryPolicyJSON: marshalJobJSON(policy),
		MetaJSON:        marshalJobJSON(meta),
	})
}

func updatePolicyRunFromJobRecordWithRepository(repo policypkg.Repository, runID int64, job JobRecord) {
	newScheduledRunLifecycle(AppDeps{PolicyRepository: repo}).updatePolicyRunFromJobRecord(runID, job)
}

func watchUpdatePolicyRunForJobWithDeps(deps AppDeps, runID int64, jobID string) {
	newScheduledRunLifecycle(deps).watchUpdatePolicyRunForJob(runID, jobID)
}

func loadScheduledJobBehavior(jobID string) scheduledJobBehavior {
	return newScheduledRunLifecycle(globalRuntimeAppDeps()).loadScheduledJobBehavior(jobID)
}

func loadScheduledJobBehaviorWithManager(current func() *JobManager, jobID string) scheduledJobBehavior {
	return newScheduledRunLifecycle(AppDeps{CurrentJobManager: current}).loadScheduledJobBehavior(jobID)
}

func updateScheduledJobDiscoveryMeta(jobID string, upgradable []string, pendingUpdates []PendingUpdate, plan UpgradePlan) {
	newScheduledRunLifecycle(globalRuntimeAppDeps()).updateScheduledJobDiscoveryMeta(jobID, upgradable, pendingUpdates, plan)
}

func updateScheduledJobDiscoveryMetaWithManager(current func() *JobManager, jobID string, upgradable []string, pendingUpdates []PendingUpdate, plan UpgradePlan) {
	newScheduledRunLifecycle(AppDeps{CurrentJobManager: current}).updateScheduledJobDiscoveryMeta(jobID, upgradable, pendingUpdates, plan)
}

func executeScheduledPolicyRun(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	newScheduledRunLifecycle(globalRuntimeAppDeps()).Execute(run, policy, server)
}

func globalRuntimeAppDeps() AppDeps {
	state := globalServerState()
	return AppDeps{
		DB:                     getDB,
		DBPath:                 dbPath,
		AuditService:           defaultAuditService(),
		BackupBarrier:          backupRestoreMu,
		ServerState:            state,
		ServerInventoryService: newServerInventoryServiceWithStateAndDB(state, getDB),
		PolicyRepository:       defaultPolicyRepository(),
		CurrentJobManager:      currentJobManager,
		UpdateService:          defaultUpdateService(),
		NotifyDashboardEvent:   notifyDashboardEvent,
		DashboardEventBroker:   dashboardEventBroker,
		CurrentMaintenanceActive: func() bool {
			return currentMaintenanceState().Active
		},
	}
}

func executeScheduledPolicyRunWithDeps(deps AppDeps, run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	newScheduledRunLifecycle(deps).Execute(run, policy, server)
}

func markScheduledPolicyRunMaintenanceSkipped(run UpdatePolicyRun, policy UpdatePolicy, server Server, summary string) {
	markScheduledPolicyRunMaintenanceSkippedWithDeps(globalRuntimeAppDeps(), run, policy, server, summary)
}

func markScheduledPolicyRunMaintenanceSkippedWithDeps(deps AppDeps, run UpdatePolicyRun, policy UpdatePolicy, server Server, summary string) {
	newScheduledRunLifecycle(deps).markMaintenanceSkipped(run, policy, server, summary)
}

func runScheduledUpdatePolicy(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	runScheduledUpdatePolicyWithDeps(globalRuntimeAppDeps(), run, policy, server)
}

func runScheduledUpdatePolicyWithDeps(deps AppDeps, run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	newScheduledRunLifecycle(deps).runUpdate(run, policy, server)
}

func runScheduledScanPolicy(run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	runScheduledScanPolicyWithDeps(globalRuntimeAppDeps(), run, policy, server)
}

func runScheduledScanPolicyWithDeps(deps AppDeps, run UpdatePolicyRun, policy UpdatePolicy, server Server) {
	newScheduledRunLifecycle(deps).runScan(run, policy, server)
}

func resetMissedUpdatePolicyTicksForTest() {
	defaultPolicyService().ResetMissedTicksForTest()
}

func processDueUpdatePolicies(now time.Time) error {
	return defaultPolicyService().ProcessDue(now)
}

func handleUpdatePoliciesListWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	policyDeps := deps.PolicyService.EnsureDeps()
	policies, err := policyDeps.ListPolicies()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load update policies"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"items":             enrichPoliciesWithMatchesUsing(deps.PolicyService, policies),
		"timezone":          deps.AppTimezoneDisplayName(),
		"resolved_timezone": deps.AppTimezoneResolvedName(),
	})
}

func handleUpdatePolicyCreateWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	var policy UpdatePolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		audit(c, "update_policy.create", "update_policy", "-", "failure", "Invalid request payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	created, err := createUpdatePolicyWithRepository(deps.PolicyService, deps.PolicyRepository, policy)
	if err != nil {
		audit(c, "update_policy.create", "update_policy", policy.Name, "failure", "Failed to create policy", map[string]any{"error": err.Error()})
		statusCode := http.StatusInternalServerError
		if isUpdatePolicyValidationError(err) {
			statusCode = http.StatusBadRequest
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}
	audit(c, "update_policy.create", "update_policy", created.Name, "success", "Update policy created", map[string]any{
		"policy_id":      created.ID,
		"execution_mode": created.ExecutionMode,
		"package_scope":  created.PackageScope,
		"upgrade_mode":   created.UpgradeMode,
		"target_tag":     created.TargetTag,
		"cadence_kind":   created.CadenceKind,
		"time_local":     created.TimeLocal,
	})
	c.JSON(http.StatusCreated, created)
}

func handleUpdatePolicyPreviewWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	var policy UpdatePolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if err := deps.PolicyService.NormalizePolicy(&policy); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	preview, err := deps.PolicyService.PreviewPolicy(policy)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to preview update policy"})
		return
	}
	c.JSON(http.StatusOK, preview)
}

func handleUpdatePolicyUpdateWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy id"})
		return
	}
	var policy UpdatePolicy
	if err := c.ShouldBindJSON(&policy); err != nil {
		audit(c, "update_policy.update", "update_policy", c.Param("id"), "failure", "Invalid request payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	updated, err := updateUpdatePolicyWithRepository(deps.PolicyService, deps.PolicyRepository, id, policy)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
			return
		}
		audit(c, "update_policy.update", "update_policy", c.Param("id"), "failure", "Failed to update policy", map[string]any{"error": err.Error()})
		statusCode := http.StatusInternalServerError
		if isUpdatePolicyValidationError(err) {
			statusCode = http.StatusBadRequest
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}
	audit(c, "update_policy.update", "update_policy", updated.Name, "success", "Update policy updated", map[string]any{"policy_id": updated.ID})
	c.JSON(http.StatusOK, updated)
}

func handleUpdatePolicyDeleteWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy id"})
		return
	}
	policy, _ := deps.PolicyRepository.GetPolicy(id)
	if err := deps.PolicyRepository.DeletePolicy(id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
			return
		}
		audit(c, "update_policy.delete", "update_policy", c.Param("id"), "failure", "Failed to delete policy", map[string]any{"error": err.Error()})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete policy"})
		return
	}
	audit(c, "update_policy.delete", "update_policy", policy.Name, "success", "Update policy deleted", map[string]any{"policy_id": id})
	c.JSON(http.StatusOK, gin.H{"message": "policy deleted"})
}

func handleUpdatePolicyRunsWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	rawLimit := strings.TrimSpace(c.DefaultQuery("limit", strconv.Itoa(defaultUpdatePolicyRunsLimit)))
	limit, err := strconv.Atoi(rawLimit)
	if err != nil || limit <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
		return
	}
	if limit > maxUpdatePolicyRunsLimit {
		limit = maxUpdatePolicyRunsLimit
	}
	runs, err := deps.PolicyRepository.ListRuns(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load policy runs"})
		return
	}
	loc, timezoneName := deps.CurrentAppTimezone()
	for i := range runs {
		runs[i].ScheduledForDisplay, _ = formatTimestampForAppDisplayWithTimezone(runs[i].ScheduledForUTC, loc, timezoneName)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":             runs,
		"timezone":          deps.AppTimezoneDisplayName(),
		"resolved_timezone": deps.AppTimezoneResolvedName(),
	})
}

func handleUpdatePolicyCalendarWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	rawDays := strings.TrimSpace(c.DefaultQuery("days", "14"))
	days, err := strconv.Atoi(rawDays)
	if err != nil || days <= 0 || days > 31 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "days must be an integer between 1 and 31"})
		return
	}
	var policyID int64
	if rawPolicyID := strings.TrimSpace(c.Query("policy_id")); rawPolicyID != "" {
		policyID, err = strconv.ParseInt(rawPolicyID, 10, 64)
		if err != nil || policyID <= 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "policy_id must be a positive integer"})
			return
		}
	}
	calendar, err := deps.PolicyService.Calendar(policypkg.CalendarOptions{
		Days:     days,
		PolicyID: policyID,
	})
	if err != nil {
		if errors.Is(err, policypkg.ErrPolicyNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load update policy calendar"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"days":              calendar.Days,
		"start_date":        calendar.StartDate,
		"end_date":          calendar.EndDate,
		"generated_at":      calendar.GeneratedAt,
		"policies":          calendar.Policies,
		"timezone":          deps.AppTimezoneDisplayName(),
		"resolved_timezone": deps.AppTimezoneResolvedName(),
	})
}

func handleUpdatePolicyOverridesWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy id"})
		return
	}
	policy, err := deps.PolicyRepository.GetPolicy(id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	items, err := deps.PolicyRepository.ListOverrides(id)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load overrides"})
		return
	}
	for i := range items {
		items[i].PolicyName = policy.Name
		items[i].TargetTag = policy.TargetTag
	}
	c.JSON(http.StatusOK, gin.H{"items": items})
}

func handleUpdatePolicyOverrideUpsertWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	id, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid policy id"})
		return
	}
	serverName := strings.TrimSpace(c.Param("server"))
	if serverName == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "server name is required"})
		return
	}
	if _, err := deps.PolicyRepository.GetPolicy(id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "policy not found"})
		return
	}
	if !serverExistsByNameInState(deps.ServerState, serverName) {
		audit(c, "update_policy.override", "server", serverName, "failure", "Server not found", map[string]any{"policy_id": id})
		c.JSON(http.StatusNotFound, gin.H{"error": "server not found"})
		return
	}
	var req struct {
		Disabled bool `json:"disabled"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		audit(c, "update_policy.override", "server", serverName, "failure", "Invalid request payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	override, err := deps.PolicyRepository.SetOverride(id, serverName, req.Disabled)
	if err != nil {
		audit(c, "update_policy.override", "server", serverName, "failure", "Failed to save override", map[string]any{"error": err.Error(), "policy_id": id})
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save override"})
		return
	}
	status := "enabled"
	if req.Disabled {
		status = "disabled"
	}
	audit(c, "update_policy.override", "server", serverName, "success", "Policy override updated", map[string]any{"policy_id": id, "override_state": status})
	c.JSON(http.StatusOK, override)
}

func handleUpdatePolicySettingsStatusWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	windows, err := deps.PolicyRepository.LoadGlobalBlackouts()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load scheduled update settings"})
		return
	}
	c.JSON(http.StatusOK, UpdatePolicySettingsResponse{
		Timezone:         deps.AppTimezoneDisplayName(),
		ResolvedTimezone: deps.AppTimezoneResolvedName(),
		GlobalBlackouts:  windows,
	})
}

func handleUpdatePolicySettingsUpdateWithDeps(c *gin.Context, deps AppDeps) {
	deps = deps.withDefaults()
	var req UpdatePolicySettingsResponse
	if err := c.ShouldBindJSON(&req); err != nil {
		audit(c, "update_policy.settings", "update_policy", "global", "failure", "Invalid request payload", nil)
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	normalizedBlackouts, err := saveGlobalUpdatePolicyBlackoutsWithRepository(deps.PolicyRepository, req.GlobalBlackouts)
	if err != nil {
		audit(c, "update_policy.settings", "update_policy", "global", "failure", "Failed to save scheduled update settings", map[string]any{"error": err.Error()})
		statusCode := http.StatusInternalServerError
		if isUpdatePolicyValidationError(err) {
			statusCode = http.StatusBadRequest
		}
		c.JSON(statusCode, gin.H{"error": err.Error()})
		return
	}
	audit(c, "update_policy.settings", "update_policy", "global", "success", "Scheduled update settings saved", map[string]any{"global_blackout_count": len(normalizedBlackouts)})
	c.JSON(http.StatusOK, UpdatePolicySettingsResponse{
		Timezone:         deps.AppTimezoneDisplayName(),
		ResolvedTimezone: deps.AppTimezoneResolvedName(),
		GlobalBlackouts:  normalizedBlackouts,
	})
}
