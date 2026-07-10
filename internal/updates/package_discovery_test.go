package updates

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"

	"golang.org/x/crypto/ssh"
)

type packageDiscoveryCommandResponse struct {
	stdout string
	stderr string
	err    error
}

type packageDiscoveryCommandRunner struct {
	responses map[string]packageDiscoveryCommandResponse
	commands  []string
}

func (r *packageDiscoveryCommandRunner) run(_ SSHConnection, cmd string, _ io.Reader, _ time.Duration) (string, string, error) {
	r.commands = append(r.commands, cmd)
	if r.responses == nil {
		return "", "", nil
	}
	response := r.responses[cmd]
	return response.stdout, response.stderr, response.err
}

func TestDiscoverPackageUpdatesRunsDiscoveryCommandsInOrder(t *testing.T) {
	summaryStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following packages will be upgraded:",
		"  openssl bash",
		"2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	metadataStdout := strings.Join([]string{
		"Listing...",
		"bash/stable 5.2.15-2+b8 amd64 [upgradable from: 5.2.15-2+b7]",
		"openssl/stable-security 3.0.17-1~deb12u2 amd64 [upgradable from: 3.0.16-1~deb12u1]",
	}, "\n")
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stdout: summaryStdout},
			AptFullUpgradeSimCmd: {stdout: summaryStdout},
			AptListMetadataCmd:   {stdout: metadataStdout},
		},
	}

	result, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err != nil {
		t.Fatalf("DiscoverPackageUpdates() error = %v", err)
	}
	if len(result.PendingUpdates) != 2 || len(result.Upgradable) != 2 {
		t.Fatalf("result counts = pending %d upgradable %d, want 2/2", len(result.PendingUpdates), len(result.Upgradable))
	}
	if result.PendingPackageCount != 2 || result.SecurityPackageCount != 1 {
		t.Fatalf("derived counts = pending %d security %d, want 2/1", result.PendingPackageCount, result.SecurityPackageCount)
	}
	if result.Empty() {
		t.Fatal("result.Empty() = true, want packages available")
	}
	if result.PendingUpdates[0].Package != "openssl" || result.PendingUpdates[0].Source != "stable-security" || !result.PendingUpdates[0].Security {
		t.Fatalf("first pending update = %+v, want security metadata for openssl", result.PendingUpdates[0])
	}
	if result.PendingUpdates[0].CVEState != "pending" || result.PendingUpdates[1].CVEState != "pending" {
		t.Fatalf("CVE states = %q/%q, want pending/pending", result.PendingUpdates[0].CVEState, result.PendingUpdates[1].CVEState)
	}
	wantCommands := []string{AptListUpgradableCmd, AptFullUpgradeSimCmd, AptListMetadataCmd}
	if !reflect.DeepEqual(runner.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, wantCommands)
	}
	if (PackageDiscoveryOutcome{Upgradable: []string{"openssl"}}).Empty() {
		t.Fatal("PackageDiscoveryOutcome.Empty() ignored available package selectors")
	}
}

func TestDiscoverPackageUpdatesReturnsSuccessfulEmptyOutcome(t *testing.T) {
	runner := &packageDiscoveryCommandRunner{}

	outcome, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err != nil {
		t.Fatalf("DiscoverPackageUpdates() error = %v", err)
	}
	if !outcome.Empty() {
		t.Fatalf("outcome = %+v, want successful empty discovery", outcome)
	}
	if outcome.PendingPackageCount != 0 || outcome.SecurityPackageCount != 0 {
		t.Fatalf("counts = pending %d security %d, want 0/0", outcome.PendingPackageCount, outcome.SecurityPackageCount)
	}
	wantCommands := []string{AptListUpgradableCmd, AptFullUpgradeSimCmd, AptListMetadataCmd}
	if !reflect.DeepEqual(runner.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, wantCommands)
	}
}

func TestPackageDiscoveryOutcomeCloneOwnsNestedSlices(t *testing.T) {
	original := PackageDiscoveryOutcome{
		PendingPackageCount:  1,
		SecurityPackageCount: 1,
		Upgradable:           []string{"openssl"},
		PendingUpdates: []servers.PendingUpdate{{
			Package:  "openssl",
			Security: true,
			CVEs:     []string{"CVE-2026-0001"},
		}},
		UpgradePlan: servers.UpgradePlan{
			FullUpgradeNewPackages: []string{"linux-image"},
		},
	}

	cloned := original.Clone()
	cloned.Upgradable[0] = "bash"
	cloned.PendingUpdates[0].Package = "bash"
	cloned.PendingUpdates[0].CVEs[0] = "CVE-2026-9999"
	cloned.UpgradePlan.FullUpgradeNewPackages[0] = "mutated"

	if original.Upgradable[0] != "openssl" || original.PendingUpdates[0].Package != "openssl" {
		t.Fatalf("original package slices mutated: %+v", original)
	}
	if original.PendingUpdates[0].CVEs[0] != "CVE-2026-0001" {
		t.Fatalf("original CVEs mutated: %+v", original.PendingUpdates[0].CVEs)
	}
	if original.UpgradePlan.FullUpgradeNewPackages[0] != "linux-image" {
		t.Fatalf("original upgrade plan mutated: %+v", original.UpgradePlan)
	}
}

func TestDiscoverPackageUpdatesFallsBackWhenMetadataCommandFails(t *testing.T) {
	summaryStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following packages will be upgraded:",
		"  openssl bash",
		"2 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stdout: summaryStdout},
			AptFullUpgradeSimCmd: {stdout: summaryStdout},
			AptListMetadataCmd:   {err: errors.New("apt list failed")},
		},
	}

	result, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err != nil {
		t.Fatalf("DiscoverPackageUpdates() error = %v", err)
	}
	if got := PackageNamesFromPendingUpdates(result.PendingUpdates); !reflect.DeepEqual(got, []string{"bash", "openssl"}) {
		t.Fatalf("PackageNamesFromPendingUpdates() = %#v, want summary fallback packages", got)
	}
	if !reflect.DeepEqual(result.Upgradable, []string{"openssl", "bash"}) {
		t.Fatalf("upgradable = %#v, want summary fallback list", result.Upgradable)
	}
	wantCommands := []string{AptListUpgradableCmd, AptFullUpgradeSimCmd, AptListMetadataCmd}
	if !reflect.DeepEqual(runner.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, wantCommands)
	}
}

func TestDiscoverPackageUpdatesMarksFullUpgradePlanUnavailableWhenSimulationFails(t *testing.T) {
	standardStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following packages will be upgraded:",
		"  openssl",
		"1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	metadataStdout := "openssl/oldstable-security 3.0.17-1 amd64 [upgradable from: 3.0.16-1]"
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stdout: standardStdout},
			AptFullUpgradeSimCmd: {stderr: "E: dpkg was interrupted", err: errors.New("exit status 100")},
			AptListMetadataCmd:   {stdout: metadataStdout},
		},
	}

	result, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err != nil {
		t.Fatalf("DiscoverPackageUpdates() error = %v", err)
	}
	if result.UpgradePlan.FullUpgradePlanAvailable {
		t.Fatalf("upgrade plan = %+v, should mark failed full-upgrade simulation unavailable", result.UpgradePlan)
	}
	if len(result.UpgradePlan.FullUpgradeNewPackages) != 0 || len(result.UpgradePlan.FullUpgradeRemovedPackages) != 0 {
		t.Fatalf("upgrade plan = %+v, should not infer impact from failed simulation", result.UpgradePlan)
	}
}

func TestDiscoverPackageUpdatesEnrichesKeptBackSecurityPlanWhenNeeded(t *testing.T) {
	standardStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following packages will be upgraded:",
		"  apache2-utils linux-base rsync",
		"3 upgraded, 0 newly installed, 0 to remove and 1 not upgraded.",
	}, "\n")
	metadataStdout := strings.Join([]string{
		"Listing...",
		"apache2-utils/oldstable-proposed-updates 2.4.68-1~deb12u1 amd64 [upgradable from: 2.4.67-1~deb12u3]",
		"linux-base/oldstable-proposed-updates,oldstable-security 4.12.1~deb12u1 all [upgradable from: 4.9]",
		"linux-image-amd64/oldstable-proposed-updates,oldstable-security 6.1.174-1 amd64 [upgradable from: 6.1.159-1]",
		"rsync/oldstable-proposed-updates 3.2.7-1+deb12u6 amd64 [upgradable from: 3.2.7-1+deb12u5]",
	}, "\n")
	fullStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following NEW packages will be installed:",
		"  linux-image-6.1.0-49-amd64",
		"The following packages will be upgraded:",
		"  apache2-utils linux-base linux-image-amd64 rsync",
		"4 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	keptBackSecurityCmd := BuildSelectedInstallSimulationCmd([]string{"linux-image-amd64:amd64"})
	keptBackSecurityStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following NEW packages will be installed:",
		"  linux-image-6.1.0-49-amd64",
		"The following packages will be upgraded:",
		"  linux-image-amd64",
		"1 upgraded, 1 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stdout: standardStdout},
			AptFullUpgradeSimCmd: {stdout: fullStdout},
			AptListMetadataCmd:   {stdout: metadataStdout},
			keptBackSecurityCmd:  {stdout: keptBackSecurityStdout},
		},
	}

	result, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err != nil {
		t.Fatalf("DiscoverPackageUpdates() error = %v", err)
	}
	if !result.UpgradePlan.KeptBackSecurityPlanAvailable || result.UpgradePlan.KeptBackSecurityPackageCount != 1 {
		t.Fatalf("kept-back security plan = %+v, want targeted plan", result.UpgradePlan)
	}
	if !reflect.DeepEqual(result.UpgradePlan.KeptBackSecurityNewPackages, []string{"linux-image-6.1.0-49-amd64"}) {
		t.Fatalf("kept-back new packages = %#v, want kernel image", result.UpgradePlan.KeptBackSecurityNewPackages)
	}
	wantCommands := []string{AptListUpgradableCmd, AptFullUpgradeSimCmd, AptListMetadataCmd, keptBackSecurityCmd}
	if !reflect.DeepEqual(runner.commands, wantCommands) {
		t.Fatalf("commands = %#v, want %#v", runner.commands, wantCommands)
	}
}

func TestDiscoverPackageUpdatesFailsWhenStandardSimulationFails(t *testing.T) {
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stderr: "temporary unavailable", err: errors.New("exit status 100")},
		},
	}

	_, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, runner.run)
	if err == nil {
		t.Fatal("DiscoverPackageUpdates() error = nil, want failure")
	}
	if got := runner.commands; !reflect.DeepEqual(got, []string{AptListUpgradableCmd}) {
		t.Fatalf("commands = %#v, want standard simulation only", got)
	}
}

func TestDiscoverPackageUpdatesRejectsMissingCommandRunner(t *testing.T) {
	_, err := DiscoverPackageUpdates(fakeConnection{}, time.Second, nil)
	if err == nil {
		t.Fatal("DiscoverPackageUpdates() error = nil, want missing runner error")
	}
}

func TestProductionHostMaintenanceSessionUsesPackageDiscovery(t *testing.T) {
	summaryStdout := strings.Join([]string{
		"Reading package lists... Done",
		"The following packages will be upgraded:",
		"  openssl",
		"1 upgraded, 0 newly installed, 0 to remove and 0 not upgraded.",
	}, "\n")
	runner := &packageDiscoveryCommandRunner{
		responses: map[string]packageDiscoveryCommandResponse{
			AptListUpgradableCmd: {stdout: summaryStdout},
			AptFullUpgradeSimCmd: {stdout: summaryStdout},
			AptListMetadataCmd:   {err: errors.New("metadata unavailable")},
		},
	}

	factory := NewProductionHostMaintenanceSessionFactory(ProductionHostMaintenanceSessionDeps{
		BuildAuthMethods: func(servers.Server) ([]ssh.AuthMethod, error) { return nil, nil },
		HostKeyCallback:  func() (ssh.HostKeyCallback, error) { return ssh.InsecureIgnoreHostKey(), nil },
		DialSSH: func(servers.Server, *ssh.ClientConfig) (SSHConnection, error) {
			return fakeConnection{}, nil
		},
		RunCommandWithTimeout: runner.run,
		DiscoverPackages: func(conn SSHConnection, timeout time.Duration) (PackageDiscoveryOutcome, error) {
			return DiscoverPackageUpdates(conn, timeout, runner.run)
		},
	})
	session, err := factory.Open(context.Background(), HostMaintenanceSessionRequest{Server: servers.Server{User: "root"}, RetryPolicy: RetryPolicy{MaxAttempts: 1}, CommandTimeout: time.Second})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	result, err := session.DiscoverPackages(context.Background(), HostOperationRequest{Operation: "test.discovery"})
	if err != nil {
		t.Fatalf("DiscoverPackages() error = %v", err)
	}
	outcome := result.Outcome
	if got := PackageNamesFromPendingUpdates(outcome.PendingUpdates); !reflect.DeepEqual(got, []string{"openssl"}) {
		t.Fatalf("PackageNamesFromPendingUpdates() = %#v, want openssl", got)
	}
	if !reflect.DeepEqual(outcome.Upgradable, []string{"openssl"}) {
		t.Fatalf("upgradable = %#v, want openssl", outcome.Upgradable)
	}
	if !outcome.UpgradePlan.FullUpgradePlanAvailable {
		t.Fatalf("upgrade plan = %+v, want full-upgrade plan available", outcome.UpgradePlan)
	}
}
