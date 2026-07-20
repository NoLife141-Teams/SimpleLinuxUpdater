package updates

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"debian-updater/internal/health"
)

const (
	inspectionOutputMaxLen            = 240
	precheckMinFreeKB                 = int64(1024 * 1024)
	precheckDiskSpaceCmd              = "df -Pk /var / | awk 'NR>1 {print $2, $4}'"
	postcheckFailedUnitsCmd           = "systemctl --failed --no-legend --plain"
	postcheckRebootCmd                = "sh -c \"if [ -f /var/run/reboot-required ]; then echo required; fi\""
	hostFactsOSCmd                    = "sh -c '. /etc/os-release 2>/dev/null; printf \"%s\\n\" \"${PRETTY_NAME:-unknown}\"'"
	hostFactsRunningKernelCmd         = "uname -r"
	hostFactsLatestInstalledKernelCmd = "sh -c \"find /boot -maxdepth 1 -type f -name 'vmlinuz-*' -print 2>/dev/null | sed 's#^.*/vmlinuz-##' | sort -V | tail -n 1\""
	hostFactsUptimeCmd                = "cat /proc/uptime"
)

var (
	precheckLocksCmd     = RootOrSudoCommand("/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock")
	precheckDpkgAuditCmd = RootOrSudoCommand("dpkg --audit")
	precheckAptCheckCmd  = RootOrSudoCommand("apt-get check")
	rebootCheckErrorRe   = regexp.MustCompile(`\b(error|failed|failure|unable|cannot|can't)\b`)
	rebootRequiredRe     = regexp.MustCompile(`\b(reboot required|requires reboot|restart required|system restart required|needs reboot|need reboot)\b`)
)

func (s *productionHostMaintenanceSession) runInspectionCommand(ctx context.Context, command string) (string, string, error) {
	if err := ctx.Err(); err != nil {
		return "", "", err
	}
	if s.deps.RunCommand == nil {
		return "", "", fmt.Errorf("host command runner is not configured")
	}
	return s.deps.RunCommand(ctx, s.conn, command, nil, s.request.CommandTimeout)
}

func (s *productionHostMaintenanceSession) runUpdatePrechecks(ctx context.Context) PrecheckSummary {
	checks := []func(context.Context) PrecheckResult{
		s.checkDiskSpace,
		s.checkAptLocks,
		func(ctx context.Context) PrecheckResult { return s.runAptHealthCheck(ctx, "apt_health") },
	}
	summary := PrecheckSummary{AllPassed: true, Results: make([]PrecheckResult, 0, len(checks))}
	for _, check := range checks {
		result := check(ctx)
		summary.Results = append(summary.Results, result)
		if !result.Passed {
			summary.AllPassed = false
			summary.FailedCheck = result.Name
			return summary
		}
	}
	return summary
}

func (s *productionHostMaintenanceSession) checkDiskSpace(ctx context.Context) PrecheckResult {
	stdout, stderr, err := s.runInspectionCommand(ctx, precheckDiskSpaceCmd)
	return diskSpaceCheckResult(stdout, stderr, err)
}

func diskSpaceCheckResult(stdout, stderr string, err error) PrecheckResult {
	output := compactInspectionOutput(stdout, stderr)
	if err != nil {
		return PrecheckResult{Name: "disk_space", Details: boundedInspectionDetail("Failed to read free disk space: %v", err), Output: output}
	}
	fields := strings.Fields(stdout)
	if len(fields) == 0 {
		return PrecheckResult{Name: "disk_space", Details: "Could not parse free disk space output.", Output: output}
	}
	minFreeKB := int64(-1)
	for _, field := range fields {
		value, convErr := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if convErr != nil {
			return PrecheckResult{Name: "disk_space", Details: boundedInspectionDetail("Invalid free space value %q.", field), Output: output}
		}
		if minFreeKB == -1 || value < minFreeKB {
			minFreeKB = value
		}
	}
	if minFreeKB < precheckMinFreeKB {
		return PrecheckResult{Name: "disk_space", Details: boundedInspectionDetail("Insufficient disk space: %.2f GiB free (minimum %.2f GiB).", inspectionKBToGiB(minFreeKB), inspectionKBToGiB(precheckMinFreeKB))}
	}
	return PrecheckResult{Name: "disk_space", Passed: true, Details: boundedInspectionDetail("Disk space OK: %.2f GiB free (minimum %.2f GiB).", inspectionKBToGiB(minFreeKB), inspectionKBToGiB(precheckMinFreeKB))}
}

func (s *productionHostMaintenanceSession) checkAptLocks(ctx context.Context) PrecheckResult {
	stdout, stderr, err := s.runInspectionCommand(ctx, precheckLocksCmd)
	output := compactInspectionOutput(stdout, stderr)
	if err == nil {
		return PrecheckResult{Name: "apt_locks", Details: "APT/DPKG lock files are currently in use.", Output: output}
	}
	combined := output + "\n" + err.Error()
	if inspectionSudoPolicyError(combined) {
		return PrecheckResult{Name: "apt_locks", Details: "Lock pre-check requires passwordless sudo for `/usr/bin/fuser`. Click \"Enable passwordless apt\" for this server, then retry.", Output: output}
	}
	if inspectionSudoMissing(combined) {
		return PrecheckResult{Name: "apt_locks", Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.", Output: output}
	}
	if inspectionFuserMissing(combined) {
		return PrecheckResult{Name: "apt_locks", Details: "Lock check command not found. Install package `psmisc` (provides /usr/bin/fuser).", Output: output}
	}
	if exitCode, ok := SSHExitCode(err); ok && exitCode == 1 {
		if !inspectionBenignNoLockOutput(output) {
			return PrecheckResult{Name: "apt_locks", Details: "Could not determine apt/dpkg lock state from lock check output.", Output: output}
		}
		return PrecheckResult{Name: "apt_locks", Passed: true, Details: "No apt/dpkg lock contention detected.", Output: output}
	}
	if exitCode, ok := SSHExitCode(err); ok && exitCode == 127 {
		return PrecheckResult{Name: "apt_locks", Details: "Lock check command failed because a required command was not found. Install `sudo` for non-root users or `psmisc` for `/usr/bin/fuser`.", Output: output}
	}
	return PrecheckResult{Name: "apt_locks", Details: boundedInspectionDetail("Failed to evaluate apt/dpkg lock state: %v", err), Output: output}
}

func (s *productionHostMaintenanceSession) runAptHealthCheck(ctx context.Context, checkName string) PrecheckResult {
	dpkgStdout, dpkgStderr, dpkgErr := s.runInspectionCommand(ctx, precheckDpkgAuditCmd)
	dpkgOutput := compactInspectionOutput(dpkgStdout, dpkgStderr)
	if dpkgErr != nil {
		combined := dpkgOutput + "\n" + dpkgErr.Error()
		if inspectionSudoPolicyError(combined) {
			return PrecheckResult{Name: checkName, Details: "APT health pre-check requires passwordless sudo for `/usr/bin/dpkg`. Click \"Enable passwordless apt\" for this server, then retry.", Output: dpkgOutput}
		}
		if inspectionSudoMissing(combined) {
			return PrecheckResult{Name: checkName, Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.", Output: dpkgOutput}
		}
		return PrecheckResult{Name: checkName, Details: boundedInspectionDetail("dpkg audit failed: %v", dpkgErr), Output: dpkgOutput}
	}
	if strings.TrimSpace(dpkgStdout+dpkgStderr) != "" {
		return PrecheckResult{Name: checkName, Details: "dpkg audit reported package state issues.", Output: dpkgOutput}
	}
	aptStdout, aptStderr, aptErr := s.runInspectionCommand(ctx, precheckAptCheckCmd)
	aptOutput := compactInspectionOutput(aptStdout, aptStderr)
	if aptErr != nil {
		combined := aptOutput + "\n" + aptErr.Error()
		if inspectionSudoPolicyError(combined) {
			return PrecheckResult{Name: checkName, Details: "APT health pre-check requires passwordless sudo for `/usr/bin/apt-get`. Click \"Enable passwordless apt\" for this server, then retry.", Output: aptOutput}
		}
		if inspectionSudoMissing(combined) {
			return PrecheckResult{Name: checkName, Details: "Remote user is not root and `sudo` is not installed. Install `sudo` on the host or connect as root, then retry.", Output: aptOutput}
		}
		return PrecheckResult{Name: checkName, Details: boundedInspectionDetail("apt-get check failed: %v", aptErr), Output: aptOutput}
	}
	return PrecheckResult{Name: checkName, Passed: true, Details: "APT health checks passed.", Output: compactInspectionOutput(dpkgOutput, aptOutput)}
}

func (s *productionHostMaintenanceSession) listFailedSystemdUnits(ctx context.Context) ([]string, string, error) {
	stdout, stderr, err := s.runInspectionCommand(ctx, postcheckFailedUnitsCmd)
	output := compactInspectionOutput(stdout, stderr)
	if err != nil {
		return nil, output, err
	}
	return ParseFailedSystemdUnits(stdout), output, nil
}

func (s *productionHostMaintenanceSession) runPostUpdateHealthChecks(ctx context.Context, cfg PostUpdateCheckConfig, baseline map[string]struct{}) PostcheckSummary {
	summary := PostcheckSummary{AllPassed: true, Results: make([]PrecheckResult, 0, 4)}
	if !cfg.Enabled {
		return summary
	}
	checks := []func(context.Context) PrecheckResult{
		func(ctx context.Context) PrecheckResult {
			result := s.runAptHealthCheck(ctx, PostcheckNameAptHealth)
			result.Details = strings.Replace(result.Details, "pre-check", "post-check", 1)
			return result
		},
		func(ctx context.Context) PrecheckResult { return s.checkFailedSystemdUnits(ctx, baseline) },
	}
	if cfg.RebootRequiredWarning {
		checks = append(checks, s.checkRebootRequired)
	}
	for _, check := range checks {
		result := check(ctx)
		summary.Results = append(summary.Results, result)
		if result.Passed {
			continue
		}
		if IsPostcheckFailureBlocking(result.Name, cfg) {
			summary.AllPassed = false
			if summary.FailedCheck == "" {
				summary.FailedCheck = result.Name
			}
			continue
		}
		summary.Warnings++
	}
	if strings.TrimSpace(cfg.CustomCommand) != "" {
		result := s.checkCustomPostUpdateCommand(ctx, cfg.CustomCommand)
		summary.Results = append(summary.Results, result)
		if !result.Passed {
			summary.AllPassed = false
			if summary.FailedCheck == "" {
				summary.FailedCheck = result.Name
			}
		}
	}
	return summary
}

func (s *productionHostMaintenanceSession) checkFailedSystemdUnits(ctx context.Context, baseline map[string]struct{}) PrecheckResult {
	units, output, err := s.listFailedSystemdUnits(ctx)
	if err != nil {
		return PrecheckResult{Name: PostcheckNameFailedUnits, Details: boundedInspectionDetail("failed to evaluate systemd unit health: %v", err), Output: output}
	}
	if len(units) == 0 {
		return PrecheckResult{Name: PostcheckNameFailedUnits, Passed: true, Details: "No failed systemd units detected."}
	}
	newlyFailed := make([]string, 0, len(units))
	for _, unit := range units {
		if _, existedBefore := baseline[unit]; !existedBefore {
			newlyFailed = append(newlyFailed, unit)
		}
	}
	if len(newlyFailed) == 0 {
		return PrecheckResult{Name: PostcheckNameFailedUnits, Passed: true, Details: boundedInspectionDetail("No new failed systemd units detected after upgrade (%d pre-existing).", len(units)), Output: output}
	}
	fullOutput := strings.Join(newlyFailed, "\n")
	if trimmed := strings.TrimSpace(output); trimmed != "" {
		fullOutput += "\n\n" + trimmed
	}
	return PrecheckResult{Name: PostcheckNameFailedUnits, Details: "systemd reports newly failed units after upgrade.", Output: truncateInspection(fullOutput)}
}

func (s *productionHostMaintenanceSession) checkRebootRequired(ctx context.Context) PrecheckResult {
	stdout, stderr, err := s.runInspectionCommand(ctx, postcheckRebootCmd)
	output := compactInspectionOutput(stdout, stderr)
	if err != nil {
		return PrecheckResult{Name: PostcheckNameRebootNeeded, Details: boundedInspectionDetail("failed to evaluate reboot-required state: %v", err), Output: output, Error: truncateInspection(err.Error())}
	}
	if strings.Contains(strings.ToLower(strings.TrimSpace(stdout)), "required") {
		return PrecheckResult{Name: PostcheckNameRebootNeeded, Details: "Reboot required to fully apply updates.", Output: output}
	}
	return PrecheckResult{Name: PostcheckNameRebootNeeded, Passed: true, Details: "No reboot requirement detected."}
}

func (s *productionHostMaintenanceSession) checkCustomPostUpdateCommand(ctx context.Context, command string) PrecheckResult {
	stdout, stderr, err := s.runInspectionCommand(ctx, command)
	output := compactInspectionOutput(stdout, stderr)
	if err != nil {
		return PrecheckResult{Name: PostcheckNameCustomCmd, Details: boundedInspectionDetail("custom post-check command failed: %v", err), Output: output}
	}
	return PrecheckResult{Name: PostcheckNameCustomCmd, Passed: true, Details: "Custom post-check command passed.", Output: output}
}

func IsPostcheckFailureBlocking(name string, cfg PostUpdateCheckConfig) bool {
	switch name {
	case PostcheckNameAptHealth:
		return cfg.BlockOnAptHealth
	case PostcheckNameFailedUnits:
		return cfg.BlockOnFailedUnits
	case PostcheckNameRebootNeeded:
		return false
	case PostcheckNameCustomCmd:
		return strings.TrimSpace(cfg.CustomCommand) != ""
	default:
		return true
	}
}

func (s *productionHostMaintenanceSession) collectServerFacts(ctx context.Context) ServerFactsRecord {
	collector := health.Collector{Probe: func(ctx context.Context, kind health.ProbeKind) health.ProbeResult {
		switch kind {
		case health.ProbeOS:
			stdout, stderr, err := s.runInspectionCommand(ctx, hostFactsOSCmd)
			return health.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
		case health.ProbeRunningKernel:
			stdout, stderr, err := s.runInspectionCommand(ctx, hostFactsRunningKernelCmd)
			return health.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
		case health.ProbeLatestInstalledKernel:
			stdout, stderr, err := s.runInspectionCommand(ctx, hostFactsLatestInstalledKernelCmd)
			return health.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
		case health.ProbeUptime:
			stdout, stderr, err := s.runInspectionCommand(ctx, hostFactsUptimeCmd)
			return health.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
		case health.ProbeDisk:
			stdout, stderr, err := s.runInspectionCommand(ctx, precheckDiskSpaceCmd)
			check := diskSpaceCheckResult(stdout, stderr, err)
			result := health.ProbeResult{Output: stdout, Stderr: stderr, Status: inspectionHealthStatus(check), Details: check.Details, Err: err}
			if freeKB, totalKB, ok := inspectionDiskFreeTotalKB(stdout); ok {
				result.FreeKB, result.TotalKB = freeKB, totalKB
			} else if freeKB, ok := inspectionDiskFreeKB(stdout); ok {
				result.FreeKB = freeKB
			}
			return result
		case health.ProbeAPT:
			check := s.runAptHealthCheck(ctx, "apt_health")
			return health.ProbeResult{Status: inspectionHealthStatus(check), Details: check.Details}
		case health.ProbeReboot:
			check := s.checkRebootRequired(ctx)
			result := health.ProbeResult{Status: inspectionHealthStatus(check), Details: check.Details}
			if required, known := inspectionRebootRequired(check); known {
				result.RebootRequired = &required
			}
			return result
		default:
			return health.ProbeResult{}
		}
	}}
	return collector.Capture(ctx, s.request.Server.Name)
}

func inspectionDiskFreeKB(output string) (int64, bool) {
	var minFree int64
	found := false
	for _, field := range strings.Fields(output) {
		value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
		if err != nil || value < 0 {
			continue
		}
		if !found || value < minFree {
			minFree, found = value, true
		}
	}
	return minFree, found
}

func inspectionDiskFreeTotalKB(output string) (int64, int64, bool) {
	var minFree, totalForMin int64
	found := false
	for _, line := range strings.Split(output, "\n") {
		values := make([]int64, 0, 2)
		for _, field := range strings.Fields(line) {
			value, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err == nil && value >= 0 {
				values = append(values, value)
			}
		}
		if len(values) < 2 {
			continue
		}
		if !found || values[1] < minFree {
			totalForMin, minFree, found = values[0], values[1], true
		}
	}
	return minFree, totalForMin, found
}

func inspectionHealthStatus(result PrecheckResult) string {
	if result.Passed {
		return "ok"
	}
	return "critical"
}

func inspectionRebootRequired(result PrecheckResult) (bool, bool) {
	if strings.TrimSpace(result.Error) != "" {
		return false, false
	}
	if result.Passed {
		return false, true
	}
	text := strings.ToLower(result.Details + " " + result.Output)
	if rebootCheckErrorRe.MatchString(text) {
		return false, false
	}
	if rebootRequiredRe.MatchString(text) {
		return true, true
	}
	return false, true
}

func compactInspectionOutput(stdout, stderr string) string {
	combined := strings.TrimSpace(strings.TrimSpace(stdout) + "\n" + strings.TrimSpace(stderr))
	return truncateInspection(combined)
}

func truncateInspection(value string) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) > inspectionOutputMaxLen {
		return string(runes[:inspectionOutputMaxLen])
	}
	return string(runes)
}

func boundedInspectionDetail(format string, args ...any) string {
	return truncateInspection(fmt.Sprintf(format, args...))
}

func inspectionKBToGiB(kb int64) float64 { return float64(kb) / (1024.0 * 1024.0) }

func inspectionSudoPolicyError(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "a password is required") || strings.Contains(normalized, "not allowed to run sudo") || strings.Contains(normalized, "is not in the sudoers file")
}

func inspectionSudoMissing(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "sudo: command not found") || strings.Contains(normalized, "sudo: not found")
}

func inspectionFuserMissing(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(normalized, "/usr/bin/fuser: command not found") || strings.Contains(normalized, "/usr/bin/fuser: not found") || strings.Contains(normalized, "unable to execute /usr/bin/fuser") || strings.Contains(normalized, "sudo: /usr/bin/fuser: no such file or directory") || (strings.Contains(normalized, "command not found") && strings.Contains(normalized, "fuser"))
}

func inspectionBenignNoLockOutput(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" || strings.Contains(normalized, "no process found") {
		return true
	}
	lockPathMentioned := strings.Contains(normalized, "/var/lib/dpkg/lock-frontend") || strings.Contains(normalized, "/var/lib/dpkg/lock") || strings.Contains(normalized, "/var/cache/apt/archives/lock")
	return lockPathMentioned && (strings.Contains(normalized, "does not exist") || strings.Contains(normalized, "no such file or directory"))
}
