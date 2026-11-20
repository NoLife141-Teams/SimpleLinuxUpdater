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
- [Incomplete backup recovery](#incomplete-backup-recovery)
- [Verify an archive offline](#verify-an-archive-offline)
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

Use local account recovery with the same binary and database path as the service. Stop the service first, then run as the service's local OS user; authorized administrators can use `sudo -u SERVICE_USER` when necessary. This keeps the stable lock file owned by the service account:

```bash
systemctl stop simplelinuxupdater
DEBIAN_UPDATER_DB_PATH=/var/lib/simplelinuxupdater/servers.db \
  ./webserver admin reset-password --username admin
systemctl start simplelinuxupdater
```

Adjust the service name, account and path for your installation. Without `--username`, the terminal asks for the target account. It then asks for the new password and confirmation **without echo**. The current password policy applies. Ctrl-C/SIGTERM cancels entry, restores terminal settings and discards pending input. Recovery is dispatched before HTTP, schedulers, notifications or other workers start.

The command updates the existing account, advances the established authentication generation, revokes all sessions in the current single-account model, and writes `auth.password.recover` with actor `local-recovery` in one transaction. Audit/write failure rolls the transaction back and returns a nonzero exit; an unconfirmed commit is reported explicitly. The account is not recreated and `/setup` stays closed. Inventory, SSH credentials, `known_hosts`, policies, historical audits and encrypted configuration are preserved. Sign in again after restarting the service; every previous browser session and the old password are invalid.

The server and recovery command hold the same OS lock on `<database path>.lock` throughout their process lifetime, including database restore/replacement and shutdown. The command refuses an active server or another recovery command. **Keep this sidecar file**: deleting it while a process runs defeats process coordination. Stop older binaries that predate installation locking before upgrading/recovering. Parent directory symlinks resolve to the same lock; symbolic/hard links for the database or lock file are rejected so another filename cannot bypass exclusivity. Use a local filesystem with working OS advisory locks, and the same configured path for all processes.

Recovery requires an existing valid writable database and the current auth/audit schema. It does not create a database, run migrations or import legacy inventories. If an old database needs migration, start the current server once and stop it before recovery. Permission, missing-account, weak-password and confirmation failures return errors without resetting the account. A stale `.lock` file after a crash is normal; the OS releases its lock when the process exits.

For Docker, stop the app container, run a temporary container from the **same image** against the same named volume and configured database path, then restart the app:

```bash
docker stop simplelinuxupdater
docker run --rm -it -v simplelinuxupdater-data:/data IMAGE \
  ./webserver admin reset-password --username admin
docker start simplelinuxupdater
```

Replace `IMAGE` and the volume/container names with your deployment's values. Reproduce custom volume mounts and `DEBIAN_UPDATER_DB_PATH` when applicable. The image entrypoint repairs owned persistence paths (including the stable lock) and drops to the application user. `docker exec` against a running server is refused by the lock; use a stopped installation. These commands do not require deleting the account or editing SQL.

For automation, `--password-stdin` explicitly opts into redirected input and requires `--username`. Supply two newline-terminated lines containing the new password and matching confirmation from a protected secret source; the command rejects terminal input in this mode. Do not pass passwords in arguments, environment variables, shell command literals or shell history. For example, a root/service-readable secret file containing those two lines can be redirected without exposing its contents:

```bash
./webserver admin reset-password --username admin --password-stdin < /run/secrets/recovery-password
# Docker automation uses -i (without -t) and the same protected input source.
```

Remove the temporary secret source through your secret-management procedure after use. Passwords and hashes are never included in CLI output or the recovery audit.

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

## Backup storage unavailable

In Admin → Installation Health, check the last successful storage observation.
Verify that the configured archive mount is present and readable by the service
user. A stale observation means the worker has not confirmed the result recently.
After the mount returns, check that a new observation succeeds and the catalog
shows a verified recovery point again.

If a restore is interrupted and offline inspection reports `insufficient_storage`,
free space on the affected recovery volume and inspect again. Follow the
[incomplete backup recovery procedure](#incomplete-backup-recovery).

## Incomplete backup recovery

If both backup application and recovery fail, the app keeps maintenance active and returns `backup recovery incomplete; maintenance remains active`. Requests, scheduled work and queued notifications stay blocked. An interrupted restore also stays blocked on startup. Before changing active files, the app synchronizes the rollback files and their versioned manifest and creates a durable marker beside SQLite. For `servers.db`, the marker is `servers.db.restore-recovery`. It binds the manifest to the rollback directory. Reserve this path for recovery state. Startup recognizes the marker even when the SQLite maintenance state is missing. A restart alone does not clear it.

Stop the application before running either mode. Use the same working directory, environment and files as the service. For a binary installation, run as the service user.

```sh
./webserver backup recover --inspect
./webserver backup recover --apply <recovery-set-id>
```

For Docker Compose, use your existing service name so the one-shot command receives the same volumes and path configuration. The packaged image runs the application as `app:app` by default. If you configured another application user, use that user in the commands below.

```sh
docker compose stop <service>
docker compose run --rm --no-deps -T --user app:app --entrypoint /app/webserver <service> backup recover --inspect
docker compose run --rm --no-deps -T --user app:app --entrypoint /app/webserver <service> backup recover --apply <recovery-set-id>
docker compose start <service>
```

Inspect the returned set IDs, inventory count and APT report count. Confirm the intended installation against your records and choose a set marked `ready`. Application keeps a private diagnosis copy beside SQLite, validates the installed files and releases recovery only after those checks pass. It invalidates restored sessions and suspends automated backups until local configuration is revalidated. Start the app after a successful application, sign in again, and review the inventory and any active APT incident from the original job report.

The report identifies the source of the APT and notification delivery evidence for each ready set. `live_database` means the command could reconcile the rollback set with the installed SQLite database. `sealed_rollback` means the installed SQLite database could not be read and the command verified the complete pre-replacement files against the durable marker and the installation encryption key. The command preserves the damaged SQLite and its sidecars in the diagnosis copy before applying the set. If the archive excluded `known_hosts`, recovery checks and preserves the active trust file.

The JSON report includes estimated and available bytes for preparation, the diagnosis copy, SQLite replacement and host trust when applicable. Roles sharing a filesystem are grouped. Preparation checks copies whose sizes are known at each phase. SQLite can allocate additional workspace to merge evidence and compact the database. Once staging completes, a shared volume's estimate describes the writes still needed and uses the compacted SQLite file size. For `insufficient_storage`, free space on the volume marked insufficient or identified by `storage_role` and run inspection again before applying the same set. Preparation uses the application's temporary directory (`TMPDIR`). The diagnosis copy is written beside SQLite. A storage or access failure while preparing the live SQLite copy leaves recovery locked so its APT and notification evidence can still be used after the failure is resolved. Capacity can be unknown and available bytes can change between inspection and application.

An older rollback set requires a readable installed database. If the report cannot validate a set, preserve the volume and rollback directories for diagnosis. A verified independent backup and retained recovery evidence may be needed. The process and container logs may also contain the original restore failure.

If inspection reports `resume_on_startup` as `preparing_v1` or `finalized_v1`,
start the application normally. It checks SQLite and clears the maintenance
lock without replacing files. The first state means the restore stopped
before modifying active files. The second means the installed files were
verified before the lock could be released. Startup removes a remaining
marker only if it matches the recorded finalization. Restore isolation, if enabled,
still requires administrator review. A missing marker with no verified phase
does not clear the lock. Inspection reports `no_verified_recovery_set` when
there are no rollback sets to select.

If the recovery latch cannot be persisted, the current process still blocks work and logs that persistence error. Diagnose the storage failure before restarting. Retained rollback files contain sensitive application data. Protect them like backups and remove them after recovery has been verified. A process interruption can leave rollback files behind even when startup is able to resume without them.

## Verify an archive offline

Run this command on the recovery machine with an existing writable directory
for temporary files. Pick a directory on another volume if the normal temporary
volume is full.

```sh
webserver backup verify --archive /recovery/backup.slubkp --work-dir /mnt/scratch
```

The command prompts for the passphrase without echoing it. For unattended
verification, pass `--passphrase-file` with the path to a private regular
file of at most 4096 bytes. Add `--json` for machine-readable output. If
`--work-dir` is omitted, the command uses the system temporary directory.

On Windows, use NTFS and restrict the passphrase file to the account running
the command, SYSTEM and local Administrators. The same access policy applies
to the rollback directory and its snapshots. The application creates backup
work files and rollback directories with private ACLs. It verifies the ACLs
before reading the passphrase or applying a rollback set.

Verification opens the archive in read-only mode and validates its decrypted
contents and SQLite compatibility in the workspace. The report marks the
restoration target as uninspected. Check the target installation in Admin
before replacing its data. Use `backup recover --inspect` when an existing
installation has an incomplete-restore marker.

Exit code `0` records successful archive verification. Code `3` means the
archive could not be authenticated or validated, `4` means its format or data
is incompatible, and `5` means the verification workspace lacks space.
On code `5`, compare the preparation volume's required and available bytes
when reported, then free space or select another workspace and verify the same
archive again. Code `2` indicates invalid command arguments, code `1` an
operational error, and code `130` an interruption.
