package updates

import (
	"context"
	"fmt"
	"strings"

	"debian-updater/internal/health"
	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"
)

const scheduledScanFactsRefreshSource = "scheduled_scan"

func BuildScheduledJobMeta(policy policies.Policy, scheduledForUTC string) ScheduledJobMeta {
	meta := ScheduledJobMeta{
		Trigger:                "scheduled",
		PolicyID:               policy.ID,
		PolicyName:             policy.Name,
		ScheduledFor:           scheduledForUTC,
		ExecutionMode:          policy.ExecutionMode,
		PackageScope:           policy.PackageScope,
		UpgradeMode:            policy.UpgradeMode,
		ApprovalTimeoutMinutes: policy.ApprovalTimeoutMinutes,
	}
	if policy.ExecutionMode == policies.ExecutionAutoApply {
		if policy.PackageScope == policies.PackageScopeSecurity {
			meta.AutoApproveScope = "security"
		} else if policy.UpgradeMode == policies.UpgradeModeFull {
			meta.AutoApproveScope = "full_upgrade"
		} else {
			meta.AutoApproveScope = "all"
		}
	}
	return meta
}

func (s *Service) RunScheduledScanJob(req ScheduledScanRunRequest) {
	deps := s.EnsureDeps()
	jm := deps.CurrentJobManager()
	setFailure := func(summary string, err error, phase string, logs string) {
		if jm != nil && strings.TrimSpace(req.JobID) != "" {
			status := jobs.StatusFailed
			jobPhase := phase
			errorClass := "permanent"
			meta := BuildScheduledJobMeta(req.Policy, req.ScheduledForUTC)
			if err != nil {
				meta.Error = err.Error()
			}
			metaJSON := jobs.MarshalJSON(meta)
			_ = jm.Transition(req.JobID, jobs.Intent{
				Status:     &status,
				Phase:      &jobPhase,
				Summary:    &summary,
				LogsText:   &logs,
				ErrorClass: &errorClass,
				MetaJSON:   &metaJSON,
			})
		}
	}

	session, err := deps.HostMaintenanceSessions.Open(context.Background(), HostMaintenanceSessionRequest{
		Server:         req.Server,
		RetryPolicy:    req.RetryPolicy,
		DialOperation:  "scheduled_scan.ssh_dial",
		CommandTimeout: deps.LoadCommandTimeout(),
	})
	if err != nil {
		summary := "Scheduled scan SSH connection failed"
		if HostMaintenanceErrorStageOf(err) == HostMaintenanceStageAuth {
			summary = "Scheduled scan auth setup failed"
		} else if HostMaintenanceErrorStageOf(err) == HostMaintenanceStageHostKey {
			summary = "Scheduled scan host key setup failed"
		}
		setFailure(summary, err, jobs.PhaseDial, "")
		return
	}
	defer func() { _ = session.Close() }()

	logs := "Starting scheduled package scan..."
	if jm != nil {
		phase := jobs.PhasePrechecks
		summary := "Running pre-checks"
		_ = jm.Transition(req.JobID, jobs.Intent{
			Phase:    &phase,
			Summary:  &summary,
			LogsText: &logs,
		})
	}
	precheckSummary := session.RunUpdatePrechecks(context.Background())
	for _, result := range precheckSummary.Results {
		state := "PASS"
		if !result.Passed {
			state = "FAIL"
		}
		line := fmt.Sprintf("\nPre-check %s [%s]: %s", result.Name, state, result.Details)
		if trimmed := strings.TrimSpace(result.Output); trimmed != "" {
			line += fmt.Sprintf(" Output: %s", trimmed)
		}
		logs += line
	}
	if !precheckSummary.AllPassed {
		setFailure(fmt.Sprintf("Scheduled scan pre-check failed (%s)", precheckSummary.FailedCheck), nil, jobs.PhasePrechecks, logs)
		return
	}

	if jm != nil {
		phase := jobs.PhaseAptUpdate
		summary := "Running apt update"
		_ = jm.Transition(req.JobID, jobs.Intent{
			Phase:    &phase,
			Summary:  &summary,
			LogsText: &logs,
		})
	}
	commandResult, err := session.RunCommand(context.Background(), HostCommandRequest{
		Operation:    "scheduled_scan.apt_update",
		Command:      AptUpdateCmd,
		Effect:       HostCommandEffectMetadataMutation,
		ReplayPolicy: ReplayRetryableOutputErrors,
	})
	stdout, stderr := commandResult.Stdout, commandResult.Stderr
	logs += "\n" + stdout + stderr
	if err != nil {
		setFailure("Scheduled scan apt update failed", err, jobs.PhaseAptUpdate, logs)
		return
	}

	discoveryResult, err := session.DiscoverPackages(context.Background(), HostOperationRequest{Operation: "scheduled_scan.list_upgradable"})
	discovery := discoveryResult.Outcome
	if err != nil {
		setFailure("Scheduled scan package discovery failed", err, jobs.PhaseAptUpdate, logs)
		return
	}

	scannedUpdates, scanErr := deps.VulnerabilityScanner.Scan(context.Background(), session, discovery.PendingUpdates)
	if scanErr != nil {
		deps.Logf("scheduled official vulnerability scan failed for server %q: %v", req.Server.Name, scanErr)
		for i := range discovery.PendingUpdates {
			if discovery.PendingUpdates[i].CVEState != "pending" {
				continue
			}
			discovery.PendingUpdates[i].CVEState = "unavailable"
			discovery.PendingUpdates[i].CVEs = []string{}
			discovery.PendingUpdates[i].CVEFindings = []servers.VulnerabilityFinding{}
		}
	} else {
		discovery.PendingUpdates = scannedUpdates
	}
	SortPendingUpdates(discovery.PendingUpdates)
	logs = s.refreshFactsAfterScheduledScan(req, session, logs)
	result := discovery.Clone()
	finalSummary := "Scheduled scan completed"
	if discovery.Empty() {
		finalSummary = "Scheduled scan completed: no pending updates"
	}
	if jm != nil {
		status := jobs.StatusSucceeded
		phase := jobs.PhaseComplete
		meta := BuildScheduledJobMeta(req.Policy, req.ScheduledForUTC)
		meta.Discovery = &result
		metaJSON := jobs.MarshalJSON(meta)
		_ = jm.Transition(req.JobID, jobs.Intent{
			Status:   &status,
			Phase:    &phase,
			Summary:  &finalSummary,
			LogsText: &logs,
			MetaJSON: &metaJSON,
		})
	}
}

func (s *Service) refreshFactsAfterScheduledScan(req ScheduledScanRunRequest, session HostMaintenanceSession, logs string) string {
	deps := s.EnsureDeps()
	facts := session.CollectServerFacts(context.Background())
	meta := map[string]any{
		"source":       scheduledScanFactsRefreshSource,
		"job_id":       req.JobID,
		"run_id":       req.RunID,
		"collected_at": facts.CollectedAt,
		"disk_status":  facts.DiskStatus,
		"apt_status":   facts.AptStatus,
	}
	if err := deps.SaveServerFacts(facts); err != nil {
		meta["error"] = err.Error()
		deps.Logf("failed to persist scheduled host facts for %q: %v", req.Server.Name, err)
		deps.AuditWithActor("system", "", "server.facts.refresh", "server", req.Server.Name, "failure", "Scheduled host facts refresh failed", meta)
		return logs + "\nScheduled host facts refresh failed; the scan result remains valid."
	}
	if !health.FactsHealthComplete(facts) {
		deps.AuditWithActor("system", "", "server.facts.refresh", "server", req.Server.Name, "warning", "Scheduled host facts refresh returned incomplete health data", meta)
		return logs + "\nScheduled host facts refreshed with incomplete health data."
	}
	deps.AuditWithActor("system", "", "server.facts.refresh", "server", req.Server.Name, "success", "Scheduled host facts refreshed", meta)
	return logs + "\nScheduled host facts refreshed."
}
