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
docker run --env-file .env -p 8080:8080 -v debian-updater-data:/data ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.3
```

## GHCR images

Release tags publish images to GitHub Container Registry:

- `ghcr.io/nolife141-teams/simplelinuxupdater:vX.Y.Z`
- `ghcr.io/nolife141-teams/simplelinuxupdater:latest`

Example:

```bash
docker pull ghcr.io/nolife141-teams/simplelinuxupdater:v0.4.3
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

[Install]
WantedBy=multi-user.target
```

## First-run setup and login

After deployment:

1. Open `/setup` (or `/`) and create the local admin user.
2. Sign in at `/login`.
3. Use `/api/auth/logout` or the UI logout action to end sessions.

Only one local user is supported by design.

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

1. Export an encrypted backup from Admin and verify the downloaded `.slubkp` archive.
2. Record the currently deployed image tag or binary version so rollback is explicit.
3. Read the target release entry in [`CHANGELOG.md`](../CHANGELOG.md).
4. Stop the existing process or container cleanly.
5. Deploy the new version with the same `/data` volume or binary data directory.
6. Confirm login, server inventory, Status health, scheduled policies, notification settings, and `/metrics` credential state before starting maintenance.

Prefer immutable `vX.Y.Z` image tags for controlled deployments. The `latest` tag follows the newest published release and is less suitable when rollback reproducibility matters.

## Single-instance operation

Run one active SimpleLinuxUpdater process per database and data directory. SQLite persistence, in-memory action coordination, scheduler ownership, rate limiting, and dashboard event delivery are designed for a single application instance; multiple replicas sharing `/data` are not a supported high-availability topology.
