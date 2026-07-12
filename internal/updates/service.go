package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"debian-updater/internal/jobs"
	runtimepkg "debian-updater/internal/runtime"
	"debian-updater/internal/servers"
)

type Service struct {
	deps ServiceDeps
}

func NewService(deps ServiceDeps) *Service {
	deps = deps.withDefaults()
	return &Service{deps: deps}
}

func (s *Service) EnsureDeps() ServiceDeps {
	if s == nil {
		return ServiceDeps{}.withDefaults()
	}
	return s.deps.withDefaults()
}

func (d ServiceDeps) withDefaults() ServiceDeps {
	if d.Now == nil {
		d.Now = func() time.Time { return time.Now().UTC() }
	}
	if d.StartJobRunner == nil {
		d.StartJobRunner = func(_ string, run func()) {
			if run != nil {
				go run()
			}
		}
	}
	if d.JobTimestampNow == nil {
		d.JobTimestampNow = func() string { return jobs.FormatTimestamp(time.Now().UTC()) }
	}
	if d.LoadCommandTimeout == nil {
		d.LoadCommandTimeout = func() time.Duration { return DefaultSSHCommandTimeout }
	}
	if d.LoadPostUpdateCheckConfig == nil {
		d.LoadPostUpdateCheckConfig = func() PostUpdateCheckConfig {
			return PostUpdateCheckConfig{Enabled: true, BlockOnAptHealth: true, BlockOnFailedUnits: true, RebootRequiredWarning: true}
		}
	}
	if d.LoadScheduledJobBehavior == nil {
		d.LoadScheduledJobBehavior = func(string) ScheduledJobBehavior { return ScheduledJobBehavior{ApprovalTimeout: 30 * time.Minute} }
	}
	if d.WaitForApprovalPoll == nil {
		d.WaitForApprovalPoll = func() { time.Sleep(ApprovalPollInterval) }
	}
	if d.HostMaintenanceSessions == nil {
		d.HostMaintenanceSessions = hostMaintenanceUnavailableFactory()
	}
	if d.IsPostcheckFailureBlocking == nil {
		d.IsPostcheckFailureBlocking = func(string, PostUpdateCheckConfig) bool { return true }
	}
	if d.SummarizeUnitNames == nil {
		d.SummarizeUnitNames = SummarizeUnitNames
	}
	if d.Logf == nil {
		d.Logf = func(string, ...any) {}
	}
	return d
}

type withActorRunner struct {
	service    *Service
	server     servers.Server
	actor      string
	clientIP   string
	policy     RetryPolicy
	jobID      string
	jobKind    string
	jobPhase   string
	startedAt  time.Time
	approvedAt time.Time

	approvalScope           string
	approvalConfirmRemovals bool
	approvedPackages        []string
	upgradePlan             servers.UpgradePlan

	session HostMaintenanceSession

	commandTimeout time.Duration

	sshDialAttempts        int
	aptUpdateAttempts      int
	listUpgradableAttempts int
	aptUpgradeAttempts     int
	commandAttempts        int

	retryExhausted bool
	lastErrClass   string

	prechecksPassed bool
	precheckFailed  string
	precheckResults []PrecheckResult

	postchecksEnabled bool
	postchecksPassed  bool
	postcheckFailed   string
	postcheckWarnings int
	postcheckResults  []PrecheckResult
	upgradeCompleted  bool

	preUpdateFailedUnits []string
	retryLogFormats      map[string]string
}

func (r *withActorRunner) deps() ServiceDeps {
	if r != nil && r.service != nil {
		return r.service.EnsureDeps()
	}
	return ServiceDeps{}.withDefaults()
}

func (r *withActorRunner) currentJobManager() *jobs.Manager {
	return r.deps().CurrentJobManager()
}

func (r *withActorRunner) withStatus(update func(*servers.ServerStatus)) bool {
	deps := r.deps()
	if deps.ServerState == nil {
		return false
	}
	deps.ServerState.Lock()
	status := deps.ServerState.StatusMap()[r.server.Name]
	if status == nil {
		deps.ServerState.Unlock()
		return false
	}
	update(status)
	snapshot := servers.CloneServerStatus(status)
	deps.ServerState.Unlock()
	r.syncJobFromStatus(snapshot)
	return true
}

func (r *withActorRunner) appendStatusLog(line string) {
	_ = r.withStatus(func(status *servers.ServerStatus) {
		status.Logs += line
	})
}

func (r *withActorRunner) setErrorLogs(logs string) {
	_ = r.withStatus(func(status *servers.ServerStatus) {
		status.Status = "error"
		status.Logs = logs
	})
}

func (r *withActorRunner) currentLogs() string {
	deps := r.deps()
	if deps.ServerState == nil {
		return ""
	}
	return deps.ServerState.CurrentStatusLogs(r.server.Name)
}

func (r *withActorRunner) setJobPhase(phase string) {
	r.jobPhase = strings.TrimSpace(phase)
	if jm := r.currentJobManager(); jm != nil && strings.TrimSpace(r.jobID) != "" && r.jobPhase != "" {
		if err := jm.Transition(r.jobID, jobs.Intent{Phase: &r.jobPhase}); err != nil {
			r.deps().Logf("failed to update job %q phase to %q: %v", r.jobID, r.jobPhase, err)
		}
	}
}

func (r *withActorRunner) syncJobFromStatus(snapshot *servers.ServerStatus) {
	if snapshot == nil {
		return
	}
	jm := r.currentJobManager()
	if jm == nil || strings.TrimSpace(r.jobID) == "" {
		return
	}
	timestamp := ""
	if runtimepkg.ServerStatusFinishesJob(snapshot.Status) {
		timestamp = r.deps().JobTimestampNow()
	}
	update := runtimepkg.JobTransitionIntentFromServerStatus(snapshot.Status, runtimepkg.ServerStatusJobUpdateOptions{
		Logs:           snapshot.Logs,
		LastErrorClass: r.lastErrClass,
		CurrentPhase:   r.jobPhase,
		Timestamp:      timestamp,
	})

	if _, err := jm.TransitionActive(r.jobID, update); err != nil {
		r.deps().Logf("failed to sync job %q from status %q: %v", r.jobID, snapshot.Status, err)
	}
}

func (r *withActorRunner) markErrorClass(err error) {
	if IsRetryableError(err) {
		r.lastErrClass = "transient"
		r.retryExhausted = true
		return
	}
	r.lastErrClass = "permanent"
}

func (r *withActorRunner) setupSSH(dialOpName string) bool {
	deps := r.deps()
	if r.retryLogFormats == nil {
		r.retryLogFormats = map[string]string{}
	}
	r.retryLogFormats[dialOpName] = "\nSSH dial attempt %d/%d failed: %v; retrying in %s"
	session, err := deps.HostMaintenanceSessions.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         r.server,
		RetryPolicy:    r.policy,
		DialOperation:  dialOpName,
		CommandTimeout: r.commandTimeout,
		OnRetry:        r.onHostRetry,
	})
	if err != nil {
		var sessionErr *HostMaintenanceError
		if errors.As(err, &sessionErr) {
			r.sshDialAttempts += sessionErr.Attempts
		}
		r.markErrorClass(err)
		switch HostMaintenanceErrorStageOf(err) {
		case HostMaintenanceStageAuth:
			r.setErrorLogs(fmt.Sprintf("Auth setup failed: %v", err))
		case HostMaintenanceStageHostKey:
			r.setErrorLogs(fmt.Sprintf("Host key verification setup failed: %v", err))
		default:
			r.setErrorLogs(fmt.Sprintf("SSH connection failed: %v", err))
		}
		return false
	}
	r.session = session
	r.sshDialAttempts += session.Stats().DialAttempts
	return true
}

func (r *withActorRunner) onHostRetry(event HostRetryEvent) {
	format := r.retryLogFormats[event.Operation]
	if format == "" {
		format = "\n%s attempt %d/%d failed: %v; retrying in %s"
		r.appendStatusLog(fmt.Sprintf(format, event.Operation, event.Attempt, event.MaxAttempts, event.Err, event.Wait.Round(time.Millisecond)))
		return
	}
	r.appendStatusLog(fmt.Sprintf(format, event.Attempt, event.MaxAttempts, event.Err, event.Wait.Round(time.Millisecond)))
}

func (r *withActorRunner) closeSession() {
	if r == nil || r.session == nil {
		return
	}
	_ = r.session.Close()
	r.session = nil
}

func (s *Service) runWithActorShared(
	server servers.Server,
	actor, clientIP string,
	jobID, jobKind string,
	policy RetryPolicy,
	auditAction string,
	initStatus func(*servers.ServerStatus, RetryPolicy),
	auditMeta func(*withActorRunner, string) map[string]any,
	outcomeForStatus func(string) string,
	dialOpName string,
	runSteps func(*withActorRunner),
) {
	deps := s.EnsureDeps()
	runner := &withActorRunner{
		service:        s,
		server:         server,
		actor:          actor,
		clientIP:       clientIP,
		policy:         policy,
		jobID:          strings.TrimSpace(jobID),
		jobKind:        strings.TrimSpace(jobKind),
		commandTimeout: deps.LoadCommandTimeout(),
		lastErrClass:   "none",
		startedAt:      deps.Now(),
	}
	auditHandled := false
	if auditMeta == nil {
		auditMeta = func(*withActorRunner, string) map[string]any { return map[string]any{} }
	}
	if outcomeForStatus == nil {
		outcomeForStatus = UpdateCompletionOutcome
	}

	defer func() {
		if auditHandled {
			return
		}
		finalStatus := "unknown"
		if deps.ServerState != nil {
			if status := deps.ServerState.CurrentStatusSnapshot(server.Name); status != nil {
				finalStatus = status.Status
			}
		}
		outcome := outcomeForStatus(finalStatus)
		deps.AuditWithActor(
			actor,
			clientIP,
			auditAction,
			"server",
			server.Name,
			outcome,
			fmt.Sprintf("Final status: %s", finalStatus),
			auditMeta(runner, finalStatus),
		)
	}()

	if !runner.withStatus(func(status *servers.ServerStatus) {
		initStatus(status, policy)
	}) {
		runner.lastErrClass = "permanent"
		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(runner.jobID) != "" {
			status := jobs.StatusFailed
			phase := jobs.PhaseComplete
			summary := "Server runtime status missing"
			errorClass := "runtime_state"
			if err := jm.Transition(runner.jobID, jobs.Intent{
				Status:     &status,
				Phase:      &phase,
				Summary:    &summary,
				ErrorClass: &errorClass,
			}); err != nil {
				deps.Logf("failed to mark job %q failed after runtime status loss: %v", runner.jobID, err)
			}
		}
		auditHandled = true
		deps.AuditWithActor(
			actor,
			clientIP,
			auditAction,
			"server",
			server.Name,
			"failure",
			"Server runtime status missing",
			map[string]any{
				"job_id":   runner.jobID,
				"job_kind": runner.jobKind,
			},
		)
		return
	}

	runner.setJobPhase(jobs.PhaseDial)
	if !runner.setupSSH(dialOpName) {
		return
	}
	defer runner.closeSession()

	runSteps(runner)
}

func updateRunnerAuditMeta(r *withActorRunner, finalStatus string) map[string]any {
	approvalScope := "none"
	if !r.approvedAt.IsZero() {
		approvalScope = NormalizeApprovalScope(r.approvalScope)
	}
	meta := map[string]any{
		"status":                        finalStatus,
		"ssh_dial_attempts_used":        r.sshDialAttempts,
		"apt_update_attempts_used":      r.aptUpdateAttempts,
		"list_upgradable_attempts_used": r.listUpgradableAttempts,
		"apt_upgrade_attempts_used":     r.aptUpgradeAttempts,
		"total_attempts_used":           r.sshDialAttempts + r.aptUpdateAttempts + r.listUpgradableAttempts + r.aptUpgradeAttempts,
		"last_error_class":              r.lastErrClass,
		"retry_exhausted":               r.retryExhausted,
		"prechecks_passed":              r.prechecksPassed,
		"precheck_failed":               r.precheckFailed,
		"precheck_results":              r.precheckResults,
		"postchecks_enabled":            r.postchecksEnabled,
		"postchecks_passed":             r.postchecksPassed,
		"postcheck_failed":              r.postcheckFailed,
		"postcheck_warnings":            r.postcheckWarnings,
		"postcheck_results":             r.postcheckResults,
		"upgrade_completed":             r.upgradeCompleted,
		"pre_update_failed_units":       r.preUpdateFailedUnits,
		"approval_scope":                approvalScope,
		"approved_package_count":        len(r.approvedPackages),
		"approved_packages":             append([]string(nil), r.approvedPackages...),
		"upgrade_plan":                  servers.CloneUpgradePlan(r.upgradePlan),
	}
	if !r.startedAt.IsZero() {
		meta["total_elapsed_ms"] = r.deps().Now().Sub(r.startedAt).Milliseconds()
	}
	if !r.approvedAt.IsZero() {
		meta["execution_duration_ms"] = r.deps().Now().Sub(r.approvedAt).Milliseconds()
	}
	return meta
}

func commandRunnerAuditMeta(r *withActorRunner, finalStatus string) map[string]any {
	return map[string]any{
		"status":                 finalStatus,
		"ssh_dial_attempts_used": r.sshDialAttempts,
		"command_attempts_used":  r.commandAttempts,
		"total_attempts_used":    r.sshDialAttempts + r.commandAttempts,
		"last_error_class":       r.lastErrClass,
		"retry_exhausted":        r.retryExhausted,
	}
}

func (r *withActorRunner) refreshFactsAfterSuccessfulUpdate() {
	if r == nil || r.session == nil {
		return
	}
	deps := r.deps()
	record := r.session.CollectServerFacts(context.Background())
	if err := deps.SaveServerFacts(record); err != nil {
		deps.Logf("failed to refresh facts after update for %q: %v", r.server.Name, err)
	}
}

func (s *Service) RunUpdateJob(req UpdateRunRequest) {
	deps := s.EnsureDeps()
	postcheckCfg := deps.LoadPostUpdateCheckConfig()
	behavior := deps.LoadScheduledJobBehavior(req.JobID)
	s.runWithActorShared(
		req.Server,
		req.Actor,
		req.ClientIP,
		req.JobID,
		jobs.KindUpdate,
		req.Policy,
		UpdateCompleteAction,
		func(status *servers.ServerStatus, policy RetryPolicy) {
			status.Status = "updating"
			status.ApprovalScope = ""
			status.ApprovalConfirmRemovals = false
			status.Upgradable = nil
			status.PendingUpdates = nil
			status.UpgradePlan = servers.UpgradePlan{}
			status.Logs = fmt.Sprintf(
				"Starting Linux Updater...\nRetries enabled: max_attempts=%d base_delay=%s max_delay=%s jitter=%d%%",
				policy.MaxAttempts,
				policy.BaseDelay,
				policy.MaxDelay,
				policy.JitterPct,
			)
		},
		updateRunnerAuditMeta,
		UpdateCompletionOutcome,
		"update.ssh_dial",
		func(r *withActorRunner) {
			r.setJobPhase(jobs.PhasePrechecks)
			r.postchecksEnabled = postcheckCfg.Enabled
			r.appendStatusLog("\nRunning pre-checks...")

			precheckSummary := r.session.RunUpdatePrechecks(context.Background())
			r.precheckResults = precheckSummary.Results
			for _, result := range precheckSummary.Results {
				state := "PASS"
				if !result.Passed {
					state = "FAIL"
				}
				line := fmt.Sprintf("\nPre-check %s [%s]: %s", result.Name, state, result.Details)
				if trimmed := strings.TrimSpace(result.Output); trimmed != "" {
					line += fmt.Sprintf(" Output: %s", trimmed)
				}
				r.appendStatusLog(line)
			}
			if !precheckSummary.AllPassed {
				r.lastErrClass = "permanent"
				r.precheckFailed = precheckSummary.FailedCheck
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "error"
					status.Logs += fmt.Sprintf("\nPre-check failed (%s). Update aborted before apt update.", precheckSummary.FailedCheck)
				})
				return
			}
			r.prechecksPassed = true
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Logs += "\nPre-checks passed.\nRunning apt update..."
			})

			preUpdateFailedUnitsMap := make(map[string]struct{})
			preUpdateFailedUnits, _, preUnitsErr := r.session.ListFailedSystemdUnits(context.Background())
			if preUnitsErr != nil {
				r.appendStatusLog(fmt.Sprintf("\nBaseline failed-units snapshot unavailable: %v", preUnitsErr))
			} else {
				r.preUpdateFailedUnits = preUpdateFailedUnits
				for _, unit := range preUpdateFailedUnits {
					preUpdateFailedUnitsMap[unit] = struct{}{}
				}
				if len(preUpdateFailedUnits) > 0 {
					r.appendStatusLog(fmt.Sprintf(
						"\nDetected %d pre-existing failed systemd unit(s) before upgrade: %s.",
						len(preUpdateFailedUnits),
						deps.SummarizeUnitNames(preUpdateFailedUnits, 6),
					))
				}
			}

			r.setJobPhase(jobs.PhaseAptUpdate)
			r.retryLogFormats["update.apt_update"] = "\napt update attempt %d/%d failed: %v; retrying in %s"
			commandResult, err := r.session.RunCommand(context.Background(), HostCommandRequest{
				Operation:    "update.apt_update",
				Command:      AptUpdateCmd,
				ReplayPolicy: ReplayRetryableOutputErrors,
			})
			r.aptUpdateAttempts += commandResult.Attempts
			stdout, stderr := commandResult.Stdout, commandResult.Stderr
			logs := r.currentLogs() + "\n" + stdout + stderr
			if err != nil {
				r.markErrorClass(err)
				logs += fmt.Sprintf("\nError: %v", err)
				r.setErrorLogs(logs)
				return
			}

			r.retryLogFormats["update.list_upgradable"] = "\nlist upgradable attempt %d/%d failed: %v; retrying in %s"
			discoveryResult, err := r.session.DiscoverPackages(context.Background(), HostOperationRequest{Operation: "update.list_upgradable"})
			r.listUpgradableAttempts += discoveryResult.Attempts
			discovery := discoveryResult.Outcome
			if err != nil {
				r.markErrorClass(err)
				r.setErrorLogs(logs + fmt.Sprintf("\nError listing upgradable: %v", err))
				return
			}

			if discovery.Empty() {
				r.refreshFactsAfterSuccessfulUpdate()
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "done"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs = logs + "\nNo packages to upgrade."
				})
				return
			}

			r.upgradePlan = discovery.UpgradePlan
			deps.UpdateScheduledDiscoveryMeta(r.jobID, discovery)
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Status = "pending_approval"
				status.ApprovalScope = ""
				status.ApprovalConfirmRemovals = false
				status.Upgradable = append([]string(nil), discovery.Upgradable...)
				status.PendingUpdates = servers.ClonePendingUpdates(discovery.PendingUpdates)
				status.UpgradePlan = servers.CloneUpgradePlan(discovery.UpgradePlan)
				status.Logs = logs + "\nUpgradable packages:\n" + strings.Join(discovery.Upgradable, "\n")
			})
			autoApproval := EvaluateAutoApproval(behavior.AutoApproveScope, discovery.PendingUpdates, discovery.UpgradePlan)
			if !autoApproval.Allowed && autoApproval.RunnerCommandLog != "" {
				r.appendStatusLog(autoApproval.RunnerCommandLog)
			}
			autoApproveScope := ""
			if autoApproval.Allowed {
				autoApproveScope = autoApproval.Scope
			}
			if autoApproveScope == "" {
				s.StartPendingCVEEnrichment(r.server, discovery.PendingUpdates, r.jobID, r.actor, r.clientIP)
			}

			if autoApproveScope != "" {
				autoApproved := false
				if deps.ServerState != nil {
					deps.ServerState.Lock()
					status := deps.ServerState.StatusMap()[r.server.Name]
					if status != nil && status.Status == "pending_approval" {
						r.approvalScope = autoApproveScope
						r.approvalConfirmRemovals = false
						status.ApprovalScope = r.approvalScope
						status.ApprovalConfirmRemovals = false
						status.Status = "approved"
						r.approvedPackages = append([]string(nil), autoApproval.SelectedPackages...)
						autoApproved = true
					}
					deps.ServerState.Unlock()
				}
				if !autoApproved {
					return
				}
				r.approvedAt = deps.Now()
			} else {
				r.closeSession()
				approvalDeadline := deps.Now().Add(behavior.ApprovalTimeout)
				for {
					deps.WaitForApprovalPoll()
					approved := false
					cancelledByUser := false
					approvalTimedOut := false
					if deps.ServerState != nil {
						deps.ServerState.Lock()
						status := deps.ServerState.StatusMap()[r.server.Name]
						if status != nil {
							if status.Status == "approved" {
								r.approvalScope = NormalizeApprovalScope(status.ApprovalScope)
								r.approvalConfirmRemovals = status.ApprovalConfirmRemovals
								r.approvedPackages = PackagesForApprovalScope(r.approvalScope, status.PendingUpdates)
								approved = true
							} else if status.Status == "cancelled" {
								cancelledByUser = true
								status.Status = "idle"
								status.ApprovalScope = ""
								status.ApprovalConfirmRemovals = false
								status.Logs = ""
								status.Upgradable = nil
								status.PendingUpdates = nil
								status.UpgradePlan = servers.UpgradePlan{}
							} else if deps.Now().After(approvalDeadline) {
								approvalTimedOut = true
								status.Status = "idle"
								status.ApprovalScope = ""
								status.ApprovalConfirmRemovals = false
								status.Logs = ""
								status.Upgradable = nil
								status.PendingUpdates = nil
								status.UpgradePlan = servers.UpgradePlan{}
							}
						}
						deps.ServerState.Unlock()
					}
					if approved {
						r.approvedAt = deps.Now()
						break
					}
					if cancelledByUser {
						return
					}
					if approvalTimedOut {
						jm := deps.CurrentJobManager()
						if jm != nil && strings.TrimSpace(r.jobID) != "" {
							jobStatus := jobs.StatusCancelled
							phase := jobs.PhaseComplete
							summary := "Approval window expired"
							_ = jm.Transition(r.jobID, jobs.Intent{
								Status:  &jobStatus,
								Phase:   &phase,
								Summary: &summary,
							})
						}
						return
					}
				}
			}

			approvalRun := InterpretApprovedScope(r.approvalScope, discovery.PendingUpdates, r.upgradePlan, ApprovalScopeOptions{ConfirmRemovals: r.approvalConfirmRemovals})
			r.approvalScope = approvalRun.Scope
			r.approvedPackages = append([]string(nil), approvalRun.SelectedPackages...)

			if approvalRun.SkipUpgrade {
				if r.session == nil && !r.setupSSH("update.ssh_dial") {
					return
				}
				r.refreshFactsAfterSuccessfulUpdate()
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "done"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.Upgradable = nil
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs += approvalRun.RunnerApprovalLog
				})
				return
			}

			r.setJobPhase(jobs.PhaseAptUpgrade)
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Status = "upgrading"
				status.ApprovalScope = ""
				status.ApprovalConfirmRemovals = false
				status.Upgradable = nil
				status.PendingUpdates = nil
				status.UpgradePlan = servers.UpgradePlan{}
				status.Logs += approvalRun.RunnerApprovalLog
			})

			if !approvalRun.Allowed {
				r.lastErrClass = "permanent"
				r.setErrorLogs(r.currentLogs() + approvalRun.RunnerErrorLog)
				return
			}
			if r.session == nil && !r.setupSSH("update.ssh_dial") {
				return
			}
			upgradeCmd := approvalRun.Command
			r.appendStatusLog(approvalRun.RunnerCommandLog)
			r.retryLogFormats["update.apt_upgrade"] = "\napt upgrade attempt %d/%d failed: %v; retrying in %s"
			commandResult, err = r.session.RunCommand(context.Background(), HostCommandRequest{
				Operation:    "update.apt_upgrade",
				Command:      upgradeCmd,
				ReplayPolicy: ReplayRetryableOutputErrors,
			})
			r.aptUpgradeAttempts += commandResult.Attempts
			stdout, stderr = commandResult.Stdout, commandResult.Stderr
			logs = r.currentLogs() + "\n" + stdout + stderr
			if err != nil {
				r.markErrorClass(err)
				logs += fmt.Sprintf("\nError: %v", err)
				r.setErrorLogs(logs)
				return
			}
			r.upgradeCompleted = true

			if !postcheckCfg.Enabled {
				r.postchecksPassed = true
				r.refreshFactsAfterSuccessfulUpdate()
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "done"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs = logs + "\nUpgrade completed."
				})
				return
			}

			r.setJobPhase(jobs.PhasePostchecks)
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Status = "upgrading"
				status.Logs = logs + "\nUpgrade completed.\nRunning post-update health checks..."
			})

			inspectionSummary := r.session.RunPostUpdateHealthChecks(context.Background(), postcheckCfg, preUpdateFailedUnitsMap)
			postcheckSummary := applyPostcheckPolicy(inspectionSummary.Results, postcheckCfg, deps.IsPostcheckFailureBlocking)
			r.postcheckResults = postcheckSummary.Results
			r.postcheckWarnings = postcheckSummary.Warnings
			for _, result := range postcheckSummary.Results {
				state := "PASS"
				if !result.Passed {
					if deps.IsPostcheckFailureBlocking(result.Name, postcheckCfg) {
						state = "FAIL"
					} else {
						state = "WARN"
					}
				}
				line := fmt.Sprintf("\nPost-check %s [%s]: %s", result.Name, state, result.Details)
				if trimmed := strings.TrimSpace(result.Output); trimmed != "" {
					line += fmt.Sprintf("\nOutput:\n%s", trimmed)
				}
				r.appendStatusLog(line)
			}
			if !postcheckSummary.AllPassed {
				r.lastErrClass = "permanent"
				r.postcheckFailed = postcheckSummary.FailedCheck
				r.postchecksPassed = false
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "error"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs += fmt.Sprintf("\nUpgrade completed but post-check failed (%s).", postcheckSummary.FailedCheck)
				})
				return
			}

			r.postchecksPassed = true
			finalLogs := r.currentLogs()
			if postcheckSummary.Warnings > 0 {
				r.refreshFactsAfterSuccessfulUpdate()
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "done"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs = finalLogs + fmt.Sprintf("\nUpgrade completed with %d post-check warning(s).", postcheckSummary.Warnings)
				})
				return
			}

			r.refreshFactsAfterSuccessfulUpdate()
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Status = "done"
				status.ApprovalScope = ""
				status.ApprovalConfirmRemovals = false
				status.PendingUpdates = nil
				status.UpgradePlan = servers.UpgradePlan{}
				status.Logs = finalLogs + "\nUpgrade completed.\nPost-update health checks passed."
			})
		},
	)
}

func applyPostcheckPolicy(results []PrecheckResult, cfg PostUpdateCheckConfig, isBlocking func(string, PostUpdateCheckConfig) bool) PostcheckSummary {
	summary := PostcheckSummary{AllPassed: true, Results: results}
	for _, result := range results {
		if result.Passed {
			continue
		}
		if isBlocking(result.Name, cfg) {
			summary.AllPassed = false
			if summary.FailedCheck == "" {
				summary.FailedCheck = result.Name
			}
			continue
		}
		summary.Warnings++
	}
	return summary
}

func (s *Service) RunSudoersBootstrapJob(req SudoersRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindSudoersEnable, req.Policy, "sudoers.enable.complete", "sudoers.enable.ssh_dial", "Configuring passwordless apt sudoers...", func(r *withActorRunner) {
		r.setJobPhase(jobs.PhaseApply)
		line := fmt.Sprintf("%s ALL=(root) NOPASSWD: /usr/bin/apt, /usr/bin/apt-get, /usr/bin/dpkg --audit, /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock", r.server.User)
		escapedLine := ShellEscapeSingleQuotes(line)
		cmd := fmt.Sprintf("sudo -S -p '' sh -c \"printf '%%s\\n' '%s' > /etc/sudoers.d/apt-nopasswd && chmod 440 /etc/sudoers.d/apt-nopasswd && /usr/sbin/visudo -cf /etc/sudoers.d/apt-nopasswd\"", escapedLine)
		r.runSingleCommand("sudoers.enable.command", "\nsudoers enable attempt %d/%d failed: %v; retrying in %s", cmd, func() io.Reader {
			return strings.NewReader(req.SudoPassword + "\n")
		}, "\nPasswordless apt sudoers enabled.")
	})
}

func (s *Service) RunSudoersDisableJob(req SudoersRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindSudoersDisable, req.Policy, "sudoers.disable.complete", "sudoers.disable.ssh_dial", "Disabling passwordless apt sudoers...", func(r *withActorRunner) {
		r.setJobPhase(jobs.PhaseApply)
		r.runSingleCommand("sudoers.disable.command", "\nsudoers disable attempt %d/%d failed: %v; retrying in %s", "sudo -S -p '' rm -f /etc/sudoers.d/apt-nopasswd", func() io.Reader {
			return strings.NewReader(req.SudoPassword + "\n")
		}, "\nPasswordless apt sudoers disabled.")
	})
}

func (s *Service) RunAutoremoveJob(req AutoremoveRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindAutoremove, req.Policy, "autoremove.complete", "autoremove.ssh_dial", "Running apt autoremove...", func(r *withActorRunner) {
		r.setJobPhase(jobs.PhaseAutoremove)
		r.runSingleCommand("autoremove.command", "\nautoremove attempt %d/%d failed: %v; retrying in %s", AptAutoremoveCmd, nil, "\nAutoremove completed.")
	})
}

func (s *Service) runCommandJob(server servers.Server, actor, clientIP, jobID, jobKind string, policy RetryPolicy, auditAction, dialOpName, description string, runSteps func(*withActorRunner)) {
	s.runWithActorShared(
		server,
		actor,
		clientIP,
		jobID,
		jobKind,
		policy,
		auditAction,
		func(status *servers.ServerStatus, policy RetryPolicy) {
			if jobKind == jobs.KindAutoremove {
				status.Status = "autoremove"
			} else {
				status.Status = "sudoers"
			}
			if strings.TrimSpace(status.Logs) == "" {
				status.Logs = "Starting Linux Updater..."
			}
			status.Logs += fmt.Sprintf(
				"\nRetries enabled: max_attempts=%d base_delay=%s max_delay=%s jitter=%d%%\n%s",
				policy.MaxAttempts,
				policy.BaseDelay,
				policy.MaxDelay,
				policy.JitterPct,
				description,
			)
		},
		commandRunnerAuditMeta,
		DoneOnlyOutcome,
		dialOpName,
		runSteps,
	)
}

func (r *withActorRunner) runSingleCommand(opName, retryLogFormat, cmd string, stdin func() io.Reader, successSuffix string) {
	r.retryLogFormats[opName] = retryLogFormat
	replayPolicy := ReplayRetryableErrors
	if cmd == AptAutoremoveCmd {
		replayPolicy = ReplayRetryableOutputErrors
	}
	result, err := r.session.RunCommand(context.Background(), HostCommandRequest{
		Operation:    opName,
		Command:      cmd,
		Stdin:        stdin,
		ReplayPolicy: replayPolicy,
	})
	r.commandAttempts += result.Attempts
	stdout, stderr := result.Stdout, result.Stderr
	logs := r.currentLogs() + "\n" + stdout + stderr
	if err != nil {
		r.markErrorClass(err)
		logs += fmt.Sprintf("\nError: %v", err)
		r.setErrorLogs(logs)
		return
	}
	_ = r.withStatus(func(status *servers.ServerStatus) {
		status.Status = "done"
		status.Logs = logs + successSuffix
	})
}

func (s *Service) ApprovePendingUpdate(name, scope string) (exists bool, approved bool) {
	return s.ApprovePendingUpdateWithOptions(name, scope, servers.ApprovalOptions{})
}

func (s *Service) ApprovePendingUpdateWithOptions(name, scope string, opts servers.ApprovalOptions) (exists bool, approved bool) {
	deps := s.EnsureDeps()
	if deps.ServerState == nil {
		return false, false
	}
	return deps.ServerState.ApprovePendingUpdateWithOptions(name, scope, opts)
}

func (s *Service) CancelPendingUpdate(name string) (exists bool, cancelled bool) {
	deps := s.EnsureDeps()
	if deps.ServerState == nil {
		return false, false
	}
	return deps.ServerState.CancelPendingUpdate(name)
}

func (s *Service) StartPendingCVEEnrichment(server servers.Server, updates []servers.PendingUpdate, parentJobID, actor, clientIP string) {
	deps := s.EnsureDeps()
	packages := PendingCVEPackages(updates)
	if len(packages) == 0 {
		return
	}

	var jobID string
	if jm := deps.CurrentJobManager(); jm != nil {
		job, err := jm.CreateJob(jobs.CreateParams{
			Kind:        jobs.KindCVEEnrichment,
			ParentJobID: strings.TrimSpace(parentJobID),
			ServerName:  server.Name,
			Actor:       actor,
			ClientIP:    clientIP,
			Status:      jobs.StatusQueued,
			Phase:       jobs.PhaseDial,
			Summary:     "Enriching pending updates with CVEs",
		})
		if err != nil {
			deps.Logf("failed to create CVE enrichment job for %q: %v", server.Name, err)
			for _, pkg := range packages {
				if !s.updatePendingPackageCVEState(server.Name, pkg, "unavailable", []string{}) {
					return
				}
			}
			return
		}
		jobID = job.ID
	}

	deps.StartJobRunner(jobID, func() {
		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
			phase := jobs.PhaseDial
			summary := "Connecting for CVE enrichment"
			_ = jm.Transition(jobID, jobs.Intent{Phase: &phase, Summary: &summary})
		}
		cveSession, err := deps.HostMaintenanceSessions.Open(context.Background(), HostMaintenanceSessionRequest{
			Server:         server,
			RetryPolicy:    RetryPolicy{MaxAttempts: 2, BaseDelay: 250 * time.Millisecond, MaxDelay: 250 * time.Millisecond},
			DialReplay:     ReplayAllDialErrors,
			DialOperation:  "cve_enrichment.ssh_dial",
			CommandTimeout: CVELookupCommandTimeout,
			OnRetry: func(event HostRetryEvent) {
				deps.Logf("CVE enrichment dial attempt %d failed for server %q: %v", event.Attempt, server.Name, event.Err)
			},
		})
		if err != nil {
			deps.Logf("CVE enrichment dial attempt 2 failed for server %q: %v", server.Name, err)
			if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
				status := jobs.StatusFailed
				phase := jobs.PhaseComplete
				summary := "Failed to connect for CVE enrichment"
				errorClass := "dial"
				meta := jobs.MarshalJSON(map[string]any{"error": err.Error()})
				_ = jm.Transition(jobID, jobs.Intent{
					Status:     &status,
					Phase:      &phase,
					Summary:    &summary,
					ErrorClass: &errorClass,
					MetaJSON:   &meta,
				})
			}
			for _, pkg := range packages {
				if !s.updatePendingPackageCVEState(server.Name, pkg, "unavailable", []string{}) {
					return
				}
			}
			return
		}
		defer func() { _ = cveSession.Close() }()

		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
			phase := jobs.PhaseLookup
			summary := "Looking up package CVEs"
			_ = jm.Transition(jobID, jobs.Intent{Phase: &phase, Summary: &summary})
		}
		for _, pkg := range packages {
			if !s.serverPendingApproval(server.Name) {
				if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
					status := jobs.StatusCancelled
					phase := jobs.PhaseComplete
					summary := "Parent update no longer pending approval"
					_ = jm.Transition(jobID, jobs.Intent{
						Status:  &status,
						Phase:   &phase,
						Summary: &summary,
					})
				}
				return
			}
			cves, queryErr := cveSession.QueryPackageCVEs(context.Background(), pkg)
			state := "ready"
			if queryErr != nil {
				deps.Logf("CVE lookup failed for server %q package %q: %v", server.Name, pkg, queryErr)
				state = "unavailable"
				cves = []string{}
			}
			if !s.updatePendingPackageCVEState(server.Name, pkg, state, cves) {
				if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
					status := jobs.StatusCancelled
					phase := jobs.PhaseComplete
					summary := "Pending update state changed before CVE enrichment finished"
					_ = jm.Transition(jobID, jobs.Intent{
						Status:  &status,
						Phase:   &phase,
						Summary: &summary,
					})
				}
				return
			}
		}
		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
			status := jobs.StatusSucceeded
			phase := jobs.PhaseComplete
			summary := "CVE enrichment completed"
			_ = jm.Transition(jobID, jobs.Intent{
				Status:  &status,
				Phase:   &phase,
				Summary: &summary,
			})
		}
	})
}

func (s *Service) serverPendingApproval(serverName string) bool {
	deps := s.EnsureDeps()
	if deps.ServerState == nil {
		return false
	}
	snapshot := deps.ServerState.CurrentStatusSnapshot(serverName)
	return snapshot != nil && snapshot.Status == "pending_approval"
}

func (s *Service) updatePendingPackageCVEState(serverName, pkg, state string, cves []string) bool {
	deps := s.EnsureDeps()
	if deps.ServerState == nil {
		return false
	}
	deps.ServerState.Lock()
	defer deps.ServerState.Unlock()
	status := deps.ServerState.StatusMap()[serverName]
	if status == nil || status.Status != "pending_approval" {
		return false
	}
	updated := false
	for i := range status.PendingUpdates {
		if status.PendingUpdates[i].Package != pkg {
			continue
		}
		status.PendingUpdates[i].CVEState = state
		status.PendingUpdates[i].CVEs = append([]string(nil), cves...)
		updated = true
	}
	if updated {
		SortPendingUpdates(status.PendingUpdates)
	}
	return true
}
