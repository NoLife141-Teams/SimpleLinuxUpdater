package updates

import (
	"errors"
	"io"
	"time"

	"debian-updater/internal/servers"
)

type PackageDiscoveryOutcome struct {
	PendingPackageCount  int                     `json:"pending_package_count"`
	SecurityPackageCount int                     `json:"security_package_count"`
	Upgradable           []string                `json:"upgradable"`
	PendingUpdates       []servers.PendingUpdate `json:"pending_updates"`
	UpgradePlan          servers.UpgradePlan     `json:"upgrade_plan"`
}

type PackageDiscoveryCommandRunner func(SSHConnection, string, io.Reader, time.Duration) (string, string, error)
type PackageDiscoverer func(SSHConnection, time.Duration) (PackageDiscoveryOutcome, error)

func (o PackageDiscoveryOutcome) Empty() bool {
	return len(o.Upgradable) == 0
}

func (o PackageDiscoveryOutcome) Clone() PackageDiscoveryOutcome {
	o.Upgradable = append([]string(nil), o.Upgradable...)
	o.PendingUpdates = servers.ClonePendingUpdates(o.PendingUpdates)
	o.UpgradePlan = servers.CloneUpgradePlan(o.UpgradePlan)
	return o
}

func DiscoverPackageUpdates(client SSHConnection, timeout time.Duration, run PackageDiscoveryCommandRunner) (PackageDiscoveryOutcome, error) {
	if run == nil {
		return PackageDiscoveryOutcome{}, errors.New("package discovery command runner is nil")
	}
	stdout, stderr, err := run(client, AptListUpgradableCmd, nil, timeout)
	if err != nil {
		return PackageDiscoveryOutcome{}, MarkRetryableFromOutput(err, stdout+"\n"+stderr)
	}
	standardPending, standardUpgradable, err := ParseUpgradableEntries(stdout)
	if err != nil {
		return PackageDiscoveryOutcome{}, err
	}

	fullUpgradeStdout, _, fullUpgradeErr := run(client, AptFullUpgradeSimCmd, nil, timeout)
	fullUpgradePlanAvailable := fullUpgradeErr == nil
	metadataStdout, _, metadataErr := run(client, AptListMetadataCmd, nil, timeout)
	if metadataErr != nil {
		pending, upgradable, plan := MergeAvailableUpdatesWithStandard(standardPending, standardUpgradable, nil, nil, fullUpgradeStdout, fullUpgradePlanAvailable)
		plan = enrichKeptBackSecurityUpgradePlan(client, timeout, run, pending, plan)
		return newPackageDiscoveryOutcome(pending, upgradable, plan), nil
	}

	metadataPending, metadataUpgradable := ParseAptListMetadataEntries(metadataStdout, nil)
	pending, upgradable, plan := MergeAvailableUpdatesWithStandard(standardPending, standardUpgradable, metadataPending, metadataUpgradable, fullUpgradeStdout, fullUpgradePlanAvailable)
	plan = enrichKeptBackSecurityUpgradePlan(client, timeout, run, pending, plan)
	return newPackageDiscoveryOutcome(pending, upgradable, plan), nil
}

func newPackageDiscoveryOutcome(pending []servers.PendingUpdate, upgradable []string, plan servers.UpgradePlan) PackageDiscoveryOutcome {
	prepared := PreparePendingUpdatesForCVE(pending)
	securityCount := 0
	for _, update := range prepared {
		if update.Security {
			securityCount++
		}
	}
	return PackageDiscoveryOutcome{
		PendingPackageCount:  len(upgradable),
		SecurityPackageCount: securityCount,
		Upgradable:           append([]string(nil), upgradable...),
		PendingUpdates:       prepared,
		UpgradePlan:          servers.CloneUpgradePlan(plan),
	}
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
