package updates

import (
	"debian-updater/internal/deferrals"
	"fmt"
	"strings"

	"debian-updater/internal/servers"
)

const (
	RootHelperPath          = "/usr/local/sbin/simplelinuxupdater-root-helper"
	ManagedSudoersPath      = "/etc/sudoers.d/simplelinuxupdater"
	LegacyAptSudoersPath    = "/etc/sudoers.d/apt-nopasswd"
	rootHelperOwnerMarker   = "# Managed by SimpleLinuxUpdater; do not edit."
	managedSudoersMarker    = "# Managed by SimpleLinuxUpdater; do not edit."
	rootHelperScriptVersion = "3"
)

func IsValidPackageSelector(selector string) bool { return deferrals.ValidSelector(selector) }

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
		"update", "upgrade", "full-upgrade", "full-upgrade-no-remove", "autoremove", "repair",
		"lock-probe", "lock-probe-extended", "dpkg-audit", "apt-check",
		"guarded-install *", "install *", "install-only-upgrade *", "install-no-remove *", "install-only-upgrade-no-remove *", "reboot",
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
` + guardedInstallHelperScript() + `
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
    full-upgrade-no-remove)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold --no-remove -y full-upgrade
        ;;
    autoremove)
        require_no_args "$@"
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -y autoremove
        ;;
    install-no-remove|install-only-upgrade-no-remove)
        require_packages "$@"
        if [ "$operation" = install-only-upgrade-no-remove ]; then
            exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold --no-remove -y install --only-upgrade -- "$@"
        fi
        exec /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold --no-remove -y install -- "$@"
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

// The root-owned, per-invocation hook checks APT's complete transaction before
// dpkg starts. No persistent preferences, selections, holds or hooks are edited.
func guardedInstallHelperScript() string {
	return `    guarded-install)
        PATH=/usr/sbin:/usr/bin:/sbin:/bin
        export PATH
        [ "$#" -ge 7 ] || refuse "guarded transaction arguments missing; update the managed helper"
        scope=$1; deadline=$2; count=$3; shift 3
        case "$scope" in all|security|security_kept_back|full_upgrade) ;; *) refuse "invalid guarded scope" ;; esac
        case "$deadline:$count" in *[!0-9:]*|:*|*:) refuse "invalid guard bounds" ;; esac
        [ "$count" -gt 0 ] && [ "$count" -le 1000 ] || refuse "invalid deferral count"
        [ "$(/usr/bin/date +%s)" -lt "$deadline" ] || refuse "package deferral expired; rescan and approve again"
        guard_dir=$(/usr/bin/mktemp -d /tmp/simplelinuxupdater-guard.XXXXXX)
        /usr/bin/chmod 0700 "$guard_dir"
        trap 'rm -rf -- "$guard_dir"' EXIT HUP INT TERM
        rules=$guard_dir/rules
        expected=$guard_dir/expected
        : > "$rules"; : > "$expected"
        while [ "$count" -gt 0 ]; do
            [ "$#" -gt 0 ] || refuse "truncated deferrals"
            valid_package_selector "$1" || refuse "invalid deferred selector"
            printf '%s\n' "$1" >> "$rules"
            shift; count=$((count - 1))
        done
        # Administrator holds remain effective even for explicitly targeted
        # installs and locally configured allow-change-held-packages defaults.
        holds=$(/usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin /usr/bin/apt-mark showhold) || refuse "administrator holds unavailable"
        for selector in $holds; do
            valid_package_selector "$selector" || refuse "invalid administrator hold selector"
            printf '%s\n' "$selector" >> "$rules"
        done
        [ "$#" -gt 0 ] || refuse "missing transaction count"
        count=$1; shift
        case "$count" in ''|*[!0-9]*) refuse "invalid transaction count" ;; esac
        [ "$count" -le 20000 ] || refuse "transaction too large"
        while [ "$count" -gt 0 ]; do
            [ "$#" -gt 0 ] || refuse "truncated transaction"
            selector=${1%%=*}; version=${1#*=}
            [ "$selector" != "$1" ] && valid_package_selector "$selector" || refuse "invalid transaction selector"
            case "$version" in ''|*[!a-zA-Z0-9.+:~_-]*) refuse "invalid transaction version" ;; esac
            printf '%s\n' "$1" >> "$expected"
            shift; count=$((count - 1))
        done
        [ "$#" -ge 2 ] || refuse "missing guarded targets"
        remove=$1; shift
        case "$remove" in no-remove|allow-removals) ;; *) refuse "invalid removal gate" ;; esac
        case "$scope:$remove" in all:allow-removals|security:allow-removals) refuse "scope cannot remove packages" ;; esac
        require_packages "$@"
        hook=$guard_dir/check
        cat > "$hook" <<'SLU_GUARD'
#!/bin/sh
set -eu
base=${0%/*}
exec /usr/bin/awk -v deadline="$(cat "$base/deadline")" '
function deny(message) { print "SimpleLinuxUpdater package deferral guard: " message > "/dev/stderr"; failed=1; exit 65 }
FILENAME==ARGV[1] { deferred[$0]=1; next }
FILENAME==ARGV[2] { approved[$0]=1; next }
FNR==1 { if ($0!="VERSION 3") deny("APT transaction protocol 3 required; no packages changed"); header=1; next }
header && !actions { if ($0=="") actions=1; next }
actions {
    if (NF!=9) deny("unrecognized APT transaction; rescan")
    if ($5==">" && $9!="**REMOVE**") deny("package downgrades are not permitted")
    name=$1; sub(/:.*/, "", name)
    arch=($7=="-" ? $3 : $7)
    if (arch=="-" || arch=="") deny("missing package architecture")
    selector=name ":" arch
    if (name in deferred || selector in deferred || (name ":" $3) in deferred) deny("APT would change deferred package " selector "; resolve dependencies or wait for expiry")
    if (arch=="all") {for (rule in deferred) {split(rule, parts, ":");if (parts[1]==name) deny("APT would change deferred architecture-independent package " name)}}
    change=selector "=" $6
    if (!(change in approved)) deny("APT transaction changed since approval for " selector "; rescan and approve again")
    seen[change]=1
}
END {
    if (failed || !header || !actions) exit 65
    for (change in approved) if (!(change in seen)) deny("APT transaction changed since approval; missing " change "; rescan and approve again")
    if (systime()>=deadline) {print "SimpleLinuxUpdater package deferral expired; rescan and approve again" > "/dev/stderr"; exit 65}
}
' "$base/rules" "$base/expected" -
SLU_GUARD
        printf '%s\n' "$deadline" > "$guard_dir/deadline"
        /usr/bin/chmod 0700 "$hook"
        # Append to existing administrator hooks. Request v3 explicitly and fail
        # closed in the hook if an older APT negotiates an older protocol.
        if [ "$remove" = no-remove ]; then set -- --no-remove "$@"; fi
        case "$scope" in all|security) set -- --only-upgrade "$@" ;; esac
        /usr/bin/env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin LC_ALL=C DEBIAN_FRONTEND=noninteractive DEBIAN_PRIORITY=critical APT_LISTCHANGES_FRONTEND=none NEEDRESTART_MODE=a UCF_FORCE_CONFFOLD=1 /usr/bin/apt-get -o APT::Ignore-Hold=false -o APT::Get::allow-change-held-packages=false -o APT::Get::allow-downgrades=false -o Dpkg::Options::=--force-confdef -o Dpkg::Options::=--force-confold -o "DPkg::Pre-Install-Pkgs::=$hook" -o "DPkg::Tools::Options::$hook::Version=3" -y install "$@"
        ;;
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
