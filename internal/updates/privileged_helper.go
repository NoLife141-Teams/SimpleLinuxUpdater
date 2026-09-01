package updates

import (
	"fmt"
	"regexp"
	"strings"

	"debian-updater/internal/servers"
)

const (
	RootHelperPath          = "/usr/local/sbin/simplelinuxupdater-root-helper"
	ManagedSudoersPath      = "/etc/sudoers.d/simplelinuxupdater"
	LegacyAptSudoersPath    = "/etc/sudoers.d/apt-nopasswd"
	rootHelperOwnerMarker   = "# Managed by SimpleLinuxUpdater; do not edit."
	managedSudoersMarker    = "# Managed by SimpleLinuxUpdater; do not edit."
	rootHelperScriptVersion = "1"
)

var (
	packageNamePattern  = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]+$`)
	architecturePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

func IsValidPackageSelector(selector string) bool {
	if selector == "" || strings.TrimSpace(selector) != selector || strings.Count(selector, ":") > 1 {
		return false
	}
	name, architecture, hasArchitecture := strings.Cut(selector, ":")
	if !packageNamePattern.MatchString(name) || strings.HasSuffix(name, "-") || strings.Contains(name, ".+") {
		return false
	}
	return !hasArchitecture || architecturePattern.MatchString(architecture) && !strings.HasSuffix(architecture, "-")
}

func IsValidSudoersUser(user string) bool {
	return servers.IsValidSSHUsername(user) && strings.TrimSpace(user) == user
}

func shellQuote(value string) string {
	return "'" + ShellEscapeSingleQuotes(value) + "'"
}

func RootOrSudoHelperCommand(rootCommand, operation string, args ...string) string {
	helperCommand := "sudo -n " + RootHelperPath + " " + shellQuote(operation)
	for _, arg := range args {
		helperCommand += " " + shellQuote(arg)
	}
	return fmt.Sprintf("if [ \"$(id -u)\" -eq 0 ]; then %s; else %s; fi", rootCommand, helperCommand)
}

func NonInteractiveAptSudoersSpec() string {
	operations := []string{
		"update", "upgrade", "full-upgrade", "autoremove", "repair",
		"lock-probe", "lock-probe-extended", "dpkg-audit", "apt-check",
		"install *", "install-only-upgrade *", "reboot",
	}
	specs := make([]string, 0, len(operations))
	for _, operation := range operations {
		specs = append(specs, RootHelperPath+" "+operation)
	}
	return strings.Join(specs, ", ")
}

func ManagedSudoersContent(user string) (string, error) {
	if !IsValidSudoersUser(user) {
		return "", fmt.Errorf("SSH user %q cannot be represented safely in the managed sudoers rule", user)
	}
	return managedSudoersMarker + "\n" + user + " ALL=(root) NOPASSWD: " + NonInteractiveAptSudoersSpec() + "\n", nil
}

func LegacyAptSudoersContent(user string) (string, error) {
	if !IsValidSudoersUser(user) {
		return "", fmt.Errorf("SSH user %q cannot be represented safely in the legacy sudoers rule", user)
	}
	line := fmt.Sprintf("%s ALL=(root) NOPASSWD: /usr/bin/apt, /usr/bin/apt-get, /usr/bin/dpkg --audit, /usr/bin/dpkg --configure -a, /usr/bin/env %s /usr/bin/apt-get *, /usr/bin/env %s /usr/bin/dpkg --force-confdef --force-confold --configure -a, /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock, /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock, /usr/bin/systemctl reboot", user, AptNonInteractiveEnvironment, AptNonInteractiveEnvironment)
	return line + "\n", nil
}

func RootHelperScript() string {
	return `#!/bin/sh
` + rootHelperOwnerMarker + `
# Protocol version ` + rootHelperScriptVersion + `
set -eu

refuse() {
    printf '%s\n' "refused: $*" >&2
    exit 64
}

require_no_args() {
    [ "$#" -eq 0 ] || refuse "operation does not accept arguments"
}

valid_package_selector() {
    selector=$1
    case "$selector" in
        ""|?:*:*|*:*:*|*-|*.+*|[!a-z0-9]*|*[!a-z0-9+.:_-]*) return 1 ;;
    esac
    name=${selector%%:*}
    case "$name" in
        ""|?|*-|*.+*|[!a-z0-9]*|*[!a-z0-9+.-]*) return 1 ;;
    esac
    if [ "$selector" != "$name" ]; then
        architecture=${selector#*:}
        case "$architecture" in
            ""|[!a-z0-9]*|*[!a-z0-9-]*) return 1 ;;
        esac
    fi
    return 0
}

require_packages() {
    [ "$#" -gt 0 ] || refuse "package operation requires at least one selector"
    for selector in "$@"; do
        valid_package_selector "$selector" || refuse "invalid package selector"
    done
    available_names=$(/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-cache pkgnames) || refuse "could not read the APT package index"
    for selector in "$@"; do
        name=${selector%%:*}
        printf '%s\n' "$available_names" | /usr/bin/grep -Fqx -- "$name" || refuse "package selector does not resolve to one exact package"
        if [ "$selector" != "$name" ]; then
            architecture=${selector#*:}
            /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-cache show "$name" | /usr/bin/awk -v wanted="$architecture" '$1 == "Architecture:" && ($2 == wanted || $2 == "all") { found=1 } END { exit !found }' || refuse "package architecture is not available for the exact package"
        fi
    done
}

[ "$#" -gt 0 ] || refuse "missing operation"
operation=$1
shift

case "$operation" in
    update)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold update
        ;;
    upgrade)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y upgrade
        ;;
    full-upgrade)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y full-upgrade
        ;;
    autoremove)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y autoremove
        ;;
    install|install-only-upgrade)
        require_packages "$@"
        if [ "$operation" = install-only-upgrade ]; then
            exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y install --only-upgrade -- "$@"
        fi
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y install -- "$@"
        ;;
    lock-probe)
        require_no_args "$@"
        exec /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock
        ;;
    lock-probe-extended)
        require_no_args "$@"
        exec /usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock
        ;;
    dpkg-audit)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/dpkg --audit
        ;;
    apt-check)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-get check
        ;;
    repair)
        require_no_args "$@"
        lock_output=$(/usr/bin/fuser /var/lib/dpkg/lock-frontend /var/lib/dpkg/lock /var/cache/apt/archives/lock /var/lib/apt/lists/lock 2>&1) && lock_status=0 || lock_status=$?
        if [ "$lock_status" -eq 0 ]; then
            printf '%s\n' 'APT/DPKG repair blocked: package-manager lock is active.' "$lock_output" >&2
            exit 75
        fi
        if [ "$lock_status" -ne 1 ] || [ -n "$lock_output" ]; then
            printf '%s\n' 'APT/DPKG repair blocked: package-manager lock probe failed.' "$lock_output" >&2
            exit 76
        fi
        /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/dpkg --force-confdef --force-confold --configure -a
        /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y -f install
        audit_output=$(/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/dpkg --audit 2>&1) || {
            status=$?
            printf '%s\n' 'APT/DPKG repair verification failed: dpkg audit command failed.' "$audit_output" >&2
            exit "$status"
        }
        [ -z "$audit_output" ] || {
            printf '%s\n' 'APT/DPKG repair verification failed: dpkg audit still reports package-state problems.' "$audit_output" >&2
            exit 77
        }
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-get check
        ;;
    reboot)
        require_no_args "$@"
        exec /usr/bin/systemctl reboot
        ;;
    *)
        refuse "unknown operation"
        ;;
esac
`
}

func BuildSudoersBootstrapCommand(user string) (string, error) {
	managedContent, legacyContent, err := sudoersManagedAndLegacyContent(user)
	if err != nil {
		return "", err
	}
	script := sudoersGuardScript(legacyContent) + `
helper_tmp=$(mktemp /usr/local/sbin/.simplelinuxupdater-root-helper.XXXXXX)
sudoers_tmp=$(mktemp /etc/sudoers.d/.simplelinuxupdater.XXXXXX)
cleanup() {
    rm -f -- "$helper_tmp" "$sudoers_tmp"
}
trap cleanup EXIT HUP INT TERM
helper_content=` + shellQuote(strings.TrimSuffix(RootHelperScript(), "\n")) + `
managed_content=` + shellQuote(strings.TrimSuffix(managedContent, "\n")) + `
printf '%s\n' "$helper_content" > "$helper_tmp"
chown root:root "$helper_tmp"
chmod 0755 "$helper_tmp"
printf '%s\n' "$managed_content" > "$sudoers_tmp"
chown root:root "$sudoers_tmp"
chmod 0440 "$sudoers_tmp"
/usr/sbin/visudo -cf "$sudoers_tmp"
if [ -e "$legacy" ] || [ -L "$legacy" ]; then rm -- "$legacy"; fi
mv -f -- "$helper_tmp" "$helper"
mv -f -- "$sudoers_tmp" "$managed"
trap - EXIT HUP INT TERM
/usr/sbin/visudo -cf "$managed"
`
	return "sudo -S -p '' /bin/sh -c " + shellQuote(script), nil
}

func BuildSudoersDisableCommand(user string) (string, error) {
	_, legacyContent, err := sudoersManagedAndLegacyContent(user)
	if err != nil {
		return "", err
	}
	script := sudoersGuardScript(legacyContent) + `
if [ -e "$managed" ] || [ -L "$managed" ]; then rm -- "$managed"; fi
if [ -e "$helper" ] || [ -L "$helper" ]; then rm -- "$helper"; fi
if [ -e "$legacy" ] || [ -L "$legacy" ]; then rm -- "$legacy"; fi
`
	return "sudo -S -p '' /bin/sh -c " + shellQuote(script), nil
}

func sudoersManagedAndLegacyContent(user string) (string, string, error) {
	managedContent, err := ManagedSudoersContent(user)
	if err != nil {
		return "", "", err
	}
	legacyContent, err := LegacyAptSudoersContent(user)
	if err != nil {
		return "", "", err
	}
	return managedContent, legacyContent, nil
}

func sudoersGuardScript(legacyContent string) string {
	return `set -eu
PATH=/usr/sbin:/usr/bin:/sbin:/bin
export PATH
managed=` + shellQuote(ManagedSudoersPath) + `
helper=` + shellQuote(RootHelperPath) + `
legacy=` + shellQuote(LegacyAptSudoersPath) + `
owner_marker=` + shellQuote(managedSudoersMarker) + `
helper_marker=` + shellQuote(rootHelperOwnerMarker) + `
legacy_content=` + shellQuote(strings.TrimSuffix(legacyContent, "\n")) + `

refuse() {
    printf '%s\n' "SimpleLinuxUpdater refused sudoers file operation: $*" >&2
    exit 65
}
is_root_owned_regular() {
    [ -f "$1" ] && [ ! -L "$1" ] && [ "$(stat -c %u -- "$1")" = 0 ]
}
if [ -e "$managed" ] || [ -L "$managed" ]; then
    is_root_owned_regular "$managed" || refuse "managed sudoers path is not a root-owned regular file"
    [ "$(sed -n '1p' -- "$managed")" = "$owner_marker" ] || refuse "managed sudoers path has no owner marker"
fi
if [ -e "$helper" ] || [ -L "$helper" ]; then
    is_root_owned_regular "$helper" || refuse "helper path is not a root-owned regular file"
    [ "$(sed -n '2p' -- "$helper")" = "$helper_marker" ] || refuse "helper path has no owner marker"
fi
if [ -e "$legacy" ] || [ -L "$legacy" ]; then
    is_root_owned_regular "$legacy" || refuse "legacy apt-nopasswd path is not a root-owned regular file"
    [ "$(cat -- "$legacy")" = "$legacy_content" ] || refuse "legacy apt-nopasswd is not an exact app-generated rule; remove or migrate it manually"
fi
`
}
