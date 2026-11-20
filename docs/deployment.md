[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Deployment

## Table of contents

- [Docker (recommended)](#docker-recommended)
- [GHCR images](#ghcr-images)
- [Binary deployment](#binary-deployment)
- [First-run setup and login](#first-run-setup-and-login)
- [Metrics scraping](#metrics-scraping)
- [Reverse proxy and HTTPS](#reverse-proxy-and-https)
- [Data persistence](#data-persistence)
- [Upgrade procedure](#upgrade-procedure)
- [Single-instance operation](#single-instance-operation)

## Docker (recommended)

Use a named volume for persistence:

```bash
docker run --env-file .env -p 127.0.0.1:8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.5.8
```

The example publishes the UI on IPv4 loopback for first-run setup and use with
a reverse proxy on the same host. The image listens on `:8080` inside the
container. To serve the UI directly on a trusted LAN or VPN, replace `127.0.0.1`
in the publish flag with the Docker host’s IP on that network and restrict
access to intended clients. Keep `/setup` private while creating the first
administrator.

## GHCR images

Release tags publish images to GitHub Container Registry:

- `ghcr.io/nolife141-teams/simplelinuxupdater:vX.Y.Z`
- `ghcr.io/nolife141-teams/simplelinuxupdater:latest`

Example:

```bash
docker pull ghcr.io/nolife141-teams/simplelinuxupdater:v0.5.8
```

## Binary deployment

If running as a binary, ensure the process can read/write the data directory and can read the `templates/` and `static/` directories at runtime.

Consider using a process supervisor (systemd) on the updater host.

Example systemd unit (adjust paths and env vars):

```ini
[Unit]
Description=SimpleLinuxUpdater
After=network-online.target
Wants=network-online.target

[Service]
WorkingDirectory=/opt/simplelinuxupdater
ExecStart=/opt/simplelinuxupdater/webserver
Restart=on-failure
Environment=DEBIAN_UPDATER_SESSION_COOKIE_SECURE=true
Environment=DEBIAN_UPDATER_LISTEN_ADDR=127.0.0.1:8080

[Install]
WantedBy=multi-user.target
```

## First-run setup and login

After deployment:

1. Open `/setup` (or `/`) and create the local admin user.
2. Sign in at `/login`.
3. Use `/api/auth/logout` or the UI logout action to end sessions.

The first account is an administrator. Administrators can create named reader,
operator and administrator accounts; optional TOTP MFA, isolated sessions and
scoped personal API tokens follow the [Accounts and API contract](personal-api.md).

Before that user exists, `/setup` is the account-creation boundary. If the
listener is non-loopback, firewall or proxy it so only the intended LAN, VPN, or
reverse-proxy clients can reach first-run setup. Do not expose it directly to the
public internet.

## Metrics scraping

`/metrics` is disabled by default. Generate a Metrics API token from `/admin`, then configure your scraper with that token.

Prometheus example:

```yaml
scrape_configs:
  - job_name: simplelinuxupdater
    metrics_path: /metrics
    static_configs:
      - targets: ["simplelinuxupdater:8080"]
    authorization:
      type: Bearer
      credentials: YOUR_METRICS_TOKEN
```

## Reverse proxy and HTTPS

The app does not terminate TLS by default. For production:

- Put it behind a reverse proxy (nginx, Caddy, Traefik) for HTTPS.
- Set `DEBIAN_UPDATER_SESSION_COOKIE_SECURE=true` under HTTPS.
- Set `DEBIAN_UPDATER_TRUSTED_PROXIES` to the reverse proxy IP/CIDR if you want audit logs and rate limits to use the original client IP from forwarded headers.
- Restrict access to your LAN/VPN.

## Data persistence

Docker convention:

- `/data/servers.db`: SQLite DB
- `/data/config.json`: encryption key
- `/data/known_hosts`: SSH known-hosts (default)

If an attacker obtains both the SQLite DB and the encryption key file, stored secrets can be decrypted. See [security.md](security.md).

## Upgrade procedure

Follow the [v0.5.8 upgrade, restore and rollback guide](upgrading.md)
for concrete Docker and binary archive commands. Export and verify a backup
before stopping the old process, retain its matching image/binary and assets,
then deploy the candidate with the same data paths. Setup stays closed. When
upgrading from v0.4.16 or earlier, the initial account becomes administrator and
old sessions are signed out once. Existing v0.5.x sessions retain their normal
lifecycle across application restarts. Review recovered jobs, renew interrupted
approvals and update the [managed helper](usage.md#managed-helper-installation)
before resuming maintenance.

Rollback uses the pre-upgrade backup in a separate old-version installation;
starting the old binary on the migrated database is not a supported downgrade.
Prefer a published immutable image digest or version tag. The `latest` tag
advances according to the [publication policy](ci-and-release.md#publication-sequence).

## Single-instance operation

Run one active SimpleLinuxUpdater process per database and data directory. SQLite persistence, in-memory action coordination, scheduler ownership, rate limiting, and dashboard event delivery are designed for a single application instance; multiple replicas sharing `/data` are not a supported high-availability topology.

## Installation health and readiness

Use `/healthz` for liveness and `/readyz` for traffic admission. Both accept GET
and HEAD, require no credential, and return only `{"status":"..."}`. Liveness is
200 `alive` while the HTTP process responds. Readiness is 200 `ready` after
Runtime Composition initialization (including a fresh installation before account
setup) and a read-only `sqlite_schema` query against the **current** DB provider.
The SQLite query has a 250 ms context deadline, including connection pool waits.
The probe temporarily disables the borrowed connection's SQLite busy wait so a
file lock fails immediately, then restores that connection setting before returning
it to the pool. These connection-local settings do not write the database.
No probe opens a session, writes a record, migrates, repairs SQLite, retries a
maintenance release, contacts a Server, or starts a notification or backup.

Readiness returns 503 with one of these states:

| State | Meaning | Operator action |
| --- | --- | --- |
| `initializing` | Runtime owners are not initialized | Allow startup grace; the listener may not yet accept HTTP during startup |
| `reloading` | Persistence replacement/runtime rehydration is incomplete | Allow restore to finish; a failed reload remains unavailable |
| `maintenance` | Exclusive Maintenance Coordination owns or is acquiring admission | Allow the controlled operation to finish |
| `recovery_required` | Restore recovery was retained by Maintenance Coordination | Follow the documented manual recovery procedure |
| `unavailable` | Current DB is absent, closed, cannot be read, or exceeds the query budget | Inspect disk availability and process logs |
| `stopping` | Graceful shutdown has begun | Drain traffic; process liveness does not imply admission |

The health endpoints bypass session and maintenance middleware. Restore cannot
be blocked by a waiting probe: admission is attempted without waiting, and a
successful read lease lasts only for the bounded SQLite read. Restore invalidates
readiness before replacing persistence and only successful rehydration restores
it. Shutdown makes readiness permanently unavailable before HTTP draining.
During HTTP server closure a connection failure is expected for either endpoint.
There is no automatic restart, retry of maintenance, or destructive watchdog.

The image HEALTHCHECK runs `/app/webserver healthcheck` as the application user,
including when the container uses `--user app`. This HTTP-only command runs before
installation locking, SQLite opening and configuration initialization. It uses
`DEBIAN_UPDATER_LISTEN_ADDR` (default for the image `:8080`; native default
`127.0.0.1:8080`), replaces wildcard IPv4/IPv6 hosts with the corresponding loopback,
disables proxies/redirects and has a two-second total request deadline. A configured
specific bind is probed at that bind. Docker allows a 15-second start period,
checks every 30 seconds, and reports unhealthy after three failures. An unhealthy
container during a long controlled restore is expected; do not attach an automatic
restart policy to this state. `healthcheck` exits 0 for readiness HTTP 200 and 1
for invalid arguments/configuration, transport failure or any other status.

Administrators using a cookie session can open **Admin → Installation Health**
or `GET /api/installation/health`. Reader/operator cookies and all personal API
credentials are rejected. The panel uses the existing Admin Page Interaction
snapshot lifecycle, lazy loading, explicit refresh and stale/retry indication.
It separates local installation facts from Server health. It shows observed
scheduler, host facts, vulnerability, backup, notification, audit-pruning and job-log-pruning worker tick,
completion and error timestamps; running/enabled state; backup verification age;
persisted pending/claimed, retrying and failed notification rows; and observed
main DB/WAL and available filesystem bytes (capacity currently on Linux).

The interactive restore review estimates additional space for the current
installation. It checks temporary envelope decoding separately. Once the
compressed archive is authenticated, it uses bounded tar entry sizes to budget
extraction, a prepared copy of SQLite and a SQLite journal of comparable
size. The encrypted upload and the decrypted compressed file already occupy
space when that estimate is measured. The replacement budget includes an
atomic write of the staged database and any archived host-key file, the
rollback snapshots of the current SQLite database and sidecars and optional
host-key file, and space to write the rollback copies back to their targets.
The estimate groups requirements by filesystem device and adds a margin of
8 MiB or 10 percent per device, whichever is greater. Available bytes exclude
blocks reserved for privileged callers. The output identifies storage roles
without exposing filesystem paths.

Restore checks space before extracting and again before writing rollback
snapshots. After sealing the snapshots it checks the remaining space with the
actual prepared database size, before publishing the recovery marker or
replacing live files. An insufficient volume stops replacement. When space
cannot be measured, the review shows unknown capacity. Available space may
change between a check and a write. A storage error still uses the normal
rollback and recovery procedure. Free space on the preparation volume
(`TMPDIR`), the volume holding SQLite and its rollback snapshots, or the
volume containing the host-key file, as indicated. Reverify before retrying.
Scheduled verification checks archive integrity and format for later use on
any compatible installation.

A successful notification condition evaluation reads and validates its inputs
and commits any state changes, including when no delivery is due. A later
evaluation failure marks this observation as degraded and retains its last
success time. When backup recovery evidence cannot be read, the evaluation
is degraded and available observations such as APT incidents can still be
admitted and delivered. The evaluation starts unobserved after startup or
persistence reload. The latest persisted HTTP success for a queued delivery survives
process restarts and outbox history pruning. Manual test deliveries use their
own diagnostics. The queue runs at most one attempt per destination type and
three attempts total. Eligible entries stay in queue order within each type,
and retries wait for their scheduled time. A slow request leaves the other
destination types and condition evaluation free to progress. Persistence
replacement waits for all admitted deliveries to finish and write their
outcomes. Restore isolation suspends all destination types until resumed.
The oldest queued delivery age includes pending, claimed and retrying rows. An empty queue has no age. Worker tick and progress timestamps
remain separate observations.
A completed empty worker sweep is progress, not proof of a successful Server
operation. Disabled configuration is expected. Never observed/unavailable is
unknown and is never displayed as a zero age. Error counts reset on restart;
failed outbox rows are subject to existing retention. Notification enabled means
at least one destination is enabled. Backup enabled means its schedule is enabled;
restored schedules stay suspended until their local destination and secret are
revalidated through the existing Backup Operation Lifecycle. Reading diagnostics
does not read destination secrets or validate a backup archive. Last verified
backup time is accepted audit evidence and does not prove that an archive still
exists or that it is stored off host. Unknown storage observations are nullable.

**Local backup archives** reports the last availability observation of registered
automated archives. The worker checks at startup and every five minutes, up to
200 registrations per pass. The backup catalog shows up to 200 registrations,
including up to 100 unavailable archives preserved by retention alongside the
configured number of healthy archives. The worker compares directory and file
metadata and confirms read access without reading the passphrase or hashing
archives. An unreadable file yields an unknown availability observation. Each
confirmed loss opens one delivery episode under the existing
backup verification failure event. Successful full re-verification restores the
archive's coverage and allows a future loss to open a new episode. An inaccessible
or replaced mount yields an unavailable observation. A surviving verified archive
still contributes to local coverage. Manual off-host backups are outside this observation.

**Newest verified local recovery point** reports the age of the data in the
newest registered archive that is present and restore-verified. The snapshot
timestamp is recorded in the encrypted manifest. Reverifying an archive can
recover a recorded capture date and refreshes the verification time separately.
Archives created before capture dates were recorded can verify successfully
with unknown capture dates. Data older than seven days is stale. Losing the
newest archive falls back to the newest confirmed surviving point. An unobserved
mount or a verified archive with an unknown capture date can leave the recovery
point unknown. After a process restart, storage and archives need a fresh
observation before they can contribute to local coverage. Audit history still
reports export and verification outcomes, including manual backups. Off-host
availability is outside the local point calculation.

The integrity worker reads at most one registered encrypted archive every five
minutes, up to the existing archive size limit. It prioritizes the newest
available recovery point once an hour and checks other candidates after
24 hours, oldest attempt first. Unavailable reads become eligible for retry
after 30 minutes. An attempt has a 45-second budget and a stalled filesystem
read occupies its slot until it returns. The worker compares the streamed
SHA-256 with the registered digest and checks the file and mount identity.
The backup catalog shows this digest check separately from the capture time
and the full verification. A mismatch opens a deduplicated archive loss
episode and coverage falls back to the best remaining point. An unavailable
read records uncertainty and leaves the last completed check timestamp in
place. The worker uses the encrypted bytes directly.

The existing Metrics Access Credential also protects the added
`slu_installation_*` Prometheus gauges. Existing `simplelinuxupdater_*` series are
preserved during normal operation. During exclusive maintenance `/metrics` and the diagnostics API return a minimal
503 maintenance status before authentication or database access; all metrics are
temporarily absent. This preserves the existing Metrics Access Credential
admission boundary and prevents credential reload/usage writes while persistence
is being replaced. Use `/readyz` for monitoring these controlled windows.
The compatibility `vulnerabilities` worker label is always disabled; standalone scanning has been retired.
Worker labels are bounded to `scheduler`, `host_facts`, `vulnerabilities`, `backup`
`notifications`, `audit_pruner` and `job_log_pruner`. Notification destination labels are limited
to the integration types `webhook`, `discord` and `telegram`. Accounts, jobs, Servers, tokens,
endpoint identities and paths are excluded from labels. Timestamps and observed error counts come from service
owners using their injected clocks. Missing timestamps, unavailable counts and
unavailable storage are omitted; the `_available` gauges identify unknown backup
and notification observations. The panel and metrics do not estimate queue depth
from timer cadence or infer progress from goroutine existence.

`slu_installation_backup_archives_observed_timestamp_seconds` tracks the
last registered-archive observation. `slu_installation_backup_archive_storage_available`
and `slu_installation_backup_local_archive_coverage` are omitted when
their corresponding facts are unknown. Verified, lost and unknown archive
counts are exported for observable storage without file identifiers or paths.

`slu_installation_backup_local_recovery_point_timestamp_seconds` and
`slu_installation_backup_local_recovery_point_age_seconds` describe capture time.
The `_available` gauge reports known presence or absence, and the `_stale` gauge
describes the freshness of a confirmed point. Unknown observations omit samples.

`slu_installation_notifications_condition_evaluation_degraded` is 1 when
the latest observed evaluation failed and 0 when it succeeded. This gauge and
`slu_installation_notifications_last_condition_evaluation_timestamp_seconds`
appear after an evaluation result and a successful evaluation, respectively.
`slu_installation_notifications_last_successful_delivery_timestamp_seconds`
tracks persisted HTTP success.
`slu_installation_notifications_oldest_pending_age_seconds` is present when
a nonterminal row has an observed creation time.

Installation Health and the `slu_installation_notifications_destination_*` series
report delivery separately for Webhook, Discord and Telegram. The last queued
attempt and HTTP success refer to the current destination configuration. The last
success for a different configuration is reported as historical. The queued success
survives a restart and history pruning. A manual test has its own result and timestamp.
The age by destination includes pending, claimed and retrying rows. A terminal-only
queue has no age sample. Unknown results have state `unknown` and omit their timestamps.
Upgrades can recover per-configuration evidence from retained, bound delivery rows.
Older results without that binding remain unknown at destination scope. When
legacy or restored observations at the same timestamp disagree, the latest
attempt's outcome is unknown and any confirmed HTTP success is still reported.

Example alert expressions (tune windows to deployment cadence):

```promql
slu_installation_ready == 0 and slu_installation_maintenance == 0
slu_installation_storage_available_bytes < 1024 * 1024 * 1024
slu_installation_notifications_retrying > 0
slu_installation_notifications_failed > 0
slu_installation_notifications_condition_evaluation_degraded == 1
slu_installation_notifications_destination_oldest_pending_age_seconds > 1800
slu_installation_backup_verified_age_seconds > 7 * 24 * 3600
slu_installation_backup_local_recovery_point_available == 0
slu_installation_backup_local_recovery_point_stale == 1
(time() - slu_installation_worker_last_tick_timestamp_seconds{worker="scheduler"} > 180)
  and on(worker) slu_installation_worker_enabled{worker="scheduler"} == 1
```

Use an external HTTP probe for availability when `/metrics` is unavailable or cannot authenticate;
monitor absent backup evidence as well as old evidence. Do not alert on a disabled
worker or a never-observed timestamp without a startup/cadence allowance. A live
process can be unready, and a ready process can have delivery failures or stale
backup evidence. This remains a single-instance SQLite installation: no HA,
replica coordination or cross-process watchdog is provided. The installation
lock, verified backups, operator monitoring and manual recovery remain required.
