package updates

import (
	"fmt"
	"strings"

	"debian-updater/internal/jobs"
	"debian-updater/internal/policies"

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
			finishedAt := deps.JobTimestampNow()
			errorClass := "permanent"
			meta := BuildScheduledJobMeta(req.Policy, req.ScheduledForUTC)
			if err != nil {
				meta.Error = err.Error()
			}
			metaJSON := jobs.MarshalJSON(meta)
			_ = jm.UpdateJobWithoutRuntimeSync(req.JobID, jobs.Update{
				Status:     &status,
				Phase:      &jobPhase,
				Summary:    &summary,
				LogsText:   &logs,
				ErrorClass: &errorClass,
				MetaJSON:   &metaJSON,
				FinishedAt: &finishedAt,
			})
		}
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

	var discovery PackageDiscoveryOutcome
	err = deps.RunSSHOperationWithRetry(
		req.Server,
		config,
		&client,
		req.RetryPolicy,
		"scheduled_scan.list_upgradable",
		"\nlist upgradable attempt %d/%d failed: %v; retrying in %s",
		new(int),
		func() error {
			outcome, discoverErr := deps.DiscoverPackages(client, deps.LoadCommandTimeout())
			if discoverErr == nil {
				discovery = outcome
			}
			return discoverErr
		},
	)
	if err != nil {
		setFailure("Scheduled scan package discovery failed", err, jobs.PhaseAptUpdate, logs)
		return
	}

	for i := range discovery.PendingUpdates {
		if discovery.PendingUpdates[i].CVEState != "pending" {
			continue
		}
		cves, lookupErr := deps.QueryPackageCVEs(client, discovery.PendingUpdates[i].Package)
		if lookupErr != nil {
			discovery.PendingUpdates[i].CVEState = "unavailable"
			discovery.PendingUpdates[i].CVEs = []string{}
			continue
		}
		discovery.PendingUpdates[i].CVEState = "ready"
		discovery.PendingUpdates[i].CVEs = append([]string(nil), cves...)
	}
	SortPendingUpdates(discovery.PendingUpdates)
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
}
