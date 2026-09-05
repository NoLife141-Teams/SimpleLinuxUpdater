package updates

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
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

// RefreshServerFacts opens one bounded read-only Host Maintenance Session,
// captures the current host health facts, and persists the accepted observation.
func (s *Service) RefreshServerFacts(ctx context.Context, server servers.Server, dialOperation string) (ServerFactsRecord, error) {
	deps := s.EnsureDeps()
	if ctx == nil {
		ctx = context.Background()
	}
	if strings.TrimSpace(dialOperation) == "" {
		dialOperation = "facts_refresh.ssh_dial"
	}
	session, err := deps.HostMaintenanceSessions.Open(ctx, HostMaintenanceSessionRequest{
		Server:         server,
		RetryPolicy:    RetryPolicy{MaxAttempts: 1},
		DialOperation:  dialOperation,
		CommandTimeout: deps.LoadCommandTimeout(),
	})
	if err != nil {
		return ServerFactsRecord{}, err
	}
	defer func() { _ = session.Close() }()
	record := session.CollectServerFacts(ctx)
	if err := ctx.Err(); err != nil {
		return ServerFactsRecord{}, err
	}
	if err := deps.SaveServerFacts(record); err != nil {
		return ServerFactsRecord{}, err
	}
	return record, nil
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
	if d.WaitForApprovalPollContext == nil {
		if d.WaitForApprovalPoll != nil {
			poll := d.WaitForApprovalPoll
			d.WaitForApprovalPollContext = func(ctx context.Context) error {
				if ctx == nil {
					ctx = context.Background()
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				poll()
				return ctx.Err()
			}
		} else {
			d.WaitForApprovalPollContext = func(ctx context.Context) error {
				if ctx == nil {
					ctx = context.Background()
				}
				timer := time.NewTimer(ApprovalPollInterval)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				}
			}
		}
	}
	if d.WaitForApprovalPoll == nil {
		d.WaitForApprovalPoll = func() { time.Sleep(ApprovalPollInterval) }
	}
	if d.SleepContext == nil {
		if d.Sleep != nil {
			sleep := d.Sleep
			d.SleepContext = func(ctx context.Context, delay time.Duration) error {
				if ctx == nil {
					ctx = context.Background()
				}
				if err := ctx.Err(); err != nil {
					return err
				}
				done := make(chan struct{})
				go func() {
					sleep(delay)
					close(done)
				}()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-done:
					return ctx.Err()
				}
			}
		} else {
			d.SleepContext = func(ctx context.Context, delay time.Duration) error {
				if ctx == nil {
					ctx = context.Background()
				}
				timer := time.NewTimer(delay)
				defer timer.Stop()
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-timer.C:
					return nil
				}
			}
		}
	}
	if d.Sleep == nil {
		d.Sleep = time.Sleep
	}
	if d.HostMaintenanceSessions == nil {
		d.HostMaintenanceSessions = hostMaintenanceUnavailableFactory()
	}
	if d.VulnerabilityScanner == nil {
		d.VulnerabilityScanner = unavailableVulnerabilityScanner{}
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

	session        HostMaintenanceSession
	maintenanceCtx context.Context

	commandTimeout time.Duration

	sshDialAttempts        int
	aptUpdateAttempts      int
	listUpgradableAttempts int
	aptUpgradeAttempts     int
	commandAttempts        int

	retryExhausted       bool
	lastErrClass         string
	auditOutcomeOverride string

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

const (
	liveCommandLogFlushInterval = 250 * time.Millisecond
	liveCommandLogFlushBytes    = 16 * 1024
)

type liveCommandLogSink struct {
	mu         sync.Mutex
	runner     *withActorRunner
	pending    []HostCommandOutput
	pendingLen int
	lastFlush  time.Time
	hasFlushed bool
	received   bool
	timer      *time.Timer
}

func newLiveCommandLogSink(runner *withActorRunner) *liveCommandLogSink {
	return &liveCommandLogSink{runner: runner}
}

func (s *liveCommandLogSink) Handle(output HostCommandOutput) {
	if s == nil || s.runner == nil || output.Data == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.received {
		output.Data = "\n" + output.Data
		s.received = true
	}
	if len(s.pending) > 0 && s.pending[len(s.pending)-1].Stream == output.Stream {
		s.pending[len(s.pending)-1].Data += output.Data
	} else {
		s.pending = append(s.pending, output)
	}
	s.pendingLen += len(output.Data)
	now := s.runner.deps().Now()
	if !s.hasFlushed || now.Sub(s.lastFlush) >= liveCommandLogFlushInterval || s.pendingLen >= liveCommandLogFlushBytes {
		s.stopTimerLocked()
		s.flushLocked(now)
		return
	}
	s.scheduleFlushLocked(liveCommandLogFlushInterval - now.Sub(s.lastFlush))
}

func (s *liveCommandLogSink) Flush() {
	if s == nil || s.runner == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopTimerLocked()
	s.flushLocked(s.runner.deps().Now())
}

func (s *liveCommandLogSink) Received() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.received
}

func (s *liveCommandLogSink) scheduleFlushLocked(delay time.Duration) {
	if s.timer != nil {
		return
	}
	if delay < 0 {
		delay = 0
	}
	s.timer = time.AfterFunc(delay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.timer = nil
		s.flushLocked(s.runner.deps().Now())
	})
}

func (s *liveCommandLogSink) stopTimerLocked() {
	if s.timer == nil {
		return
	}
	s.timer.Stop()
	s.timer = nil
}

func (s *liveCommandLogSink) flushLocked(now time.Time) {
	if len(s.pending) == 0 {
		return
	}
	s.runner.appendLiveStatusLogs(s.pending)
	s.pending = nil
	s.pendingLen = 0
	s.lastFlush = now
	s.hasFlushed = true
}

func (r *withActorRunner) deps() ServiceDeps {
	if r != nil && r.service != nil {
		return r.service.EnsureDeps()
	}
	return ServiceDeps{}.withDefaults()
}

func (r *withActorRunner) maintenanceContext() context.Context {
	if r != nil && r.maintenanceCtx != nil {
		return r.maintenanceCtx
	}
	return context.Background()
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

func (r *withActorRunner) appendLiveStatusLogs(outputs []HostCommandOutput) {
	if len(outputs) == 0 {
		return
	}
	var combined strings.Builder
	fragments := make([]jobs.LogFragment, 0, len(outputs))
	for _, output := range outputs {
		if output.Data == "" {
			continue
		}
		combined.WriteString(output.Data)
		fragments = append(fragments, jobs.LogFragment{Stream: string(output.Stream), Data: output.Data})
	}
	if len(fragments) == 0 {
		return
	}
	line := combined.String()
	deps := r.deps()
	if deps.ServerState == nil {
		return
	}
	deps.ServerState.Lock()
	status := deps.ServerState.StatusMap()[r.server.Name]
	if status == nil {
		deps.ServerState.Unlock()
		return
	}
	status.Logs += line
	deps.ServerState.Unlock()

	jm := r.currentJobManager()
	if jm == nil || strings.TrimSpace(r.jobID) == "" {
		return
	}
	if _, err := jm.AppendActiveLogFragments(r.jobID, fragments); err != nil {
		deps.Logf("failed to append live logs to job %q: %v", r.jobID, err)
	}
}

func (r *withActorRunner) setErrorLogs(logs string) {
	_ = r.withStatus(func(status *servers.ServerStatus) {
		status.Status = "error"
		status.Logs = logs
	})
}

func (r *withActorRunner) setSudoPolicyErrorLogs(logs string) {
	r.lastErrClass = "sudo_policy"
	_ = r.withStatus(func(status *servers.ServerStatus) {
		status.Status = runtimepkg.StatusError
		status.Logs = logs + "\nPasswordless APT access is missing or outdated. Click Enable apt for this server, enter the host's sudo password, wait for it to succeed, then retry the update."
	})
}

func (r *withActorRunner) setCommandErrorLogs(logs string, err error) {
	if !strings.EqualFold(strings.TrimSpace(r.server.User), "root") && IsSudoPolicyError(logs+"\n"+err.Error()) {
		r.setSudoPolicyErrorLogs(logs)
		return
	}
	var reconciliation interface{ RequiresReconciliation() bool }
	if r.jobKind == jobs.KindAptRepair || (errors.As(err, &reconciliation) && reconciliation.RequiresReconciliation()) {
		r.lastErrClass = "reconciliation_required"
		_ = r.withStatus(func(status *servers.ServerStatus) {
			status.Status = runtimepkg.StatusNeedsReconciliation
			status.Logs = logs + "\nAPT outcome requires reconciliation before another package mutation."
		})
		return
	}
	r.setErrorLogs(logs)
}

func (r *withActorRunner) currentLogs() string {
	deps := r.deps()
	if deps.ServerState == nil {
		return ""
	}
	return deps.ServerState.CurrentStatusLogs(r.server.Name)
}

func (r *withActorRunner) interruptForShutdown(summary string) {
	if r == nil {
		return
	}
	if strings.TrimSpace(summary) == "" {
		summary = "Maintenance interrupted by application shutdown"
	}
	r.lastErrClass = "interrupted"
	r.auditOutcomeOverride = "interrupted"
	r.jobPhase = jobs.PhaseComplete
	logs := r.currentLogs()
	if strings.TrimSpace(logs) != "" {
		logs += "\n"
	}
	logs += summary

	deps := r.deps()
	if deps.ServerState != nil {
		deps.ServerState.Lock()
		if status := deps.ServerState.StatusMap()[r.server.Name]; status != nil {
			status.Status = runtimepkg.StatusIdle
			status.ApprovalScope = ""
			status.ApprovalConfirmRemovals = false
			status.Upgradable = nil
			status.PendingUpdates = nil
			status.UpgradePlan = servers.UpgradePlan{}
			status.Logs = logs
		}
		deps.ServerState.Unlock()
	}

	if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(r.jobID) != "" {
		jobStatus := jobs.StatusInterrupted
		phase := jobs.PhaseComplete
		errorClass := "interrupted"
		if err := jm.Transition(r.jobID, jobs.Intent{
			Status:     &jobStatus,
			Phase:      &phase,
			Summary:    &summary,
			LogsText:   &logs,
			ErrorClass: &errorClass,
		}); err != nil {
			deps.Logf("failed to mark job %q interrupted during application shutdown: %v", r.jobID, err)
		}
	}
}

func (r *withActorRunner) handleShutdownCancellation(err error, summary string) bool {
	if !errors.Is(err, context.Canceled) {
		return false
	}
	var reconciliation interface{ RequiresReconciliation() bool }
	if errors.As(err, &reconciliation) && reconciliation.RequiresReconciliation() {
		return false
	}
	r.interruptForShutdown(summary)
	return true
}

func (r *withActorRunner) setJobPhase(phase string) {
	r.jobPhase = strings.TrimSpace(phase)
	if jm := r.currentJobManager(); jm != nil && strings.TrimSpace(r.jobID) != "" && r.jobPhase != "" {
		if err := jm.Transition(r.jobID, jobs.Intent{Phase: &r.jobPhase}); err != nil {
			r.deps().Logf("failed to update job %q phase to %q: %v", r.jobID, r.jobPhase, err)
		}
	}
}

func (r *withActorRunner) requireMutationPhase(phase string) bool {
	phase = strings.TrimSpace(phase)
	var err error
	if jm := r.currentJobManager(); jm != nil && strings.TrimSpace(r.jobID) != "" && phase != "" {
		status := jobs.StatusRunning
		err = jm.Transition(r.jobID, jobs.Intent{Status: &status, Phase: &phase})
	} else {
		r.jobPhase = phase
	}
	if err != nil {
		r.deps().Logf("failed to persist job %q APT mutation phase %q: %v", r.jobID, phase, err)
		r.lastErrClass = "persistence"
		logs := r.currentLogs() + fmt.Sprintf("\nUnable to persist the APT mutation phase; command aborted before execution: %v", err)
		r.setCommandErrorLogs(logs, err)
		return false
	}
	r.jobPhase = phase
	return true
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
	var classified interface{ Transient() bool }
	if errors.As(err, &classified) && classified.Transient() {
		r.lastErrClass = "transient"
		return
	}
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
	session, err := deps.HostMaintenanceSessions.Open(r.maintenanceContext(), HostMaintenanceSessionRequest{
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
		if r.handleShutdownCancellation(err, "Maintenance interrupted while establishing SSH connectivity.") {
			return false
		}
		r.markErrorClass(err)
		switch HostMaintenanceErrorStageOf(err) {
		case HostMaintenanceStageAuth:
			r.setCommandErrorLogs(fmt.Sprintf("Auth setup failed: %v", err), err)
		case HostMaintenanceStageHostKey:
			r.setCommandErrorLogs(fmt.Sprintf("Host key verification setup failed: %v", err), err)
		default:
			r.setCommandErrorLogs(fmt.Sprintf("SSH connection failed: %v", err), err)
		}
		return false
	}
	r.session = session
	r.maintenanceCtx = maintenanceContextForSession(session)
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
		message := fmt.Sprintf("Final status: %s", finalStatus)
		if runner.auditOutcomeOverride != "" {
			outcome = runner.auditOutcomeOverride
			if outcome == "interrupted" {
				message = "Maintenance interrupted by application shutdown"
			}
		}
		deps.AuditWithActor(
			actor,
			clientIP,
			auditAction,
			"server",
			server.Name,
			outcome,
			message,
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
		"interrupted":                   r.auditOutcomeOverride == "interrupted",
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

func (r *withActorRunner) refreshFactsAfterSuccessfulUpdate() bool {
	if r == nil || r.session == nil {
		return true
	}
	deps := r.deps()
	ctx := r.maintenanceContext()
	record := r.session.CollectServerFacts(ctx)
	if err := ctx.Err(); err != nil {
		r.interruptForShutdown("Update interrupted during final host facts refresh.")
		return false
	}
	if err := deps.SaveServerFacts(record); err != nil {
		deps.Logf("failed to refresh facts after update for %q: %v", r.server.Name, err)
	}
	if err := ctx.Err(); err != nil {
		r.interruptForShutdown("Update interrupted before successful completion could be persisted.")
		return false
	}
	return true
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
				"Starting Linux Updater...\n%s\nRetries enabled: max_attempts=%d base_delay=%s max_delay=%s jitter=%d%%",
				AptInteractionStrategySummary,
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
			maintenanceCtx := r.maintenanceContext()
			r.setJobPhase(jobs.PhasePrechecks)
			r.postchecksEnabled = postcheckCfg.Enabled
			r.appendStatusLog("\nRunning pre-checks...")

			precheckSummary := r.session.RunUpdatePrechecks(maintenanceCtx)
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
			if precheckSummary.FailedCheck == "application_shutdown" || errors.Is(maintenanceCtx.Err(), context.Canceled) {
				r.precheckFailed = "application_shutdown"
				r.interruptForShutdown("Update interrupted during pre-checks.")
				return
			}
			if !precheckSummary.AllPassed {
				r.precheckFailed = precheckSummary.FailedCheck
				logs := r.currentLogs() + fmt.Sprintf("\nPre-check failed (%s). Update aborted before apt update.", precheckSummary.FailedCheck)
				if !strings.EqualFold(strings.TrimSpace(r.server.User), "root") && failedCheckResultsHaveSudoPolicyError(precheckSummary.Results, nil) {
					r.setSudoPolicyErrorLogs(logs)
					return
				}
				r.lastErrClass = "permanent"
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "error"
					status.Logs = logs
				})
				return
			}
			r.prechecksPassed = true
			_ = r.withStatus(func(status *servers.ServerStatus) {
				status.Logs += "\nPre-checks passed.\nRunning apt update..."
			})

			preUpdateFailedUnitsMap := make(map[string]struct{})
			preUpdateFailedUnits, _, preUnitsErr := r.session.ListFailedSystemdUnits(maintenanceCtx)
			if r.handleShutdownCancellation(preUnitsErr, "Update interrupted while capturing the pre-update systemd baseline.") {
				return
			}
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
			if r.handleShutdownCancellation(maintenanceCtx.Err(), "Update interrupted before apt update.") {
				return
			}

			r.setJobPhase(jobs.PhaseAptUpdate)
			r.retryLogFormats["update.apt_update"] = "\napt update attempt %d/%d failed: %v; retrying in %s"
			commandResult, err := r.session.RunCommand(maintenanceCtx, HostCommandRequest{
				Operation:    "update.apt_update",
				Command:      AptUpdateCmd,
				Effect:       HostCommandEffectMetadataMutation,
				ReplayPolicy: ReplayRetryableOutputErrors,
			})
			r.aptUpdateAttempts += commandResult.Attempts
			stdout, stderr := commandResult.Stdout, commandResult.Stderr
			logs := r.currentLogs() + "\n" + stdout + stderr
			if err != nil {
				if r.handleShutdownCancellation(err, "Update interrupted during apt update.") {
					return
				}
				r.markErrorClass(err)
				logs += fmt.Sprintf("\nError: %v", err)
				r.setCommandErrorLogs(logs, err)
				return
			}

			r.retryLogFormats["update.list_upgradable"] = "\nlist upgradable attempt %d/%d failed: %v; retrying in %s"
			discoveryResult, err := r.session.DiscoverPackages(maintenanceCtx, HostOperationRequest{Operation: "update.list_upgradable"})
			r.listUpgradableAttempts += discoveryResult.Attempts
			discovery := discoveryResult.Outcome
			if err != nil {
				if r.handleShutdownCancellation(err, "Update interrupted while discovering upgradable packages.") {
					return
				}
				r.markErrorClass(err)
				r.setErrorLogs(logs + fmt.Sprintf("\nError listing upgradable: %v", err))
				return
			}
			if r.handleShutdownCancellation(maintenanceCtx.Err(), "Update interrupted after package discovery.") {
				return
			}

			if discovery.Empty() {
				if !r.refreshFactsAfterSuccessfulUpdate() {
					return
				}
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
			planDiskResult := r.session.RunPlanDiskPrecheck(maintenanceCtx, discovery.UpgradePlan)
			if r.handleShutdownCancellation(maintenanceCtx.Err(), "Update interrupted during the plan-aware disk pre-check.") {
				return
			}
			r.precheckResults = append(r.precheckResults, planDiskResult)
			planDiskState := "PASS"
			if !planDiskResult.Passed {
				planDiskState = "FAIL"
			}
			planDiskLog := fmt.Sprintf("\nPre-check %s [%s]: %s", planDiskResult.Name, planDiskState, planDiskResult.Details)
			if trimmed := strings.TrimSpace(planDiskResult.Output); trimmed != "" {
				planDiskLog += fmt.Sprintf(" Output: %s", trimmed)
			}
			r.appendStatusLog(planDiskLog)
			if !planDiskResult.Passed {
				r.lastErrClass = "permanent"
				r.prechecksPassed = false
				r.precheckFailed = planDiskResult.Name
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = runtimepkg.StatusError
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.Upgradable = nil
					status.PendingUpdates = nil
					status.UpgradePlan = servers.CloneUpgradePlan(discovery.UpgradePlan)
					status.Logs += "\nPlan-aware disk pre-check failed. Update aborted before approval or package mutation."
				})
				return
			}
			logs = r.currentLogs()
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
					if err := deps.WaitForApprovalPollContext(maintenanceCtx); err != nil {
						if r.handleShutdownCancellation(err, "Update interrupted while waiting for approval.") {
							return
						}
					}
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
				if !r.refreshFactsAfterSuccessfulUpdate() {
					return
				}
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

			if !r.requireMutationPhase(jobs.PhaseAptUpgrade) {
				return
			}
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
			liveOutput := newLiveCommandLogSink(r)
			commandResult, err = r.session.RunCommand(maintenanceCtx, HostCommandRequest{
				Operation:         "update.apt_upgrade",
				Command:           upgradeCmd,
				Effect:            HostCommandEffectPackageStateMutation,
				ReplayPolicy:      ReplayRetryableOutputErrors,
				OnOutput:          liveOutput.Handle,
				OnAttemptComplete: liveOutput.Flush,
			})
			liveOutput.Flush()
			r.aptUpgradeAttempts += commandResult.Attempts
			stdout, stderr = commandResult.Stdout, commandResult.Stderr
			logs = r.currentLogs()
			if !liveOutput.Received() {
				logs += "\n" + stdout + stderr
			}
			if err != nil {
				if r.handleShutdownCancellation(err, "Update interrupted during package mutation.") {
					return
				}
				r.markErrorClass(err)
				logs += fmt.Sprintf("\nError: %v", err)
				r.setCommandErrorLogs(logs, err)
				return
			}
			r.upgradeCompleted = true

			if !postcheckCfg.Enabled {
				r.postchecksPassed = true
				if !r.refreshFactsAfterSuccessfulUpdate() {
					return
				}
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

			inspectionSummary := r.session.RunPostUpdateHealthChecks(maintenanceCtx, postcheckCfg, preUpdateFailedUnitsMap)
			if r.handleShutdownCancellation(maintenanceCtx.Err(), "Update interrupted during post-update health checks.") {
				return
			}
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
				r.postcheckFailed = postcheckSummary.FailedCheck
				r.postchecksPassed = false
				logs := r.currentLogs() + fmt.Sprintf("\nUpgrade completed but post-check failed (%s).", postcheckSummary.FailedCheck)
				if !strings.EqualFold(strings.TrimSpace(r.server.User), "root") && failedCheckResultsHaveSudoPolicyError(postcheckSummary.Results, func(name string) bool {
					return deps.IsPostcheckFailureBlocking(name, postcheckCfg)
				}) {
					r.lastErrClass = "sudo_policy"
					_ = r.withStatus(func(status *servers.ServerStatus) {
						status.Status = runtimepkg.StatusError
						status.ApprovalScope = ""
						status.ApprovalConfirmRemovals = false
						status.PendingUpdates = nil
						status.UpgradePlan = servers.UpgradePlan{}
						status.Logs = logs + "\nPasswordless APT access is missing or outdated. Click Enable apt for this server, enter the host's sudo password, wait for it to succeed, then retry the update."
					})
					return
				}
				r.lastErrClass = "permanent"
				_ = r.withStatus(func(status *servers.ServerStatus) {
					status.Status = "error"
					status.ApprovalScope = ""
					status.ApprovalConfirmRemovals = false
					status.PendingUpdates = nil
					status.UpgradePlan = servers.UpgradePlan{}
					status.Logs = logs
				})
				return
			}

			r.postchecksPassed = true
			finalLogs := r.currentLogs()
			if postcheckSummary.Warnings > 0 {
				if !r.refreshFactsAfterSuccessfulUpdate() {
					return
				}
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

			if !r.refreshFactsAfterSuccessfulUpdate() {
				return
			}
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
		cmd, err := BuildSudoersBootstrapCommand(r.server.User)
		if err != nil {
			r.lastErrClass = "validation"
			r.setErrorLogs(r.currentLogs() + "\nError: " + err.Error())
			return
		}
		r.runSingleCommand("sudoers.enable.command", "\nsudoers enable attempt %d/%d failed: %v; retrying in %s", cmd, HostCommandEffectSystemStateMutation, ReplayRetryableErrors, false, func() io.Reader {
			return strings.NewReader(req.SudoPassword + "\n")
		}, "\nRestricted SimpleLinuxUpdater sudoers helper enabled.")
	})
}

func (s *Service) RunSudoersDisableJob(req SudoersRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindSudoersDisable, req.Policy, "sudoers.disable.complete", "sudoers.disable.ssh_dial", "Disabling passwordless apt sudoers...", func(r *withActorRunner) {
		r.setJobPhase(jobs.PhaseApply)
		cmd, err := BuildSudoersDisableCommand(r.server.User)
		if err != nil {
			r.lastErrClass = "validation"
			r.setErrorLogs(r.currentLogs() + "\nError: " + err.Error())
			return
		}
		r.runSingleCommand("sudoers.disable.command", "\nsudoers disable attempt %d/%d failed: %v; retrying in %s", cmd, HostCommandEffectSystemStateMutation, ReplayRetryableErrors, false, func() io.Reader {
			return strings.NewReader(req.SudoPassword + "\n")
		}, "\nRestricted SimpleLinuxUpdater sudoers helper disabled.")
	})
}

func (s *Service) RunAutoremoveJob(req AutoremoveRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindAutoremove, req.Policy, "autoremove.complete", "autoremove.ssh_dial", "Running apt autoremove...\n"+AptInteractionStrategySummary, func(r *withActorRunner) {
		if !r.requireMutationPhase(jobs.PhaseAutoremove) {
			return
		}
		r.runSingleCommand("autoremove.command", "\nautoremove attempt %d/%d failed: %v; retrying in %s", AptAutoremoveCmd, HostCommandEffectPackageStateMutation, ReplayRetryableOutputErrors, true, nil, "\nAutoremove completed.")
	})
}

func (s *Service) RunAptRepairJob(req AptRepairRunRequest) {
	s.runCommandJob(req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindAptRepair, req.Policy, "apt_repair.complete", "apt_repair.ssh_dial", "Inspecting and repairing APT/DPKG state...\n"+AptInteractionStrategySummary, func(r *withActorRunner) {
		if !r.requireMutationPhase(jobs.PhaseReconcile) {
			return
		}
		r.runSingleCommand("apt_repair.command", "\nAPT repair attempt %d/%d failed: %v; retrying in %s", AptRepairCmd, HostCommandEffectPackageStateMutation, ReplayRetryableErrors, false, nil, "\nAPT/DPKG repair completed and package health checks passed.")
	})
}

func (s *Service) RunRebootJob(req RebootRunRequest) {
	s.runWithActorShared(
		req.Server, req.Actor, req.ClientIP, req.JobID, jobs.KindReboot, req.Policy,
		"reboot.complete",
		func(status *servers.ServerStatus, policy RetryPolicy) {
			status.Status = runtimepkg.StatusRebooting
			status.Logs = fmt.Sprintf("Starting controlled reboot...\nRetries enabled: max_attempts=%d base_delay=%s max_delay=%s jitter=%d%%", policy.MaxAttempts, policy.BaseDelay, policy.MaxDelay, policy.JitterPct)
		},
		commandRunnerAuditMeta,
		DoneOnlyOutcome,
		"reboot.ssh_dial",
		func(r *withActorRunner) {
			maintenanceCtx := r.maintenanceContext()
			baseline := r.session.CollectServerFacts(maintenanceCtx)
			if r.handleShutdownCancellation(maintenanceCtx.Err(), "Reboot interrupted while capturing the uptime baseline.") {
				return
			}
			if baseline.UptimeSeconds <= 0 {
				r.lastErrClass = "verification"
				r.setErrorLogs(r.currentLogs() + "\nUnable to establish a positive uptime baseline; reboot was not sent.")
				return
			}
			r.appendStatusLog(fmt.Sprintf("\nBaseline captured: uptime=%ds kernel=%s.", baseline.UptimeSeconds, strings.TrimSpace(baseline.RunningKernelVersion)))
			r.setJobPhase(jobs.PhaseReboot)
			result, err := r.session.RunCommand(maintenanceCtx, HostCommandRequest{Operation: "reboot.command", Command: ControlledRebootCmd, Effect: HostCommandEffectSystemStateMutation, ReplayPolicy: ReplayNever})
			r.commandAttempts += result.Attempts
			if err != nil {
				if r.handleShutdownCancellation(err, "Reboot interrupted while sending the reboot command.") {
					return
				}
				r.markErrorClass(err)
				r.setErrorLogs(r.currentLogs() + fmt.Sprintf("\nReboot command failed before acknowledgement: %v", err))
				return
			}
			r.closeSession()
			r.setJobPhase(jobs.PhaseVerify)
			r.appendStatusLog("\nReboot command acknowledged. Waiting for the host to restart and return over SSH...")
			deps := r.deps()
			verifyPolicy := r.policy
			verifyPolicy.MaxAttempts = 1
			for attempt := 1; attempt <= RebootVerificationAttempts; attempt++ {
				if err := deps.SleepContext(maintenanceCtx, RebootVerificationInterval); err != nil {
					if r.handleShutdownCancellation(err, "Reboot verification interrupted by application shutdown.") {
						return
					}
				}
				session, openErr := deps.HostMaintenanceSessions.Open(maintenanceCtx, HostMaintenanceSessionRequest{
					Server: r.server, RetryPolicy: verifyPolicy, DialOperation: "reboot.verify.ssh_dial", CommandTimeout: r.commandTimeout,
				})
				if openErr != nil {
					if r.handleShutdownCancellation(openErr, "Reboot verification interrupted by application shutdown.") {
						return
					}
					r.appendStatusLog(fmt.Sprintf("\nReboot verification %d/%d: host not reachable yet.", attempt, RebootVerificationAttempts))
					continue
				}
				facts := session.CollectServerFacts(maintenanceCtx)
				_ = session.Close()
				if r.handleShutdownCancellation(maintenanceCtx.Err(), "Reboot verification interrupted while collecting host facts.") {
					return
				}
				if facts.UptimeSeconds > 0 && facts.UptimeSeconds < baseline.UptimeSeconds {
					if facts.RebootRequired != nil && *facts.RebootRequired {
						r.appendStatusLog(fmt.Sprintf("\nReboot verification %d/%d: host returned, but still reports reboot required.", attempt, RebootVerificationAttempts))
						continue
					}
					if err := deps.SaveServerFacts(facts); err != nil {
						deps.Logf("failed to persist post-reboot facts for %q: %v", r.server.Name, err)
					}
					if r.handleShutdownCancellation(maintenanceCtx.Err(), "Reboot verification interrupted while persisting host facts.") {
						return
					}
					logs := r.currentLogs()
					_ = r.withStatus(func(status *servers.ServerStatus) {
						status.Status = runtimepkg.StatusDone
						status.Logs = logs + fmt.Sprintf("\nReboot verified: SSH restored, uptime reset to %ds, running kernel=%s.", facts.UptimeSeconds, strings.TrimSpace(facts.RunningKernelVersion))
					})
					return
				}
				r.appendStatusLog(fmt.Sprintf("\nReboot verification %d/%d: SSH reachable, but uptime reset is not proven yet.", attempt, RebootVerificationAttempts))
			}
			r.lastErrClass = "verification"
			r.setErrorLogs(r.currentLogs() + "\nReboot could not be verified before the verification window expired.")
		},
	)
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
			} else if jobKind == jobs.KindAptRepair {
				status.Status = runtimepkg.StatusRepairing
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

func (r *withActorRunner) runSingleCommand(opName, retryLogFormat, cmd string, effect HostCommandEffect, replayPolicy HostCommandReplayPolicy, streamOutput bool, stdin func() io.Reader, successSuffix string) {
	r.retryLogFormats[opName] = retryLogFormat
	var liveOutput *liveCommandLogSink
	if streamOutput {
		liveOutput = newLiveCommandLogSink(r)
	}
	request := HostCommandRequest{
		Operation:    opName,
		Command:      cmd,
		Effect:       effect,
		Stdin:        stdin,
		ReplayPolicy: replayPolicy,
	}
	if liveOutput != nil {
		request.OnOutput = liveOutput.Handle
		request.OnAttemptComplete = liveOutput.Flush
	}
	result, err := r.session.RunCommand(r.maintenanceContext(), request)
	if liveOutput != nil {
		liveOutput.Flush()
	}
	r.commandAttempts += result.Attempts
	stdout, stderr := result.Stdout, result.Stderr
	logs := r.currentLogs()
	if liveOutput == nil || !liveOutput.Received() {
		logs += "\n" + stdout + stderr
	}
	if err != nil {
		if r.handleShutdownCancellation(err, "Maintenance operation interrupted by application shutdown.") {
			return
		}
		r.markErrorClass(err)
		logs += fmt.Sprintf("\nError: %v", err)
		r.setCommandErrorLogs(logs, err)
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

	markInterrupted := func(err error) {
		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
			status := jobs.StatusInterrupted
			phase := jobs.PhaseComplete
			summary := "CVE enrichment interrupted by application shutdown"
			errorClass := "interrupted"
			metaMap := map[string]any{}
			if err != nil {
				metaMap["error"] = err.Error()
			}
			meta := jobs.MarshalJSON(metaMap)
			_ = jm.Transition(jobID, jobs.Intent{
				Status:     &status,
				Phase:      &phase,
				Summary:    &summary,
				ErrorClass: &errorClass,
				MetaJSON:   &meta,
			})
		}
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
			if errors.Is(err, context.Canceled) {
				markInterrupted(err)
				return
			}
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
		cveCtx := maintenanceContextForSession(cveSession)

		if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
			phase := jobs.PhaseLookup
			summary := "Looking up package CVEs"
			_ = jm.Transition(jobID, jobs.Intent{Phase: &phase, Summary: &summary})
		}
		if err := cveCtx.Err(); err != nil {
			markInterrupted(err)
			return
		}
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
		scannedUpdates, scanErr := deps.VulnerabilityScanner.Scan(cveCtx, cveSession, updates)
		if scanErr != nil {
			if errors.Is(scanErr, context.Canceled) || errors.Is(cveCtx.Err(), context.Canceled) {
				markInterrupted(scanErr)
				return
			}
			deps.Logf("official vulnerability scan failed for server %q: %v", server.Name, scanErr)
			for _, pkg := range packages {
				if !s.updatePendingPackageCVEState(server.Name, pkg, "unavailable", []string{}) {
					return
				}
			}
			if jm := deps.CurrentJobManager(); jm != nil && strings.TrimSpace(jobID) != "" {
				status := jobs.StatusFailed
				phase := jobs.PhaseComplete
				summary := "Official vulnerability data unavailable"
				errorClass := "vulnerability_data"
				meta := jobs.MarshalJSON(map[string]any{"error": scanErr.Error()})
				_ = jm.Transition(jobID, jobs.Intent{
					Status:     &status,
					Phase:      &phase,
					Summary:    &summary,
					ErrorClass: &errorClass,
					MetaJSON:   &meta,
				})
			}
			return
		}
		if err := cveCtx.Err(); err != nil {
			markInterrupted(err)
			return
		}
		for _, update := range scannedUpdates {
			if update.CVEState == "skipped" {
				continue
			}
			if !s.updatePendingPackageVulnerabilityAssessment(server.Name, update) {
				if err := cveCtx.Err(); err != nil {
					markInterrupted(err)
					return
				}
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
		if err := cveCtx.Err(); err != nil {
			markInterrupted(err)
			return
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
		status.PendingUpdates[i].CVEFindings = []servers.VulnerabilityFinding{}
		status.PendingUpdates[i].CVECoverage = ""
		status.PendingUpdates[i].CVESource = ""
		status.PendingUpdates[i].CVEScannedAt = ""
		updated = true
	}
	if updated {
		SortPendingUpdates(status.PendingUpdates)
	}
	return true
}

func (s *Service) updatePendingPackageVulnerabilityAssessment(serverName string, update servers.PendingUpdate) bool {
	deps := s.EnsureDeps()
	if deps.ServerState == nil {
		return false
	}
	updateSelector := pendingUpdatePackageSelector(update)
	if updateSelector == "" {
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
		if pendingUpdatePackageSelector(status.PendingUpdates[i]) != updateSelector {
			continue
		}
		status.PendingUpdates[i] = servers.ClonePendingUpdates([]servers.PendingUpdate{update})[0]
		updated = true
	}
	if updated {
		SortPendingUpdates(status.PendingUpdates)
	}
	return updated
}
