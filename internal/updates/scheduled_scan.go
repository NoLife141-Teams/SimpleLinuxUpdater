package updates

import (
	"fmt"
	"strings"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"
	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

func BuildScheduledJobMeta(policy policies.Policy, scheduledForUTC string) ScheduledJobMeta {
	meta := ScheduledJobMeta{
		Trigger:                "scheduled",
		PolicyID:               policy.ID,
		PolicyName:             policy.Name,
		ScheduledFor:           scheduledForUTC,
		ExecutionMode:          policy.ExecutionMode,
		PackageScope:           policy.PackageScope,
		ApprovalTimeoutMinutes: policy.ApprovalTimeoutMinutes,
	}
	if policy.ExecutionMode == policies.ExecutionAutoApply {
		if policy.PackageScope == policies.PackageScopeSecurity {
			meta.AutoApproveScope = "security"
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
			finishedAt := deps.JobTimestampNow()
			errorClass := "permanent"
			_ = jm.UpdateJobWithoutRuntimeSync(req.JobID, jobs.Update{
				Status:     &status,
				Phase:      &jobPhase,
				Summary:    &summary,
				LogsText:   &logs,
				ErrorClass: &errorClass,
				FinishedAt: &finishedAt,
			})
		}
		runStatus := policies.RunFailed
		reason := "failed"
		finishedAt := deps.JobTimestampNow()
		_ = deps.UpdatePolicyRun(req.RunID, policies.RunUpdate{
			Status:     &runStatus,
			Reason:     &reason,
			Summary:    &summary,
			FinishedAt: &finishedAt,
		})
		meta := map[string]any{
			"policy_id":      req.Policy.ID,
			"policy_name":    req.Policy.Name,
			"execution_mode": req.Policy.ExecutionMode,
			"package_scope":  req.Policy.PackageScope,
		}
		if err != nil {
			meta["error"] = err.Error()
		}
		deps.AuditWithActor("system", "", "schedule.run.failed", "server", req.Server.Name, "failure", summary, meta)
	}

	authMethods, err := deps.BuildAuthMethods(req.Server)
	if err != nil {
		setFailure("Scheduled scan auth setup failed", err, jobs.PhaseDial, "")
		return
	}
	hostKeyCallback, err := deps.HostKeyCallback()
	if err != nil {
		setFailure("Scheduled scan host key setup failed", err, jobs.PhaseDial, "")
		return
	}
	config := &ssh.ClientConfig{
		User:            req.Server.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback,
		Timeout:         deps.SSHConnectTimeout,
	}
	client, err := deps.DialSSHWithRetry(req.Server, config, req.RetryPolicy, "scheduled_scan.ssh_dial", nil)
	if err != nil {
		setFailure("Scheduled scan SSH connection failed", err, jobs.PhaseDial, "")
		return
	}
	defer func() { _ = client.Close() }()

	logs := "Starting scheduled package scan..."
	if jm != nil {
		phase := jobs.PhasePrechecks
		summary := "Running pre-checks"
		_ = jm.UpdateJobWithoutRuntimeSync(req.JobID, jobs.Update{
			Phase:    &phase,
			Summary:  &summary,
			LogsText: &logs,
		})
	}
	precheckSummary := deps.RunUpdatePrechecks(client)
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
		_ = jm.UpdateJobWithoutRuntimeSync(req.JobID, jobs.Update{
			Phase:    &phase,
			Summary:  &summary,
			LogsText: &logs,
		})
	}
	var stdout, stderr string
	err = deps.RunSSHOperationWithRetry(
		req.Server,
		config,
		&client,
		req.RetryPolicy,
		"scheduled_scan.apt_update",
		"\napt update attempt %d/%d failed: %v; retrying in %s",
		new(int),
		func() error {
			var runErr error
			stdout, stderr, runErr = deps.RunSSHCommandWithTimeout(client, AptUpdateCmd, nil, deps.LoadCommandTimeout())
			return MarkRetryableFromOutput(runErr, stdout+"\n"+stderr)
		},
	)
	logs += "\n" + stdout + stderr
	if err != nil {
		setFailure("Scheduled scan apt update failed", err, jobs.PhaseAptUpdate, logs)
		return
	}

	var pendingUpdates []servers.PendingUpdate
	var upgradable []string
	err = deps.RunSSHOperationWithRetry(
		req.Server,
		config,
		&client,
		req.RetryPolicy,
		"scheduled_scan.list_upgradable",
		"\nlist upgradable attempt %d/%d failed: %v; retrying in %s",
		new(int),
		func() error {
			var listErr error
			pendingUpdates, upgradable, listErr = deps.GetUpgradable(client, deps.LoadCommandTimeout())
			return listErr
		},
	)
	if err != nil {
		setFailure("Scheduled scan package discovery failed", err, jobs.PhaseAptUpdate, logs)
		return
	}

	pendingUpdates = PreparePendingUpdatesForCVE(pendingUpdates)
	for i := range pendingUpdates {
		if pendingUpdates[i].CVEState != "pending" {
			continue
		}
		cves, lookupErr := deps.QueryPackageCVEs(client, pendingUpdates[i].Package)
		if lookupErr != nil {
			pendingUpdates[i].CVEState = "unavailable"
			pendingUpdates[i].CVEs = []string{}
			continue
		}
		pendingUpdates[i].CVEState = "ready"
		pendingUpdates[i].CVEs = append([]string(nil), cves...)
	}
	SortPendingUpdates(pendingUpdates)
	result := ScheduledJobDiscovery{
		PendingPackageCount:  len(upgradable),
		SecurityPackageCount: len(SecurityPackagesFromPendingUpdates(pendingUpdates)),
		Upgradable:           append([]string(nil), upgradable...),
		PendingUpdates:       servers.ClonePendingUpdates(pendingUpdates),
	}
	resultJSON := jobs.MarshalJSON(result)
	finalSummary := "Scheduled scan completed"
	if len(upgradable) == 0 {
		finalSummary = "Scheduled scan completed: no pending updates"
	}
	if jm != nil {
		status := jobs.StatusSucceeded
		phase := jobs.PhaseComplete
		meta := BuildScheduledJobMeta(req.Policy, req.ScheduledForUTC)
		meta.Discovery = &result
		metaJSON := jobs.MarshalJSON(meta)
		finishedAt := deps.JobTimestampNow()
		_ = jm.UpdateJobWithoutRuntimeSync(req.JobID, jobs.Update{
			Status:     &status,
			Phase:      &phase,
			Summary:    &finalSummary,
			LogsText:   &logs,
			MetaJSON:   &metaJSON,
			FinishedAt: &finishedAt,
		})
	}
	runStatus := policies.RunSucceeded
	finishedAt := deps.JobTimestampNow()
	_ = deps.UpdatePolicyRun(req.RunID, policies.RunUpdate{
		Status:     &runStatus,
		Summary:    &finalSummary,
		ResultJSON: &resultJSON,
		FinishedAt: &finishedAt,
	})
	deps.AuditWithActor("system", "", "schedule.run.completed", "server", req.Server.Name, "success", finalSummary, map[string]any{
		"policy_id":              req.Policy.ID,
		"policy_name":            req.Policy.Name,
		"pending_package_count":  result.PendingPackageCount,
		"security_package_count": result.SecurityPackageCount,
	})
}
