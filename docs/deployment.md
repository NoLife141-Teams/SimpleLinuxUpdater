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
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.16
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
docker pull ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.16
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

Follow the [verified v0.4.16 upgrade, restore and rollback guide](upgrading.md)
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
