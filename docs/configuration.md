[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Configuration

## Table of contents

- [Authentication and sessions](#authentication-and-sessions)
- [Network listener](#network-listener)
- [Codex browser annotations](#codex-browser-annotations)
- [Metrics API token](#metrics-api-token)
- [Notification destinations](#notification-destinations)
- [Backup and restore](#backup-and-restore)
  - [Automated backups](#automated-backups)
- [Storage paths](#storage-paths)
- [Job log storage and retention](#job-log-storage-and-retention)
- [Retry policy](#retry-policy)
- [Automatic host-facts refresh](#automatic-host-facts-refresh)
- [Post-update checks](#post-update-checks)
- [Known hosts handling](#known-hosts-handling)
- [Environment file (.env)](#environment-file-env)

## Authentication and sessions

SimpleLinuxUpdater now uses a built-in single-user login flow:

- First run requires setup at `/setup` to create the initial local user.
- Passwords are stored as Argon2id hashes in SQLite (`auth_users` table).
- Authenticated UI/API access uses server-side sessions stored in SQLite.

Session defaults:

- Lifetime: 30 days
- Cookie: `HttpOnly`, `SameSite=Lax`
- Cookie `Secure`: configurable (recommended `true` behind HTTPS)

Environment variables:

- `DEBIAN_UPDATER_SESSION_COOKIE_SECURE` (`true|false`, default `false`)
- `DEBIAN_UPDATER_SESSION_IDLE_TIMEOUT_HOURS` (optional, integer hours; unset/`0` keeps default behavior)
- `DEBIAN_UPDATER_TRUSTED_PROXIES` (optional comma-separated proxy IPs/CIDRs; unset/`none` trusts no proxies)

When `DEBIAN_UPDATER_TRUSTED_PROXIES` is configured, Gin honors forwarded client IP headers from those proxies. This affects audit `client_ip` values and in-memory auth/metrics rate limiting.

## Network listener

`DEBIAN_UPDATER_LISTEN_ADDR` controls the HTTP bind address. Direct binary runs
default to `127.0.0.1:8080`. The Docker image explicitly sets `:8080` so port
publishing remains compatible.

Values must use `host:port` syntax with a numeric port from `1` to `65535`.
Bracket IPv6 literals, for example `[::1]:8080`. Surrounding whitespace is
ignored. Malformed values and addresses that cannot be resolved or bound stop
startup with a clear error before the router and background workers are started.

Examples:

```bash
DEBIAN_UPDATER_LISTEN_ADDR=127.0.0.1:8080 ./webserver
DEBIAN_UPDATER_LISTEN_ADDR='[::1]:8080' ./webserver
```

Use a non-loopback address such as `:8080` only when access is restricted to the
intended LAN, VPN, container network, or reverse proxy. This is especially
important before the first admin account has been created at `/setup`.

## Codex browser annotations

SimpleLinuxUpdater's strict Content Security Policy blocks inline `<style>` elements by default. The Codex in-app browser injects its annotation overlay stylesheet as an inline shadow-root style, so local annotation mode requires an explicit development exception:

```bash
export DEBIAN_UPDATER_DEV_ALLOW_BROWSER_ANNOTATIONS=true
./webserver
```

The exception adds `style-src-elem 'unsafe-inline'` while leaving `script-src` strict. It is disabled by default and should not be enabled when the UI is exposed beyond a trusted local development environment.

## Metrics API token

`/metrics` is protected separately from UI sessions for machine-to-machine scraping.

Behavior:

- Disabled by default.
- Enabled only after generating a token from the Admin page.
- Token is shown once on create/rotate; if lost, rotate again.
- Scrape requests are rate-limited per client IP (in-memory, per app instance).
- The Admin page records the credential creation and rotation times plus the last successful use and a masked client origin. Bearer values and full client addresses are never stored in lifecycle metadata.
- An enabled token is marked stale after 30 days without a successful request. This is an operator warning only; the app never disables stale credentials automatically.
- Tokens created before lifecycle tracking remain valid and show `Usage unknown` until their next successful request or rotation.

Prometheus must send:

```text
Authorization: Bearer <token>
```

## Notification destinations

Notification Hooks are configured in Admin and stored in the application database. No notification environment variables are required.

Available destinations:

- Generic webhook: public endpoints must use HTTPS; HTTP is accepted only for localhost, private IPs, and `.local` or `.internal` hosts. Embedded URL credentials are rejected.
- Discord: an official HTTPS incoming-webhook URL.
- Telegram: a bot token plus a numeric chat ID or public `@channel` name.

Selectable events include completed updates, failed/skipped scheduled runs, backup restores, published/expired approvals, confirmed reboot requirements, failed backup exports/verification, and missing/stale recovery evidence. The new condition events require explicit selection; legacy selections stay unchanged. Successful update runs that install no packages do not send `update.complete` notifications.

Grouped reminders are disabled by default. Enable them in Admin and set an interval of 5–43200 minutes (default 1440); each destination receives at most one grouped summary of selected unresolved conditions per interval. These settings are stored in the existing encrypted-destination configuration, with no new environment variables. See [notification lifecycle and recovery behavior](usage.md#notification-hooks) for persistent episodes, destination changes, disablement, Server rename/deletion, and database restoration.

Destination secrets are encrypted before they are stored. Settings responses return only masked configuration state. Delivery is best-effort with bounded retries; Admin exposes per-destination test actions and safe diagnostics such as outcome, attempt count, HTTP status, duration, consecutive failures, and next retry time.

## Backup and restore

Backup/restore is managed in-app from `/admin` (session-authenticated).

Behavior:

- Export requires a passphrase (minimum 12 characters).
- Backup payload is encrypted and downloaded as `.slubkp`.
- Backup contains:
  - `servers.db`
  - `config.json`
  - optional `known_hosts` (controlled by export toggle)
- Restore requires the backup file + passphrase and applies immediately (no restart required).
- Verify checks the archive manifest and decryptability without replacing the database or other application state.
- Restore replaces `servers.db` and optional `known_hosts`; backup `config.json` is validated, but the local `config.json` remains in place and restored secrets are re-encrypted to its key.
- Recovery health treats successful export and verification evidence as stale after 7 days (168 hours).
- Recovery evidence comes from audit history and is retained for up to 90 days. Manual exports are downloaded to operator-managed storage. Optional automated backups use a separately configured destination and retention, described below.

What backup does not include:

- Reverse-proxy certificates/keys and external proxy config
- Container runtime settings outside the app data paths
- Any external secret manager state

### Automated backups

Automated backups extend Backup Operation Lifecycle with a daily or weekly schedule. They are disabled by default and use the same encrypted `.slubkp` format, verification pipeline and restore workflow as manual exports.

| Environment variable | Default | Meaning |
| --- | --- | --- |
| `DEBIAN_UPDATER_BACKUP_DIR` | unset | Absolute path to an existing dedicated archive directory; an external filesystem mount is supported. |
| `DEBIAN_UPDATER_BACKUP_PASSPHRASE_FILE` | unset | Absolute path to an existing private regular file containing the archive passphrase (12–4096 bytes, surrounding whitespace ignored). |

Set both variables together. The directory must be separate from the directories containing the active DB, config and writable known_hosts, and must not be writable by group or others (`0700` recommended). The secret must be outside the archive directory, use owner-only permissions (`0600`), and must not be an active data file. Neither path may contain a symlink. Local filesystem semantics must support hard links and directory fsync for atomic, no-overwrite publication. The app does not create or change permissions on deployment-provided directories or secret files.

Mount the secret separately from the archives and back it up in a separate secret store. Losing it makes those encrypted archives unrecoverable. The passphrase is never returned over HTTP, stored in SQLite, written to audit, or included in the archive. Rotation affects future archives; retain previous passphrases for older archives. The application creates a private `<database path>.backup-instance` identity file next to its DB; this file is also excluded from archives. Preserve it with the installation if retaining ownership of existing automated archives is desired. Losing or replacing it suspends automation and prevents the new identity from deleting the old instance's archives.

In **Admin → Backup & Restore → Automated encrypted backups**, choose daily/weekly cadence, application-local time, weekday (Sunday=0), inclusion of known_hosts and retention count (1–100). **Save and Validate** checks the local secret and destination and enables the chosen schedule. It also supports validation while scheduling is disabled, allowing **Run Control Backup** before enabling automatic runs. Disabling remains possible when local deployment validation fails.

**Timing and active work:** the worker checks once per minute and uses Application Time Interpretation. A nonexistent DST time is skipped; an ambiguous time runs at its first occurrence only. A timezone change rebases the next future occurrence. Active server work or a shared maintenance lease causes a visible five-minute deferral, bounded to one hour. A deferred control run is retried even with recurring scheduling disabled. An exclusive backup/restore blocks admission without accessing persistence being replaced. Occurrences older than one hour expire instead of producing an unbounded catchup queue. A claimed occurrence is persisted before export; restart reports an interrupted attempt and never replays the same claimed occurrence. The next future attempt remains scheduled.

**Publication and retention:** the existing Backup Operation Lifecycle retains exclusive Maintenance Coordination through export, destination copy, restore-readiness verification, publication and retention. Destination temporary and final files are private. Publication uses an atomic same-filesystem hard link that refuses to overwrite any existing filename, followed by temporary-link removal and directory fsync. Only registered temporary files owned by this installation are cleaned after failure or restart. A crash after publication may leave a safe extra archive; an unregistered file is never adopted or deleted. Retention runs only after new verified publication, matches the installation identity and recorded content digest, and preserves at least one verified archive. Changed files, symlinks, unrecognized archives and manual exports are not removed. Retention failure is visible and can leave more archives than configured.

The UI reports configuration/suspension, next attempt, last attempt, last verified success, deferral and failure. Audit actions `backup.automatic.succeeded`, `backup.automatic.failed`, and `backup.automatic.deferred` carry occurrence metadata for later notification consumers; this feature does not add notification delivery subscriptions. Published verification also updates existing recovery-health evidence.

Restoration preserves historical configuration and results but clears local approval, even when restoring into the same installation. Revalidate destination and secret in Admin before resuming. Existing manual exports and archives remain compatible. No S3/SFTP backend or remote-server filesystem snapshot is provided.

## Storage paths

The updater persists state in SQLite and encrypts SSH credentials at rest.

Defaults:

- DB:
  - `/data/servers.db` if `/data` exists (Docker volume is typical)
  - `./data/servers.db` otherwise
- Encryption key file:
  - `/data/config.json` when using `/data`
  - `./data/config.json` otherwise

Optional override:

- `DEBIAN_UPDATER_DB_PATH`

Example:

```bash
export DEBIAN_UPDATER_DB_PATH=/var/lib/simplelinuxupdater/servers.db
./webserver
```

On first run, the app may import legacy `servers.json` if it exists, then uses SQLite going forward.

## Job log storage and retention

Job output is stored as ordered SQLite fragments with its stream (`stdout`, `stderr`, or compatibility `combined`) and raw terminal control characters such as carriage returns. `jobs.logs_text` remains a compatibility preview and is bounded to 32 KiB; detailed logs are available through authenticated job reports and `GET /api/jobs/:id/logs`.

Defaults:

- Detailed logs for completed jobs are retained for 30 days.
- Each job may persist up to 2 MiB of detailed logs.
- When the size limit is reached, the first 64 KiB and newest output are kept, with an explicit marker replacing the removed middle.
- Active jobs are never expired or purged.
- Expiration removes detailed log content but preserves the job record, status, summary, metadata, and timestamps.

Environment variables:

- `DEBIAN_UPDATER_JOB_LOG_RETENTION_DAYS` (default `30`, allowed `1..3650`)
- `DEBIAN_UPDATER_JOB_LOG_MAX_BYTES` (default `2097152`, allowed `131072..1073741824`)

Invalid values are logged and fall back to the defaults. Retention runs during startup and then daily; it does not run `VACUUM`.

## Retry policy

Remote operations use exponential backoff retries for transient failures (SSH resets/timeouts, temporary transport issues, apt lock contention). Permanent failures (bad auth, host key verification, invalid config) fail fast.

Environment variables:

- `DEBIAN_UPDATER_RETRY_MAX_ATTEMPTS` (default `3`, allowed `1..10`)
- `DEBIAN_UPDATER_RETRY_BASE_DELAY_MS` (default `1000`, must be `> 0`)
- `DEBIAN_UPDATER_RETRY_MAX_DELAY_MS` (default `8000`, must be `> 0`)
- `DEBIAN_UPDATER_RETRY_JITTER_PCT` (default `20`, allowed `0..50`)
- `DEBIAN_UPDATER_SSH_COMMAND_TIMEOUT_SECONDS` (default `300`, allowed `1..1800`)

If invalid values are provided, the updater logs a warning and falls back to defaults.

For lock-protected APT commands, the SSH command timeout is an inactivity window
rather than a fixed wall-clock limit. Output on either stdout or stderr proves
progress and restarts that window without opening another SSH session. If the
window expires, the updater probes the APT/DPKG lock files on a second SSH
session. If a package-manager lock is still active, the command keeps running
and receives another full inactivity window. Each checkpoint records the
cumulative wait and the lock-holder PIDs in the job log. There is no fixed
checkpoint limit while the package manager continues to hold its lock. If the
probe no longer confirms an active lock after the command has already produced
progress, the updater allows one final bounded window for quiet package-manager
finalization. Without prior progress, or after that single grace window, it stops
waiting and marks the command outcome as unknown. It does not automatically
replay the mutating APT command, because the remote process may still be
completing after the SSH session closes.

## Automatic host-facts refresh

Host facts are refreshed automatically after successful updates and controlled
reboots. Successful scheduled scans also collect and persist facts through the
SSH session they already opened, without making a second connection.

A background worker covers hosts that do not have a successful scheduled scan.
It starts refreshing complete facts after 20 hours plus a stable per-host jitter
of up to 2 hours, checks every 15 minutes, and processes only one host at a time. A refresh
is skipped while the host is busy or an exclusive Backup operation is active.
Deferred attempts, failures, and incomplete disk/APT facts use exponential
backoff from 15 minutes to 6 hours and are recorded as `server.facts.refresh` audit events with source
`automatic_periodic`. The manual **Refresh host facts** action remains available.

Environment variable:

- `DEBIAN_UPDATER_HOST_FACTS_AUTO_REFRESH_ENABLED` (`true|false`, default `true`)

Disabling the worker does not disable the facts capture performed by successful
updates, controlled reboots, or scheduled scans.

## Post-update checks

After a successful upgrade, the updater can run health checks and optionally block completion.

Environment variables:

- `DEBIAN_UPDATER_POSTCHECKS_ENABLED` (default `true`)
- `DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_APT_HEALTH` (default `true`)
- `DEBIAN_UPDATER_POSTCHECK_BLOCK_ON_FAILED_UNITS` (default `true`)
- `DEBIAN_UPDATER_POSTCHECK_REBOOT_REQUIRED_WARNING` (default `true`)
- `DEBIAN_UPDATER_POSTCHECK_CMD` (optional custom command; blocking when configured)

See [usage.md](usage.md) for behavior details and interpretation of failures.

## Known hosts handling

The app maintains SSH known-hosts entries and can scan/trust a host key from the UI before first connection.

Edit Server also provides **Known host management** actions:

- Check whether the current host/port is already present in `known_hosts`.
- Clear the matching known-host entry for the current host/port.
- Save bypasses redundant host-key trust prompts when the same host/port was already confirmed as trusted in the active edit session.

Override search path:

- `DEBIAN_UPDATER_KNOWN_HOSTS` accepts a native path list: separate paths with
  `:` on Unix and `;` on Windows. For example,
  `/data/known_hosts:/etc/ssh/ssh_known_hosts` on Unix or
  `C:\Users\Alice\.ssh\known_hosts;D:\ssh\known_hosts` on Windows.
- The first non-empty configured path is the managed write target; all configured
  paths are considered when reading host-key trust.

Default behavior:

- When using Docker with `/data`, the default known-hosts file is typically `/data/known_hosts`.
- When running locally, it is stored next to the DB in the local data directory.
- Host-key verification can read existing fallback known-hosts files, but UI trust/clear, backup, and restore writes use the first configured path or the app data `known_hosts` file.

## Environment file (.env)

For Docker, `.env` is not automatically loaded unless you pass it:

```bash
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.16
```

## Installed-package vulnerability scans

`DEBIAN_UPDATER_VULNERABILITY_SCAN_ENABLED` defaults to `true` and disables both manual and periodic installed-package scans when `false`. Approval-time CVE enrichment remains independent. `DEBIAN_UPDATER_VULNERABILITY_SCAN_INTERVAL_HOURS` defaults to `24` (range `0–8760`); `0` disables the periodic worker. It checks persisted attempt timestamps once per minute, beginning one minute after startup, and defers disabled or busy hosts and exclusive Maintenance Coordination. Failed attempts are retried at the configured interval, or manually.

`DEBIAN_UPDATER_VULNERABILITY_SCAN_TIMEOUT_SECONDS` defaults to `300` (range `10–900`). At most two scans run concurrently across manual and periodic admission; the periodic worker visits servers sequentially. There is no unbounded task queue. A scan accepts at most 20,000 installed packages, a 4 MiB inventory, 20,000 findings, and 1,000 advisory detail lookups. OSV requests have a 30-second deadline, an 8 MiB response limit, and at most 100 pages per batch. Exceeding inventory or finding limits preserves the previous accepted result and reports failure.

Optional `DEBIAN_UPDATER_VULNERABILITY_TARGET_LOW_DAYS`, `..._MEDIUM_DAYS`, `..._HIGH_DAYS`, and `..._CRITICAL_DAYS` accept `0–3650` days. Unset or zero disables that objective. The target is measured from `first_seen`; unknown severity has no target. Targets are displayed as operator objectives, never as approval gates or CVSS scores. Invalid scan configuration rejects startup. Freshness uses the configured interval, or 24 hours when periodic scanning is disabled.

## Application post-check profiles

Admin → Application Checks stores versioned profiles in SQLite. Assign a profile directly to exact Server names or to tags. A direct assignment wins. Several matching tag profiles are a configuration conflict: assign one profile directly or change the tag assignments. Conflicts and unavailable configuration abort updates before package mutation. Server rename preserves direct assignments; resolve any conflicting direct assignment to the new name before renaming. Deletion removes direct assignments. Profiles are included in database backups and restored without a process cache.

Each profile has 1–10 checks and a total recovery budget of at most 600 seconds. Each check has a stable ID, name, type, blocking/warning mode, recovery timeout (1–300 seconds) and retry interval (1–30 seconds, no longer than the timeout). The first attempt runs immediately. The two supported types are `systemd_active` (an exact unit such as `nginx.service`) and `http_status` (HTTP GET with an expected status from 100 to 599). Checks execute on the remote Server with its existing SSH account, in the existing Host Maintenance Session. Localhost refers to that Server. No extra sudo permissions are granted and no tool, service or configuration is changed.

HTTP checks require remote `curl`; systemd checks require `systemctl`. Missing tools fail the check without installation. URLs must use HTTP(S), without embedded credentials, query parameters, fragments or whitespace. Use a health endpoint that contains no secrets. Curl's user configuration, URL globbing and proxy environment are disabled; TLS verification stays enabled, redirects are not followed and response bodies are discarded. Each request has a five-second limit within the check's recovery budget. Only safe status/result facts, attempt counts and durations are recorded; remote command output and error text are excluded.

Profiles run after an actual upgrade, alongside the existing APT/systemd/reboot checks and `DEBIAN_UPDATER_POSTCHECK_CMD`. Their blocking/warning modes are independent of `DEBIAN_UPDATER_POSTCHECKS_ENABLED`; disabling the global legacy checks does not disable assigned application expectations. Installations without profiles retain existing behavior. An update with no package mutation does not run application checks. The effective profile and revision are shown with the pending approval and captured in job/audit metadata. If profile content or assignment changes during approval, the refreshed expectations require a new approval before mutation, including previously automatic approvals. A profile captured for an accepted operation remains fixed during the upgrade.

A blocking failure marks the job and Scheduled Run failed, stopping subsequent rollout waves. Warning failures remain visible in logs, job detail and audits but allow success. Manage's Server editor previews the saved effective profile and links to Admin. Save inventory/tag and profile changes before using **Run read-only test**; this tests the saved effective profile on the chosen Server without package or service changes. Profile saves use optimistic revision checks: after a conflict, discard or copy the draft, then reload before saving.

Read-only tests reserve the Server until completion, preventing concurrent maintenance and inventory changes. A check's recovery timeout does not invalidate later checks: a transport closed by that deadline is reconnected before the next probe. Interrupted updates retain completed and partial application results in both job and audit metadata.

The read-only test response supports the full recovery budget, including profiles that take longer than the ordinary one-minute HTTP response deadline. The test itself is bounded to eleven minutes.
