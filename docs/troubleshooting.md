[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Troubleshooting

## Table of contents

- [Setup and login issues](#setup-and-login-issues)
- [Forgotten admin password](#forgotten-admin-password)
- [Metrics authentication issues](#metrics-authentication-issues)
- [SSH host key issues](#ssh-host-key-issues)
- [APT locks and missing fuser](#apt-locks-and-missing-fuser)
- [`needs_reconciliation` after an APT timeout](#needs_reconciliation-after-an-apt-timeout)
- [Sudoers migration refused](#sudoers-migration-refused)
- [Pre-check failures](#pre-check-failures)
- [APT/DPKG health failures](#aptdpkg-health-failures)
- [Post-check failures](#post-check-failures)
- [Controlled reboot verification failures](#controlled-reboot-verification-failures)
- [CVE enrichment issues](#cve-enrichment-issues)
- [Notification delivery failures](#notification-delivery-failures)
- [Database and file permissions](#database-and-file-permissions)

## Setup and login issues

Symptom: cannot create user, cannot log in, or repeated redirects to `/login`.

Checks:

- On first run, you must complete `/setup` before `/login` works.
- Password must meet policy requirements (length and complexity).
- Confirm your browser accepts cookies for the app host.
- If behind HTTPS, ensure `DEBIAN_UPDATER_SESSION_COOKIE_SECURE` matches your deployment:
  - `true` when served over HTTPS
  - `false` for local plain HTTP testing

## Forgotten admin password

For single-user deployments, password recovery is a reset flow:

1. Stop the application.
2. Back up `servers.db` before editing it.
3. In one SQLite transaction, delete rows from `sessions` and then `auth_users`.
4. Start the application again and revisit `/setup` to create a new admin user.

Impact:

- Deleting `sessions` and `auth_users` resets login and invalidates existing authenticated sessions while preserving inventory and operational history.
- Dropping the entire DB also removes saved servers, audit history, and app settings.

## Metrics authentication issues

Symptom: `/metrics` returns `404` or `401`.

Checks:

- If `404`: metrics token is not configured (generate one from `/admin`).
- If `401`: scraper must send `Authorization: Bearer <token>`.
- If token was rotated, update scraper credentials to the newest token.

## SSH host key issues

Symptom: SSH connection fails due to unknown host key or fingerprint mismatch.

Fix:

- Use the UI "Trust SSH host key now" to scan and trust the host key.
- Verify the fingerprint out-of-band before trusting.
- In Edit Server, use **Known host management** to check whether the current host/port is already saved and to clear a stale entry before re-trusting.
- If you rotate host keys, clear the stale known-host entry and trust the new fingerprint.

## APT locks and missing fuser

Symptom: `apt_locks` pre-check fails before `apt-get update`.

Notes:

- The lock pre-check requires `/usr/bin/fuser` from the `psmisc` package. Root SSH sessions run it directly; non-root SSH users run it through `sudo`.
- Since v0.2.3, missing `fuser` is a blocking pre-check failure. The older process-name fallback was removed because it could be spoofed by unrelated processes.
- Missing lock files are treated as no-lock (non-fatal).

Examples you may see in logs:

- Missing `fuser`:
  - `sudo: /usr/bin/fuser: command not found`
  - `sudo: unable to execute /usr/bin/fuser: No such file or directory`
- Non-root user on a host without `sudo`:
  - `sh: 1: sudo: not found`
  - `bash: line 1: sudo: command not found`
- Lock file path missing (non-fatal/no-lock):
  - `/usr/bin/fuser: /var/cache/apt/archives/lock: No such file or directory`

Fix:

```bash
sudo apt-get update
sudo apt-get install -y psmisc
```

After installing `psmisc`, rerun **Enable apt** so the app installs its typed root helper and managed sudoers rule. Do not add a generic `fuser`, `apt`, or `apt-get` sudoers grant; the helper exposes only the two fixed lock-probe operations documented in [security.md](security.md).

If the host intentionally uses root SSH without `sudo` (common on Proxmox), connect as `root`; the updater will run apt and pre-check commands directly.

## `needs_reconciliation` after an APT timeout

This status means a mutating APT command timed out while its final outcome could not be proven. Do not immediately retry the update: the original `apt-get` or `dpkg` process may have continued after the SSH command timed out.

1. Open the server in Status and follow **Recommended action → Repair package state**.
2. Review the logs and verify no external maintenance session is active.
3. Confirm **Repair APT**. The repair refuses to start its dpkg work while an APT/DPKG lock holder is detected.
4. Retry the update only after the repair job reports that package health checks passed.

For non-root SSH users configured before the typed helper was added, run **Enable apt** again so repair uses the helper's fixed operation.

## Sudoers migration refused

Symptom: **Enable apt** or **Disable apt** fails with `refused sudoers file operation`, `has no owner marker`, or `legacy apt-nopasswd is not an exact app-generated rule`.

SimpleLinuxUpdater refuses to overwrite or remove `/etc/sudoers.d/simplelinuxupdater`, `/usr/local/sbin/simplelinuxupdater-root-helper`, or the legacy `/etc/sudoers.d/apt-nopasswd` unless the file type, root ownership, and app identity checks pass. This protects administrator-managed files and symlinks.

1. Inspect the reported path as root without changing it.
2. If `/etc/sudoers.d/apt-nopasswd` is administrator-managed or customized, migrate the required policy manually and remove the generic legacy grants only after validating the replacement.
3. If an app-managed file was edited, compare it with a trusted release and decide whether to remove it manually before rerunning **Enable apt**.
4. Run `sudo visudo -c` after any manual sudoers change.

The app never treats an unrecognized legacy file as safe to delete.

## Pre-check failures

Common reasons:

- Insufficient free disk space on `/var` or `/`
- Baseline disk space below `1 GiB` (1048576 KB)
- Exact plan requirement is too large for the filesystem holding the configured APT archive cache, `/var`, or `/`; it includes archive bytes, positive installed growth, a bounded safety margin, and the `1 GiB` operational reserve
- APT did not provide trustworthy size facts and the compatibility estimate is too large: `1 GiB` base + `64 MiB` per planned package + `512 MiB` per newly installed package
- APT/DPKG health failures (`dpkg --audit` or `apt-get check`)
- Lock contention

The plan-aware failure log identifies the calculation source (`exact` or `estimate`) and blocking filesystem. Exact results include archive bytes, installed growth, safety margin, and reserve; estimated results include planned and newly installed package counts. Free space or remove unnecessary files, then run a fresh scan so the requirement is recalculated from the new plan. The updater never runs cleanup or autoremove automatically.

## APT/DPKG health failures

Symptom: `apt_health` pre-check fails and the log shows `dpkg --audit` or `apt-get check` output.

Notes:

- This means the host already has an interrupted or inconsistent package state before the updater starts.
- The updater stops before `apt-get update` so it does not hide or worsen the existing package problem.
- Use **Recommended action → Repair package state** when available. The repair first verifies that no package-manager lock is active, completes pending dpkg configuration, repairs dependencies, and reruns both health checks.
- If the in-app repair cannot complete, fix the reported maintainer-script or dependency problem directly on the host, then rerun the update.

Common recovery command:

```bash
sudo dpkg --configure -a
```

If the command reports a specific package maintainer-script failure, fix that package first. For example, a missing Postfix configuration such as `/etc/postfix/main.cf` must be restored or regenerated before dependent packages can finish configuring.

## Post-check failures

Common reasons:

- Failed systemd units after upgrade
- APT/DPKG health failures after upgrade

Blocking behavior is configurable; see [configuration.md](configuration.md).

## Controlled reboot verification failures

The controlled reboot command is sent once and is never retried automatically. A reboot job remains failed when SimpleLinuxUpdater cannot prove all required outcomes:

- SSH became unavailable and then returned;
- uptime reset compared with the pre-reboot baseline;
- `/var/run/reboot-required` cleared.

Check the host console or hypervisor, confirm that `systemd` is the active init system, verify SSH startup and network reachability, and inspect the reboot-required marker. Do not repeatedly click reboot until the host's actual state is known.

## CVE enrichment issues

Symptom: CVE state becomes `unavailable`.

Possible causes:

- SSH dial failure in the enrichment goroutine
- Failure collecting `/etc/os-release`, `dpkg-query`, or APT candidate metadata
- The application cannot reach `https://api.osv.dev`
- OSV returned an invalid or incomplete response

A `Coverage unknown` state is different from `unavailable`: it means an explicit installed or candidate origin is third-party or unsupported, or its local APT `InRelease` metadata could not be validated with the distribution archive keyring, so SimpleLinuxUpdater deliberately makes no vulnerability claim. Check that `gpgv` and the distribution archive keyring package are installed and that `/var/lib/apt/lists` contains current metadata. When APT no longer exposes the origin of an older installed version but the candidate is signed by the official archive, the OSV assessment still runs and is labelled `Installed provenance unverified`.

## Notification delivery failures

Open Admin → Notification Hooks and review Delivery diagnostics for the destination, attempt count, HTTP status, consecutive failures, and next retry time.

Checks:

- Generic public webhook URLs use HTTPS and contain no embedded credentials.
- Discord webhook and Telegram bot credentials have not been rotated or revoked.
- The Telegram bot is a member of the target chat/channel and can post there.
- DNS, proxy, firewall, and outbound HTTPS rules permit the updater process to reach the destination.
- The receiver accepts the destination-specific payload and returns a successful HTTP status.

Use **Send Test** only after verifying the destination because it emits a real message. A notification failure does not roll back or change the triggering maintenance outcome.

## Database and file permissions

Symptom: missing persistence or errors writing DB/config.

Fix:

- Ensure the process user can read/write the data directory.
- In Docker, mount a volume to `/data`.
