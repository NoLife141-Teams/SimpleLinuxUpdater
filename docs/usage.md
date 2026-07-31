[README](../README.md) | [Installation](installation.md) | [Configuration](configuration.md) | [Usage](usage.md) | [Deployment](deployment.md) | [Security](security.md) | [Troubleshooting](troubleshooting.md) | [Architecture](architecture.md) | [Contributing](contributing.md)

# Usage

## Table of contents

- [Add and manage servers](#add-and-manage-servers)
- [Authentication flow](#authentication-flow)
- [Trust a host key](#trust-a-host-key)
- [Run updates with approval](#run-updates-with-approval)
- [CVE-aware pending approval](#cve-aware-pending-approval)
- [Logs and status](#logs-and-status)
- [Scheduled canary and wave rollouts](#scheduled-canary-and-wave-rollouts)
- [Audit trail](#audit-trail)
- [Observability and metrics](#observability-and-metrics)
- [Backup and restore](#backup-and-restore)
- [How SimpleLinuxUpdater compares](#how-simplelinuxupdater-compares-to-scripts-and-ansible)

## Add and manage servers

Use the Manage page to add, edit, or delete servers. Authentication options:

- Password per server
- SSH key per server (uploaded via UI)
- Global SSH key (uploaded via UI and reused when per-server key is missing)

## Authentication flow

UI/API access uses the built-in local login:

1. First run: create the admin account at `/setup`.
2. Sign in at `/login`.
3. Use the logout action to end the current session.

Notes:

- Sessions are server-side and stored in SQLite.
- `/metrics` is not tied to UI sessions; it uses a dedicated bearer token managed from `/admin`.
- UI pages are CSP-hardened and load JavaScript/CSS from external `/static` assets only (no inline scripts/styles/handlers).
- Programmatic setup/login/logout requests must satisfy same-origin host checks:
  - `Origin` host matches request host (or `Referer` host matches request host)
  - `Sec-Fetch-Site: same-origin` is recommended and validated when present

Example (programmatic login):

```bash
curl -X POST http://localhost:8080/api/auth/login \
  -H "Content-Type: application/json" \
  -H "Origin: http://localhost:8080" \
  -H "Referer: http://localhost:8080/" \
  -H "Sec-Fetch-Site: same-origin" \
  -d '{"username":"admin","password":"<password>"}'
```

## Trust a host key

In the Add/Edit server form, use "Trust SSH host key now" to:

- Scan the server host key
- Show its fingerprint for verification
- Append it to the known-hosts file after confirmation

This helps avoid first-connection failures due to unknown host keys.

In Edit Server, use **Known host management** to:

- Check whether the current host/port is already saved in `known_hosts`.
- Clear an existing known-host entry for the current host/port (for key rotation/replacement).
- Avoid redundant trust prompts on Save when the same host/port was already confirmed as trusted in that edit session.

## Run updates with approval

Typical workflow:

1. Trigger an update.
2. The updater runs pre-checks, then runs `apt-get update`.
3. It simulates the upgrade to list pending packages.
4. The server enters `pending_approval`.
5. You approve or cancel.

Approval actions:

- Approve all pending updates
- Approve security-only (runs a targeted `apt-get install --only-upgrade` for the approved security packages)

If you approve security-only and no security packages are detected, the upgrade is skipped and the update completes without applying changes.

### Pre-checks (fail fast)

Before `apt-get update`, update actions run mandatory pre-checks over SSH:

- Disk space: checks free space on `/var` and `/` and requires at least `1 GiB` (`1048576 KB`).
- Lock contention: checks apt/dpkg locks with `/usr/bin/fuser` from the `psmisc` package. If `fuser` is missing or not allowed by passwordless sudo, the pre-check fails instead of using the older process-name fallback.
- APT/DPKG health: runs `dpkg --audit` and `apt-get check`.

After `apt-get update` and the read-only package simulations, a second disk-space gate evaluates the actual upgrade plan before approval. Its conservative reserve is `1 GiB` base + `64 MiB` per planned package + `512 MiB` per newly installed package. New kernels and firmware plans therefore reserve substantially more working space than a small library-only update. The calculated requirement, package counts, free space, and result are retained in the plan and update logs.

If either the baseline or plan-aware pre-check fails, the update stops before the approval flow and the server enters `error`. The simulated plan remains available as failure evidence, but no pending approval or package mutation is created.

### Non-interactive package policy

Every APT/dpkg mutation declares the same unattended policy instead of relying on the SSH session's missing TTY or ambient environment:

- `DEBIAN_FRONTEND=noninteractive` with critical debconf priority
- `APT_LISTCHANGES_FRONTEND=none` so changelogs cannot open a pager
- `NEEDRESTART_MODE=a` so needrestart handles eligible service restarts automatically
- `UCF_FORCE_CONFFOLD=1` and dpkg `--force-confdef --force-confold`, preserving the installed configuration when a conffile conflict has no default answer

The chosen strategy is written near the start of update, repair, and autoremove logs. Read-only APT simulations also force the C locale and non-interactive frontends. This prevents the `Dialog → Readline → Teletype → Noninteractive` fallback warnings seen when no controlling TTY exists.

### Post-update health checks

After an upgrade completes, the updater can run post-update health checks:

- APT/DPKG health (`dpkg --audit`, `apt-get check`)
- Failed systemd units (`systemctl --failed`)
- Reboot required marker (`/var/run/reboot-required`, warning only by default)
- Optional custom command (`DEBIAN_UPDATER_POSTCHECK_CMD`)

Blocking behavior is configurable; see [configuration.md](configuration.md).

The failed-units post-check takes a baseline snapshot before upgrade and compares it with the post-upgrade state to avoid flagging pre-existing failures as newly introduced.

## CVE-aware pending approval

During `pending_approval`, the UI shows a structured pending updates list:

- Security updates are prioritized first.
- CVE enrichment runs asynchronously so approval stays fast.
- CVE state values:
  - `pending`: lookup in progress
  - `ready`: CVE list populated
  - `unavailable`: lookup failed or timed out
  - `skipped`: outside lookup budget

Notes:

- CVE information is best-effort and advisory. Missing CVEs does not imply a package is not security-relevant.
- CVE lookup is derived from package changelogs (`apt-get changelog`) on the target host.

## Logs and status

The Status page shows current state and allows you to inspect logs. Logs are updated automatically as the updater runs.

Each selected server also shows a **Recommended action**. An uncertain mutating APT timeout is persisted as `needs_reconciliation`; normal update and cleanup actions remain blocked until an operator reviews the live logs and confirms **Repair APT**. The repair checks package-manager locks, runs `dpkg --configure -a`, repairs dependencies, and verifies `dpkg --audit` plus `apt-get check` before returning the server to `done`.

When current host facts report that a reboot is required, **Recommended action → Reboot and verify** offers a deliberately confirmed controlled reboot. The reboot command is sent once and is never automatically replayed. SimpleLinuxUpdater then waits for SSH to return and only reports success after proving that uptime reset and the reboot-required marker cleared. A missing uptime baseline or an unverified restart leaves the job in `error` for operator review.

### Passwordless apt toggle

From the Status page, you can enable or disable passwordless apt (per server). This is only needed for non-root SSH users: it creates or removes `/etc/sudoers.d/apt-nopasswd` on the target host so the managed apt/dpkg commands and the exact `systemctl reboot` command can be executed via sudo without prompting. Root SSH sessions run those commands directly. Re-run **Enable apt** once on non-root hosts configured before the explicit non-interactive strategy so their managed sudoers rule permits the restricted `/usr/bin/env … /usr/bin/apt-get` and dpkg command forms.

## Scheduled canary and wave rollouts

In **Admin → Scheduled Update Policies → Execution**, choose **Canary, then waves** to avoid releasing one scheduled policy to the whole matched fleet at once. Configure:

- the number of canary servers (`1–50`);
- the maximum servers in each following wave (`1–200`);
- the minimum delay between wave release times (`1–1440` minutes).

Matched servers are sorted deterministically by name, so the preview and scheduler use the same canary and wave membership. Wave 1 cannot start until every canary run reaches `succeeded`; each later wave similarly waits for every earlier run. `queued`, `running`, and `waiting_approval` hold the gate. A failed, skipped, cancelled, or interrupted earlier run stops downstream waves and records them with reason `rollout_gate` rather than silently applying the rest of the fleet.

The policy preview labels each matched server as canary or wave N. The policy list and calendar expose the stored rollout settings. Use **All matched servers** when no staged rollout is desired.

## Audit trail

The Manage page includes Activity History, backed by SQLite `audit_events`.

API:

```bash
curl -H "Cookie: simplelinuxupdater_session=<session-cookie>" "http://localhost:8080/api/audit-events?page=1&page_size=20&status=failure"
```

Notes:

- The actor is derived from the authenticated session username.
- The audit store is automatically pruned (default retention is 90 days).

## Observability and metrics

UI:

- `GET /observability`

Summary API:

- `GET /api/observability/summary?window=24h|7d|30d`

Metrics:

- `GET /metrics` (Prometheus text format, bearer token required)
- Disabled by default until a token is generated in `/admin`
- Token management API (session-authenticated):
  - `GET /api/metrics/token` (status only)
  - `POST /api/metrics/token` (generate/rotate and return token once)
  - `DELETE /api/metrics/token` (disable metrics token)

The token status distinguishes disabled, never-used, current, and stale credentials. A token becomes stale after 30 days without a successful `/metrics` request, but is never disabled automatically. Status responses expose only safe timestamps and a masked last-used origin; they never return the bearer token after its one-time create/rotate response.

Observability KPIs are computed from `update.complete` audit events.

## Backup and restore

Use `/admin` -> **Backup & Restore** for disaster recovery and host/container migration.

Export:

1. Enter and confirm a backup passphrase (minimum 12 characters).
2. Choose whether to include `known_hosts`.
3. Download the encrypted `.slubkp` file.

Restore:

1. Upload a `.slubkp` file.
2. Enter the backup passphrase.
3. Confirm replacing `servers.db` and optional `known_hosts`.

Notes:

- Restore immediately replaces `servers.db` and, if present in the backup, `known_hosts`. The backup `config.json` is validated and its encryption key is used to re-encrypt restored secrets with the local `config.json` key.
- No restart is required after restore.
- If your deployment depends on external TLS/reverse-proxy config, back that up separately.

## How SimpleLinuxUpdater compares to scripts and Ansible

### Overview table

| Aspect                          | SimpleLinuxUpdater                           | Shell/script automation                | Ansible-style automation                     |
| ------------------------------- | -------------------------------------------- | -------------------------------------- | -------------------------------------------- |
| **Primary goal**                | Lightweight web UI for Debian/Ubuntu updates | Quick custom commands and scripts      | Full automation and configuration management |
| **Ease of setup**               | Low (fast UI setup)                          | Very low (simple scripts run anywhere) | Medium (install Ansible, manage inventories) |
| **Typical use case**            | Manual review and remote update control      | Ad-hoc tasks or one-off automation     | Repeatable automation across many hosts      |
| **Learning curve**              | Low                                          | Low (if you know shell basics)         | Medium-high (YAML, playbooks, roles)         |
| **Scale**                       | Small fleets / homelabs                      | Works on small sets, limited structure | Best for large infrastructures               |
| **Repeatability / idempotence** | Manual (UI driven)                           | Depends on script quality              | Designed for repeatable, idempotent runs     |
| **Scope**                       | Package updates only                         | Any shell task                         | Updates, configuration, orchestration        |

### Pros and cons

#### SimpleLinuxUpdater

Pros:

- Easy to install and use with a web UI
- No need to write scripts or playbooks
- Great for quickly managing updates from one dashboard

Cons:

- Specialized to apt updates
- Not built for broader automation workflows
- Not ideal for large automated infrastructures

#### Shell/script automation

Pros:

- Extremely flexible; write exactly what you need
- No additional tooling required apart from a shell and SSH
- Good for simple tasks or custom operations

Cons:

- Harder to maintain as complexity grows
- Less structure for large inventories of hosts
- Repeatability is up to the script author

#### Ansible-style automation

Pros:

- Structured, scalable automation for many tasks (not just updates)
- Ideal for configuration management and large fleets
- Uses playbooks and inventories for repeatable operations

Cons:

- Requires learning Ansible fundamentals (YAML, inventories)
- More setup and tooling overhead
- Overkill when all you need is a UI for Debian package updates

### When to choose what

- SimpleLinuxUpdater: if you want a simple web UI to manage Debian/Ubuntu updates without scripts or playbooks
- Shell/script automation: if you are comfortable with shell and want quick, custom control
- Ansible-style automation: if you need large-scale automation, configuration, and repeatability across environments
