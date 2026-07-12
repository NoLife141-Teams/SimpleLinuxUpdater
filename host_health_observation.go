package main

import (
	"context"
	"time"

	healthpkg "debian-updater/internal/health"
)

// collectServerFactsWithConnection adapts the SSH transport to the
// transport-neutral Host Health Observation collector.
func collectServerFactsWithConnection(server Server, client sshConnection, timeout time.Duration) serverFactsRecord {
	collector := healthpkg.Collector{
		Probe: func(_ context.Context, kind healthpkg.ProbeKind) healthpkg.ProbeResult {
			switch kind {
			case healthpkg.ProbeOS:
				stdout, stderr, err := runSSHCommandWithTimeout(client, serverFactsOSCmd, nil, timeout)
				return healthpkg.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
			case healthpkg.ProbeUptime:
				stdout, stderr, err := runSSHCommandWithTimeout(client, serverFactsUptimeCmd, nil, timeout)
				return healthpkg.ProbeResult{Output: stdout, Stderr: stderr, Err: err}
			case healthpkg.ProbeDisk:
				stdout, stderr, err := runSSHCommandWithTimeout(client, precheckDiskSpaceCmd, nil, timeout)
				check := checkDiskSpace(client)
				result := healthpkg.ProbeResult{Output: stdout, Stderr: stderr, Status: healthStatusFromResult(check), Details: check.Details, Err: err}
				if freeKB, totalKB, ok := diskFreeTotalKBFromOutput(stdout); ok {
					result.FreeKB, result.TotalKB = freeKB, totalKB
				} else if freeKB, ok := diskFreeKBFromOutput(stdout); ok {
					result.FreeKB = freeKB
				}
				return result
			case healthpkg.ProbeAPT:
				check := checkAptHealth(client)
				return healthpkg.ProbeResult{Status: healthStatusFromResult(check), Details: check.Details}
			case healthpkg.ProbeReboot:
				check := checkRebootRequired(client)
				result := healthpkg.ProbeResult{Status: healthStatusFromResult(check), Details: check.Details}
				if required, known := rebootResultRequiresRestart(check); known {
					result.RebootRequired = &required
				}
				return result
			default:
				return healthpkg.ProbeResult{}
			}
		},
	}
	return collector.Capture(context.Background(), server.Name)
}
