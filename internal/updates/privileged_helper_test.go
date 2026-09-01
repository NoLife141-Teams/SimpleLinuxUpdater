package updates

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedSudoersContentContainsOnlyTypedHelperOperations(t *testing.T) {
	content, err := ManagedSudoersContent("operator")
	if err != nil {
		t.Fatalf("ManagedSudoersContent() error = %v", err)
	}
	if !strings.HasPrefix(content, managedSudoersMarker+"\n") {
		t.Fatalf("managed sudoers content lacks owner marker: %q", content)
	}
	for _, forbidden := range []string{"/usr/bin/apt,", "/usr/bin/apt-get,", "/usr/bin/apt-get *", "/usr/bin/env ", "/bin/sh", " ALL=(ALL)"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("managed sudoers content contains forbidden generic grant %q: %q", forbidden, content)
		}
	}
	if !strings.Contains(content, "operator ALL=(root) NOPASSWD: "+RootHelperPath+" update") {
		t.Fatalf("managed sudoers content = %q, want typed helper grant", content)
	}
}

func TestPrivilegedMaintenanceCommandsUseTypedHelperForNonRoot(t *testing.T) {
	commands := map[string]string{
		"update":               AptUpdateCmd,
		"upgrade":              AptUpgradeCmd,
		"full-upgrade":         AptFullUpgradeCmd,
		"autoremove":           AptAutoremoveCmd,
		"repair":               AptRepairCmd,
		"lock-probe-extended":  AptExtendedLockProbeCmd,
		"dpkg-audit":           precheckDpkgAuditCmd,
		"apt-check":            precheckAptCheckCmd,
		"install":              BuildSelectedInstallCmd([]string{"libssl3:amd64"}),
		"install-only-upgrade": BuildSelectedUpgradeCmd([]string{"openssl"}),
	}
	for operation, command := range commands {
		if !strings.Contains(command, "else sudo -n "+RootHelperPath+" '"+operation+"'") {
			t.Errorf("%s command does not use its typed helper operation: %q", operation, command)
		}
		for _, forbidden := range []string{"else sudo -n /usr/bin/apt", "else sudo -n /usr/bin/apt-get", "else sudo -n /usr/bin/env", "else sudo -n /bin/sh"} {
			if strings.Contains(command, forbidden) {
				t.Errorf("%s command contains generic privileged execution %q: %q", operation, forbidden, command)
			}
		}
	}
	if !strings.Contains(ControlledRebootCmd, "sudo -n "+RootHelperPath+" reboot") {
		t.Fatalf("ControlledRebootCmd = %q, want typed helper reboot", ControlledRebootCmd)
	}
	if !strings.Contains(AptLockProbeCmd, "sudo -n /usr/bin/fuser /var/lib/dpkg/lock-frontend") || strings.Contains(AptLockProbeCmd, "*") {
		t.Fatalf("AptLockProbeCmd = %q, want exact legacy-only fuser fallback", AptLockProbeCmd)
	}
}

func TestManagedSudoersContentValidatesWithVisudo(t *testing.T) {
	visudo, err := exec.LookPath("visudo")
	if err != nil {
		t.Skip("visudo is not available")
	}
	for _, user := range []string{"operator", "Admin", "ops.admin", "ops-admin_2", "123", "-ops", ".ops"} {
		t.Run(user, func(t *testing.T) {
			content, err := ManagedSudoersContent(user)
			if err != nil {
				t.Fatalf("ManagedSudoersContent() error = %v", err)
			}
			path := filepath.Join(t.TempDir(), "simplelinuxupdater")
			if err := os.WriteFile(path, []byte(content), 0o440); err != nil {
				t.Fatalf("write sudoers fixture: %v", err)
			}
			if output, err := exec.Command(visudo, "-cf", path).CombinedOutput(); err != nil {
				t.Fatalf("visudo rejected managed rule: %v\n%s", err, output)
			}
		})
	}
}

func TestRootHelperClearsInheritedEnvironmentBeforeAptAndDpkg(t *testing.T) {
	script := RootHelperScript()
	if strings.Contains(script, "/usr/bin/env DEBIAN_FRONTEND=") {
		t.Fatal("root helper invokes package tools with an inherited environment")
	}
	if got := strings.Count(script, "/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin"); got != 14 {
		t.Fatalf("root helper clean-environment package invocations = %d, want 14", got)
	}
	for _, forbidden := range []string{"exec /usr/bin/dpkg --audit", "exec /usr/bin/apt-get check", "audit_output=$(/usr/bin/dpkg --audit"} {
		if strings.Contains(script, forbidden) {
			t.Errorf("root helper contains inherited-environment health check %q", forbidden)
		}
	}
}

func TestRootHelperVerifiesEverySelectorAgainstExactAptCacheNames(t *testing.T) {
	script := RootHelperScript()
	for _, required := range []string{"/usr/bin/apt-cache pkgnames", "/usr/bin/grep -Fqx -- \"$name\"", "/usr/bin/apt-cache show \"$name\"", "$2 == wanted || $2 == \"all\""} {
		if !strings.Contains(script, required) {
			t.Errorf("root helper lacks exact package resolution guard %q", required)
		}
	}
}

func TestSudoersCommandsGuardEveryManagedPathBeforeMutation(t *testing.T) {
	bootstrap, err := BuildSudoersBootstrapCommand("operator")
	if err != nil {
		t.Fatalf("BuildSudoersBootstrapCommand() error = %v", err)
	}
	disable, err := BuildSudoersDisableCommand("operator")
	if err != nil {
		t.Fatalf("BuildSudoersDisableCommand() error = %v", err)
	}
	for name, command := range map[string]string{"bootstrap": bootstrap, "disable": disable} {
		if output, err := exec.Command("/bin/sh", "-n", "-c", command).CombinedOutput(); err != nil {
			t.Errorf("%s command is not valid POSIX shell: %v\n%s", name, err, output)
		}
		for _, required := range []string{ManagedSudoersPath, RootHelperPath, LegacyAptSudoersPath, managedSudoersMarker, "root-owned regular file", "exact app-generated rule", "[ ! -L"} {
			if !strings.Contains(command, required) {
				t.Errorf("%s command missing guard %q", name, required)
			}
		}
	}
	if strings.Contains(disable, "rm -f -- \"$managed\"") || strings.Contains(disable, "rm -f "+ManagedSudoersPath) {
		t.Fatalf("disable uses unconditional managed-file deletion: %q", disable)
	}
	legacyCheck := strings.Index(disable, "exact app-generated rule")
	legacyRemove := strings.LastIndex(disable, `rm -- "$legacy"`)
	if legacyCheck < 0 || legacyRemove < 0 || legacyCheck >= legacyRemove {
		t.Fatalf("disable does not verify legacy ownership before removal")
	}
}

func TestSudoersCommandsRejectUnsafeSSHUser(t *testing.T) {
	for _, user := range []string{"", "root ALL=(ALL) NOPASSWD: ALL", "user,name", "user name", "../operator"} {
		if _, err := BuildSudoersBootstrapCommand(user); err == nil {
			t.Errorf("bootstrap accepted unsafe SSH user %q", user)
		}
		if _, err := BuildSudoersDisableCommand(user); err == nil {
			t.Errorf("disable accepted unsafe SSH user %q", user)
		}
	}
}

func TestSudoersCommandsAcceptInventorySSHUsernameAlphabet(t *testing.T) {
	for _, user := range []string{"operator", "Admin", "ops.admin", "ops-admin_2", "123", "-ops", ".ops"} {
		content, err := ManagedSudoersContent(user)
		if err != nil {
			t.Errorf("ManagedSudoersContent(%q) error = %v", user, err)
			continue
		}
		if !strings.Contains(content, "\n"+user+" ALL=(root)") {
			t.Errorf("ManagedSudoersContent(%q) = %q", user, content)
		}
	}
}

func TestSelectedPackageBuildersRejectEntireInvalidSet(t *testing.T) {
	for _, packages := range [][]string{
		{"openssl", "--reinstall"},
		{"openssl", "-o", "APT::Update::Pre-Invoke::=/bin/sh"},
		{"openssl", "libssl3;id"},
		{"openssl", "/tmp/package.deb"},
		{"openssl", "libssl3:amd64;id"},
		{"openssl", "curl-"},
		{"openssl", "curl:amd64-"},
		{"openssl", "a.+"},
	} {
		if got := BuildSelectedUpgradeCmd(packages); got != "" {
			t.Errorf("BuildSelectedUpgradeCmd(%q) = %q, want fail-closed empty command", packages, got)
		}
		if got := BuildSelectedInstallCmd(packages); got != "" {
			t.Errorf("BuildSelectedInstallCmd(%q) = %q, want fail-closed empty command", packages, got)
		}
		if got := BuildSelectedInstallSimulationCmd(packages); got != "" {
			t.Errorf("BuildSelectedInstallSimulationCmd(%q) = %q, want fail-closed empty command", packages, got)
		}
	}
}
