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
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.5.0
```

The image explicitly sets `DEBIAN_UPDATER_LISTEN_ADDR=:8080`. To limit access on
the Docker host itself, publish to a specific host address, for example
`-p 127.0.0.1:8080:8080` behind a local reverse proxy.

## GHCR images

Release tags publish images to GitHub Container Registry:

- `ghcr.io/nolife141-teams/simplelinuxupdater:vX.Y.Z`
- `ghcr.io/nolife141-teams/simplelinuxupdater:latest`

Example:

```bash
docker pull ghcr.io/nolife141-teams/simplelinuxupdater:v0.5.0
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

Follow the [v0.4.16 to v0.5.0 upgrade, restore and rollback guide](upgrading.md)
for concrete Docker and binary archive commands. Export and verify a backup
before stopping the old process, retain its matching image/binary and assets,
then deploy the candidate with the same data paths. The initial account becomes
administrator, setup stays closed and old sessions are signed out once. New
sessions survive subsequent restarts. Review recovered jobs, renew interrupted
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
scheduler, host facts, vulnerability, backup notification, audit-pruning and job-log-pruning worker tick,
completion and error timestamps; running/enabled state; backup verification age;
persisted pending/claimed, retrying and failed notification rows; and observed
main DB/WAL and available filesystem bytes (capacity currently on Linux).
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

The existing Metrics Access Credential also protects the added
`slu_installation_*` Prometheus gauges. Existing `simplelinuxupdater_*` series are
preserved during normal operation. During exclusive maintenance `/metrics` and the diagnostics API return a minimal
503 maintenance status before authentication or database access; all metrics are
temporarily absent. This preserves the existing Metrics Access Credential
admission boundary and prevents credential reload/usage writes while persistence
is being replaced. Use `/readyz` for monitoring these controlled windows.
Worker labels are bounded to `scheduler`, `host_facts`, `vulnerabilities`, `backup`
`notifications`, `audit_pruner` and `job_log_pruner`; no accounts, jobs, Servers, tokens, destinations or paths
are added as labels. Timestamps and observed error counts come from service
owners using their injected clocks. Missing timestamps, unavailable counts and
unavailable storage are omitted; the `_available` gauges identify unknown backup
and notification observations. The panel and metrics do not estimate queue depth
from timer cadence or infer progress from goroutine existence.

Example alert expressions (tune windows to deployment cadence):

```promql
slu_installation_ready == 0 and slu_installation_maintenance == 0
slu_installation_storage_available_bytes < 1024 * 1024 * 1024
slu_installation_notifications_retrying > 0
slu_installation_notifications_failed > 0
slu_installation_backup_verified_age_seconds > 7 * 24 * 3600
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
