// Package helperprotocol defines the managed helper's read-only handshake.
// The helper protocol is independent of APT's transaction hook protocol.
package helperprotocol

import "strings"

const (
	Version          = "3"
	Path             = "/usr/local/sbin/simplelinuxupdater-root-helper"
	OwnerMarker      = "# Managed by SimpleLinuxUpdater; do not edit."
	Capabilities     = "simplelinuxupdater-helper protocol=" + Version + " guarded-install"
	InstallationHelp = "Export with webserver helper export --user SSH_USER > install-helper.sh (or docker run --rm --entrypoint /app/webserver IMAGE helper export --user SSH_USER). Review the script, transfer it to the target, then run sudo /bin/sh install-helper.sh there. Run Retry checks in Manage after installation; for package deferrals, rescan and approve again."
)

// RequiredCommand checks ownership and the marker before executing even the
// read-only handshake. It never invokes APT or changes the host. A sub-shell
// prevents its exit/status variables from escaping into a caller's command.
const RequiredCommand = `(helper=` + Path + `
fail_helper() { printf '%s\n' "$1"; printf '%s\n' "Managed helper protocol ` + Version + ` required: $1" >&2; printf '%s\n' '` + InstallationHelp + `' >&2; exit 65; }
if [ ! -e "$helper" ] && [ ! -L "$helper" ]; then fail_helper SLU_HELPER_MISSING; fi
[ -f "$helper" ] && [ ! -L "$helper" ] && [ "$(stat -c %u -- "$helper")" = 0 ] && [ "$(sed -n '2p' -- "$helper")" = '` + OwnerMarker + `' ] || fail_helper SLU_HELPER_UNRECOGNIZED
[ "$(sed -n '3p' -- "$helper")" = '# Protocol version ` + Version + `' ] || fail_helper SLU_HELPER_OLD
[ -x "$helper" ] && [ "$("$helper" capabilities 2>/dev/null)" = '` + Capabilities + `' ] || fail_helper SLU_HELPER_OLD
if [ "$(id -u)" -ne 0 ]; then
    sudo -n -l -- "$helper" update >/dev/null 2>&1 && sudo -n -l -- "$helper" upgrade >/dev/null 2>&1 && sudo -n -l -- "$helper" dpkg-audit >/dev/null 2>&1 && sudo -n -l -- "$helper" guarded-install probe >/dev/null 2>&1 && sudo -n -- "$helper" dpkg-audit >/dev/null 2>&1 || fail_helper SLU_HELPER_SUDO_DENIED
fi
printf '%s\n' SLU_HELPER_READY)`

const OnboardingCommand = `if [ "$(id -u)" -eq 0 ]; then printf root; else ` + RequiredCommand + `; fi`

func Diagnostic(output string) string {
	// Only fixed protocol tokens are interpreted; remote output is never shown.
	for _, item := range []struct{ token, message string }{
		{"SLU_HELPER_MISSING", "The managed root helper is absent."},
		{"SLU_HELPER_OLD", "The managed root helper is too old or lacks protocol " + Version + " capabilities."},
		{"SLU_HELPER_UNRECOGNIZED", "The helper path is not a recognized root-owned regular managed file. Have an administrator inspect it; automatic replacement is refused."},
		{"SLU_HELPER_SUDO_DENIED", "The restricted sudo policy denies maintenance access to the managed helper."},
	} {
		if containsLine(output, item.token) {
			return item.message
		}
	}
	return "Restricted maintenance permissions could not be verified."
}

func containsLine(output, token string) bool {
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == token {
			return true
		}
	}
	return false
}
