[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Security

## Table of contents

- [Summary](#summary)
- [Threat model](#threat-model)
- [Authentication model](#authentication-model)
- [Metrics endpoint protection](#metrics-endpoint-protection)
- [Notification destination security](#notification-destination-security)
- [Backup and restore artifacts](#backup-and-restore-artifacts)
- [Encryption at rest](#encryption-at-rest)
- [Remote sudo behavior](#remote-sudo-behavior)
- [SSH key handling](#ssh-key-handling)
- [Recommended hardening](#recommended-hardening)

## Summary

This tool:

- Accepts SSH credentials (passwords and private keys) through the UI
- Can install the root-owned `/usr/local/sbin/simplelinuxupdater-root-helper` and `/etc/sudoers.d/simplelinuxupdater` on remote hosts when enabling passwordless apt
- Runs restricted apt/dpkg, lock-inspection, repair, and controlled reboot commands via `sudo` on non-root remote hosts
- Can send operational notifications to operator-configured external destinations

Treat it as privileged infrastructure.

## Threat model

- Intended for trusted LAN/VPN environments only.
- No TLS termination by default; use a reverse proxy for HTTPS.
- Single-user local authentication is intended for small trusted teams/homelabs.

## Authentication model

SimpleLinuxUpdater uses:

- First-run setup at `/setup` to create one local admin user
- Argon2id password hashing (`auth_users` table in SQLite)
- Server-side sessions stored in SQLite (`sessions` table)
- Session cookies with `HttpOnly` and `SameSite=Lax`
- Response hardening headers:
  - `Content-Security-Policy`
  - `X-Content-Type-Options: nosniff`
  - `Referrer-Policy: strict-origin-when-cross-origin`
  - `X-Frame-Options: DENY`
  - `Strict-Transport-Security` (HSTS) when requests are HTTPS (`TLS` or `X-Forwarded-Proto: https`)
  - UI assets enforce strict CSP by design:
    - No inline `<script>` blocks
    - No inline `<style>` blocks
    - No inline `on*=` handlers
    - No inline `style=` attributes
  - CI includes strict-CSP template checks and fails on inline regressions

Setup enforces a password policy for the local admin user in `auth_users`:

- Minimum length: 10 characters
- Maximum length: 64 characters
- Must include at least one letter and one digit
- Username is required, maximum 64 characters, and limited to supported SSH-safe characters

Session hardening options:

- Set `DEBIAN_UPDATER_SESSION_COOKIE_SECURE=true` when running behind HTTPS.
- Optionally set `DEBIAN_UPDATER_SESSION_IDLE_TIMEOUT_HOURS` (hours). Default is `0`/unset, which means no additional idle timeout is applied.
- Set `DEBIAN_UPDATER_TRUSTED_PROXIES` only to proxies you control. It controls whether forwarded client IP headers affect audit logs and rate limiting.

## Metrics endpoint protection

`/metrics` is protected by a bearer token, separate from UI sessions.

Configure from the Admin page (`/admin`):

- Generate/rotate the Metrics API token in-app
- Store the one-time token output securely for your scraper
- Disable token to make `/metrics` return `404`
- Review the safe lifecycle status in Admin: creation, rotation, last successful use, and masked origin
- Treat the 30-day stale state as a scraper-health warning; stale credentials remain enabled until an operator rotates or disables them

Scrapers must send:

```text
Authorization: Bearer <token>
```

Operational note:

- Auth and metrics rate limiting is in-memory per process. In multi-instance deployments, enforce global limits at the load balancer/API gateway.

## Notification destination security

Generic webhook URLs, Discord webhook URLs, Telegram bot tokens, and Telegram chat IDs are sensitive configuration. They are encrypted before SQLite persistence and are returned to the UI only as masked state.

- Public generic webhook endpoints must use HTTPS. Plain HTTP is accepted only for localhost, private IP addresses, and `.local` or `.internal` hosts.
- Embedded URL credentials are rejected.
- Discord accepts official HTTPS webhook URLs; Telegram delivery uses the configured bot token and chat identifier.
- Outbound notification payloads contain sanitized operational facts, not stored SSH credentials, private keys, metrics tokens, backup passphrases, or notification secrets.
- Test delivery sends a real external message. Verify the destination and authorization before using it.
- Delivery is best-effort with bounded retries and does not change the result of the triggering maintenance action.

## Backup and restore artifacts

Backup export is available from `/admin` and is session-authenticated.

- Exported backup files (`.slubkp`) are encrypted with a user-provided passphrase.
- Payload includes `servers.db`, `config.json`, and optional `known_hosts`.
- Passphrases are not stored by the app.
- Restoring a backup immediately replaces `servers.db` and optional `known_hosts`; backup `config.json` is validated while local `config.json` remains in place.

Recommendations:

- Store backup files and passphrases separately in secure systems.
- Treat backup files as sensitive infrastructure data even when encrypted.
- Back up external reverse-proxy TLS/private keys separately (outside app scope).

## Encryption at rest

Secrets are stored encrypted in SQLite.

Files:

- SQLite DB: `/data/servers.db` (Docker) or `./data/servers.db` (local)
- Encryption key: `/data/config.json` (Docker) or `./data/config.json` (local)

If an attacker obtains both the database and the encryption key file (or the mounted volume), they can decrypt stored secrets.

## Remote sudo behavior

The updater runs apt operations directly when the SSH user is root. For non-root SSH users with the current policy, it uses `sudo -n /usr/local/sbin/simplelinuxupdater-root-helper <operation> ...` over SSH. Before migration, the lock check alone can fall back to the previous exact `/usr/bin/fuser` command when the typed helper is denied; this compatibility path never grants or invokes generic APT commands.

The UI can enable/disable passwordless apt by installing/removing two owner-marked, root-owned files:

- `/usr/local/sbin/simplelinuxupdater-root-helper`
- `/etc/sudoers.d/simplelinuxupdater`

The generated sudoers rule does not grant `/usr/bin/apt`, `/usr/bin/apt-get`, a shell, or `/usr/bin/env` directly. It permits only typed helper operations. The helper has an internal allowlist for metadata refresh, standard upgrade, full upgrade, autoremove, repair, the two lock probes, package-state checks, selected-package install/upgrade, and controlled reboot. Each operation constructs its own fixed command; it does not accept a raw shell command, arbitrary APT options, `-o`, APT hooks, or filesystem paths. APT and dpkg start with an empty environment rebuilt from a fixed PATH and the documented non-interactive variables, so inherited APT configuration variables cannot alter the operation.

Selected-package operations accept only Debian package selectors matching a lowercase package name, optionally followed by one validated architecture suffix such as `openssl:amd64`. The helper inserts `--` itself and rejects options, hooks, metacharacters, extra colons, version expressions, and arbitrary paths before invoking APT.

**Enable apt** stages both files, validates the sudoers rule with `visudo`, and replaces an existing destination only when it is a root-owned regular file with the SimpleLinuxUpdater owner marker. **Disable apt** applies the same checks before removal. Symlinks and unmarked files are refused.

Versions before this boundary used `/etc/sudoers.d/apt-nopasswd`. On the next **Enable apt** or **Disable apt**, SimpleLinuxUpdater removes that legacy file only when its complete content exactly matches the previous rule generated for the same SSH user. Any customized, unrecognized, non-root-owned, or symlinked legacy file is left untouched and the action fails with manual migration guidance. Inspect an ambiguous legacy file yourself; do not copy its generic `apt`/`apt-get` grants into the new rule.

After enabling, validate the target policy with:

```bash
sudo visudo -c
sudo -l -U <ssh-user>
```

## SSH key handling

- SSH private keys can be uploaded through the UI (global or per-server).
- Uploaded key files are limited in size (64KB) to reduce accidental large uploads.

## Recommended hardening

- Do not expose the UI to the public internet.
- Restrict access with a VPN and/or reverse proxy controls.
- Use HTTPS and set `DEBIAN_UPDATER_SESSION_COOKIE_SECURE=true`.
- Store the generated Metrics API token in a secret manager and rotate it periodically.
- Protect the persisted volume (`/data`) like a secret.
