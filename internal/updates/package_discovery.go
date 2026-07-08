package updates

import (
	"errors"
	"io"
	"time"

	"debian-updater/internal/servers"
)

type PackageDiscoveryResult struct {
	PendingUpdates []servers.PendingUpdate
	Upgradable     []string
	UpgradePlan    servers.UpgradePlan
}

type PackageDiscoveryCommandRunner func(SSHConnection, string, io.Reader, time.Duration) (string, string, error)

func DiscoverPackageUpdates(client SSHConnection, timeout time.Duration, run PackageDiscoveryCommandRunner) (PackageDiscoveryResult, error) {
	if run == nil {
		return PackageDiscoveryResult{}, errors.New("package discovery command runner is nil")
	}
	stdout, stderr, err := run(client, AptListUpgradableCmd, nil, timeout)
	if err != nil {
		return PackageDiscoveryResult{}, MarkRetryableFromOutput(err, stdout+"\n"+stderr)
	}
	standardPending, standardUpgradable, err := ParseUpgradableEntries(stdout)
	if err != nil {
		return PackageDiscoveryResult{}, err
	}

	fullUpgradeStdout, _, fullUpgradeErr := run(client, AptFullUpgradeSimCmd, nil, timeout)
	fullUpgradePlanAvailable := fullUpgradeErr == nil
	metadataStdout, _, metadataErr := run(client, AptListMetadataCmd, nil, timeout)
	if metadataErr != nil {
		pending, upgradable, plan := MergeAvailableUpdatesWithStandard(standardPending, standardUpgradable, nil, nil, fullUpgradeStdout, fullUpgradePlanAvailable)
		plan = enrichKeptBackSecurityUpgradePlan(client, timeout, run, pending, plan)
		return PackageDiscoveryResult{
			PendingUpdates: servers.ClonePendingUpdates(pending),
			Upgradable:     append([]string(nil), upgradable...),
			UpgradePlan:    servers.CloneUpgradePlan(plan),
		}, nil
	}

	metadataPending, metadataUpgradable := ParseAptListMetadataEntries(metadataStdout, nil)
	pending, upgradable, plan := MergeAvailableUpdatesWithStandard(standardPending, standardUpgradable, metadataPending, metadataUpgradable, fullUpgradeStdout, fullUpgradePlanAvailable)
	plan = enrichKeptBackSecurityUpgradePlan(client, timeout, run, pending, plan)
	return PackageDiscoveryResult{
		PendingUpdates: servers.ClonePendingUpdates(pending),
		Upgradable:     append([]string(nil), upgradable...),
		UpgradePlan:    servers.CloneUpgradePlan(plan),
	}, nil
}

func enrichKeptBackSecurityUpgradePlan(client SSHConnection, timeout time.Duration, run PackageDiscoveryCommandRunner, pending []servers.PendingUpdate, plan servers.UpgradePlan) servers.UpgradePlan {
	packages := KeptBackSecurityPackagesFromPendingUpdates(pending)
	if len(packages) == 0 || run == nil {
		return plan
	}
	cmd := BuildSelectedInstallSimulationCmd(packages)
	if cmd == "" {
		return plan
	}
	stdout, stderr, err := run(client, cmd, nil, timeout)
	if err != nil {
		return plan
	}
	ApplyKeptBackSecuritySimulation(&plan, stdout+"\n"+stderr)
	return plan
}
