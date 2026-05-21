package updates

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"debian-updater/internal/servers"
)

func TestParseUpgradableEntriesAndPackageSelection(t *testing.T) {
	stdout := strings.Join([]string{
		"NOTE: noise",
		"Inst openssl [3.0.1] (3.0.2 Ubuntu:22.04/jammy-security [amd64])",
		"Inst curl [7.1] (7.2 Ubuntu:22.04/jammy-updates [amd64])",
	}, "\n")
	pending, upgradable, err := ParseUpgradableEntries(stdout)
	if err != nil {
		t.Fatalf("ParseUpgradableEntries() error = %v", err)
	}
	if len(upgradable) != 2 {
		t.Fatalf("upgradable count = %d, want 2", len(upgradable))
	}
	if pending[0].Package != "openssl" || !pending[0].Security || pending[0].CurrentVersion != "3.0.1" || pending[0].CandidateVersion != "3.0.2" {
		t.Fatalf("first pending update = %+v, want parsed security openssl", pending[0])
	}
	if got := SecurityPackagesFromPendingUpdates(pending); !reflect.DeepEqual(got, []string{"openssl"}) {
		t.Fatalf("SecurityPackagesFromPendingUpdates() = %#v, want openssl", got)
	}
}

func TestBuildSelectedUpgradeCmdEscapesPackages(t *testing.T) {
	got := BuildSelectedUpgradeCmd([]string{"openssl", "libfoo'bar"})
	want := RootOrSudoCommand(`apt-get -y install --only-upgrade -- 'openssl' 'libfoo'"'"'bar'`)
	if got != want {
		t.Fatalf("BuildSelectedUpgradeCmd() = %q, want %q", got, want)
	}
}

func TestRootOrSudoCommand(t *testing.T) {
	got := RootOrSudoCommand("apt-get update")
	want := `if [ "$(id -u)" -eq 0 ]; then apt-get update; else sudo -n apt-get update; fi`
	if got != want {
		t.Fatalf("RootOrSudoCommand() = %q, want %q", got, want)
	}
}

func TestRetryHelpersClassifyRetryableOutput(t *testing.T) {
	err := MarkRetryableFromOutput(errors.New("exit status 100"), "Could not get lock /var/lib/dpkg/lock-frontend")
	if !IsRetryableError(err) {
		t.Fatalf("MarkRetryableFromOutput() did not tag retryable lock output")
	}
	delay := ComputeRetryDelay(RetryPolicy{BaseDelay: time.Second, MaxDelay: 8 * time.Second, JitterPct: 0}, 3, 0)
	if delay != 4*time.Second {
		t.Fatalf("ComputeRetryDelay() = %s, want 4s", delay)
	}
}

func TestPreparePendingUpdatesForCVELimitsAndSorts(t *testing.T) {
	updates := make([]servers.PendingUpdate, 0, CVELookupMaxPackages+1)
	for i := 0; i < CVELookupMaxPackages+1; i++ {
		updates = append(updates, servers.PendingUpdate{Package: strings.Repeat("a", i+1), Security: i%2 == 0})
	}
	prepared := PreparePendingUpdatesForCVE(updates)
	if prepared[0].CVEState != "pending" {
		t.Fatalf("first CVE state = %q, want pending", prepared[0].CVEState)
	}
	if prepared[len(prepared)-1].CVEState != "skipped" {
		t.Fatalf("last CVE state = %q, want skipped", prepared[len(prepared)-1].CVEState)
	}
}
