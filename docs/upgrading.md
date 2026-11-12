[Documentation map](README.md) | [Deployment](deployment.md) | [Accounts and API](personal-api.md) | [CI](ci-and-release.md)

# Upgrading to v0.5.7

Use the image digest recorded in the published release's `publication.json`,
or download the matching versioned binary archive and checksum list.

## From v0.5.6

Export and verify a backup, finish or cancel pending maintenance, then replace
the executable and web assets together. v0.5.7 keeps dashboard action controls
stable during live data refreshes. This fixes intermittent clicks on the
selected-host Logs button and preserves keyboard focus and pending-package
search selection while the displayed data changes. Action availability and
labels continue to reflect the latest host state.

No database migration, required environment variable, dependency change or SSH
helper protocol change is introduced relative to v0.5.6. Reload open browser
tabs after deployment to load the updated interface. Interrupted approvals
require a fresh update check and renewed approval.

## From v0.5.5

Export and verify a backup, finish or cancel pending maintenance, then replace
the executable and web assets together. v0.5.6 distinguishes updates explicitly
deferred by Ubuntu phasing from updates classified as requiring a full upgrade.
The pending package list and package details display “Phased rollout · Waiting
for Ubuntu” for phased updates. Run a fresh update check to refresh existing
pending package classifications.

No database migration, required environment variable, dependency change or SSH
helper protocol change is introduced relative to v0.5.5. This release does not
force installation of phased updates. Interrupted approvals require fresh
discovery and approval.

## From v0.5.4

Export and verify a backup, finish or cancel pending maintenance, then replace
the executable and web assets together. v0.5.5 prevents idle automatic backup
polling from briefly taking exclusive maintenance admission. Disabled or
suspended backups, future occurrences and pending retries no longer block
scheduled host updates during that poll. Due backups, interrupted recovery and
temporary archive cleanup retain exclusive maintenance protection, and backup
state is revalidated before any operation.

No database migration, required environment variable, dependency change or SSH
helper protocol change is introduced relative to v0.5.4. Existing notification
selections remain in effect. Review **Admin → Scheduled Runs** for prior
`skipped` outcomes with reason `maintenance`. Those occurrences remain recorded
and are not replayed automatically. Run a manual update check for each host
that needs to catch up, or wait for its next allowed scheduled occurrence.
Interrupted approvals require fresh discovery and approval.

## From v0.5.3

Export and verify a backup, finish or cancel pending maintenance, then replace
the executable and web assets together. v0.5.4 changes operator-page layout and
navigation and updates the SQLite driver from v1.60.1 to v1.61.1 with its
transitive dependencies. `golang.org/x/net` updates from v0.61.0 to v0.62.0.
It introduces no new application database migration,
required environment variable or SSH helper protocol. Accounts, sessions,
policies, secrets, package deferrals and Approval Scope retain their contracts.

On mobile, open **Menu** to switch pages and **Settings** to choose an Admin
section. Status keeps secondary metrics and filters in disclosures; Manage
starts with the directory and places export and bulk tools in disclosures.
Saved section preferences are retained. Use **Expand** to reveal section
content; a collapsed Metrics section now hides its controls correctly.

## From v0.5.2

Export and verify a backup, finish or cancel pending maintenance, then replace
the executable and web assets together using the procedures below. Also account
for the operator-page and SQLite changes above. The available-update interface
introduced in v0.5.3 adds no application database migration, required environment
variable or SSH helper protocol relative to v0.5.2. Accounts, sessions, policies,
secrets and package deferrals retain their contracts. Interrupted approvals
require fresh discovery and approval.

The package drawer now shows 15 packages per page, search and security filters,
with Previous/Next controls above and below package and CVE lists. Details opens
a full-width package inspector with five CVE references per page and one source
proof open at a time. View coverage groups source limitations. Filtering and
pagination do not change Approval Scope. CVE information still covers only
available updates, not the whole installed inventory.

## From v0.5.1

Export and verify a backup, then use the Docker or binary replacement procedure
below. Replace the executable and web assets together. Existing accounts,
sessions, policies, secrets and package deferrals retain their contracts.
v0.5.2 introduces no new database migration, required environment variable or
SSH helper protocol relative to v0.5.1.

CVE information now accompanies only available APT updates on Status and in the
package drawer. Standalone CVE analysis, its ledger and independent remediation
workspaces are retired. Old page links redirect to Status and authenticated
retired APIs return `410 Gone`. Update integrations accordingly. Former
`DEBIAN_UPDATER_VULNERABILITY_SCAN_*`, `DEBIAN_UPDATER_VULNERABILITY_TARGET_*`
and `DEBIAN_UPDATER_SECURITY_INTEL_*` settings no longer start background work.
Normal scheduled APT discovery remains available.

Legacy CVE observations stay stored and backup-compatible but do not feed
current Status counts. Refresh the APT package plan after upgrading. Candidate
correction counts require current, qualified evidence; unavailable CVE data
remains incomplete and does not prevent approval of a known security update.
The update plan does not claim a complete inventory of installed vulnerabilities
or runtime exploitability.

Finish or cancel pending maintenance before stopping the old process. Interrupted
approvals require fresh discovery and approval. Rollback restores the pre-upgrade
backup into a separate old-version installation; do not run the old executable
on the upgraded database.

## From v0.5.0

Also account for the additive remediation-history/risk-review tables and package
evidence introduced in v0.5.1. Those records remain retained in v0.5.5, while the
standalone workflow is retired as described above. The historical
[v0.5.1 qualification record](release-v0.5.1-readiness.md) describes that release's
measured checks and limits; it does not qualify v0.5.5.

## From v0.4.16 or earlier

Also follow the [v0.5.0 compatibility notes](../CHANGELOG.md#v050---2026-10-29)
for account/session migration, policy changes and the protocol 3 managed helper.
The pinned automated old-version qualification below remains based on published
v0.4.16; it is not a separate v0.5.4-to-v0.5.5 migration proof.

## Before changing the application

1. Finish or cancel host maintenance. Record enabled policies, pending approvals,
   timezone, notification destinations and the deployed version/image digest.
   Pause scheduled policies and automatic backups during the change window.
2. In **Admin → Backup & Restore**, export an encrypted `.slubkp` archive with
   `known_hosts` included. Run **Verify** on that downloaded archive while the
   old application is still available. Keep it and its passphrase separately,
   outside the installation and Docker volume. Preserve your environment file
   and any separately managed SSH trust files too.
3. Stop the old application cleanly; allow ongoing backup transfers to finish.
   Keep one process per database. Retain the old binary/image and assets for
   recovery, but keep that installation stopped after migration.

## Docker

For a deployment named `simplelinuxupdater` using the named volume
`debian-updater-data`, first choose the target image by its published digest:

```bash
NEW_IMAGE='ghcr.io/nolife141-teams/simplelinuxupdater@sha256:REPLACE_WITH_TARGET_DIGEST'
docker pull "$NEW_IMAGE"
docker stop --time 60 simplelinuxupdater
docker rename simplelinuxupdater simplelinuxupdater-before-upgrade
docker run -d --name simplelinuxupdater --restart unless-stopped \
  --env-file .env -p 127.0.0.1:8080:8080 \
  -v debian-updater-data:/data "$NEW_IMAGE"
docker logs --tail 50 simplelinuxupdater
```

Carry over your actual port, proxy network, environment and extra mounts. If
using Compose, pin `image:` to the chosen digest, then `docker compose stop`
and `docker compose up -d --no-deps SERVICE`. Keep the same volume mapping.
The image entrypoint retains its existing ownership validation and runs the
application as `app`; do not bypass it during normal startup. Do not restart
the retained old container on the migrated volume.

The Docker smoke gate checks non-root startup and account persistence when
recreating a container with the same named volume. The upgrade harness checks
the actual published old archive and candidate executable inside a separate
networkless container; it does not qualify every operator's Compose/proxy setup
or claim an old-GHCR-image migration test.

## Binary archive

Download the target platform archive and its checksum list from the same
versioned GitHub Release. On Linux amd64, in a new staging directory:

```bash
VERSION='0.5.7'
ARCHIVE="SimpleLinuxUpdater_${VERSION}_linux_amd64.tar.gz"
BASE="https://github.com/NoLife141-Teams/SimpleLinuxUpdater/releases/download/v${VERSION}"
curl --fail --location --output "$ARCHIVE" "$BASE/$ARCHIVE"
curl --fail --location --output checksums.txt "$BASE/checksums.txt"
sha256sum --check --ignore-missing checksums.txt
tar -xzf "$ARCHIVE"
```

Check that the named archive reports `OK`. Obtain the checksum list over the
trusted release channel; it is not a signature. The automated v0.4.16 baseline
additionally pins that list, the archive and `publication.json` by SHA256 in
[release.json](../tools/ci/upgrade/release.json).

Stop your supervisor (`sudo systemctl stop simplelinuxupdater` for the example
unit in [Deployment](deployment.md#binary-deployment)). Replace `webserver`,
`templates/` and `static/` together with those from the extracted directory.
Keep the existing data-directory paths and encryption configuration. Restart
with the same working directory and environment
(`sudo systemctl start simplelinuxupdater` for that unit). Do not copy empty
staging data over the old database or `config.json`. No Go installation is
needed by an operator using the archive.

## Confirm the migrated installation

- `/setup` stays closed. The initial account keeps its ID, username, Argon2id
  password hash and password. When upgrading from v0.4.16 or earlier it becomes
  administrator, and pre-migration browser sessions are invalidated once. Existing
  v0.5.x sessions retain their normal lifecycle across application restarts.
  Named reader/operator/admin accounts,
  optional TOTP MFA and scoped personal API tokens are documented in
  [Accounts and API](personal-api.md); metrics authentication is separate.
- Check inventory, disabled state, tags, SSH password/key markers and host-key
  trust, timezone, daily/weekly policy schedules, notification settings and
  metrics access before re-enabling work. Daily/weekly policies retain their
  cadence and automatic canaries. New deferral and occurrence tables start with
  their additive migration defaults.
- Queued, running and waiting-for-approval jobs are interrupted at startup.
  Interrupted package mutations require APT reconciliation. Old pending approval
  cannot resume: inspect the host, rescan and approve the newly displayed job,
  generation and transaction. Package/deferral/profile drift can require renewed
  approval too. Never reuse an archived approval body.
- The protocol 3 managed helper from v0.5.2 remains compatible. If an older
  helper is installed, update it before protected maintenance. Export the
  current installer using `./webserver helper export --user deployer > install-helper.sh`,
  or `docker run --rm --entrypoint /app/webserver "$NEW_IMAGE" helper export --user deployer > install-helper.sh`.
  Review and transfer the script to the intended target and run it there with
  administrator privileges. See [managed helper installation](usage.md#managed-helper-installation)
  for Check/Enable apt and protocol 3 requirements, including root SSH with
  package deferrals. No helper is installed on the updater host by qualification.

## Restore and rollback

To restore onto the candidate, initialize an isolated candidate installation,
log in as its administrator, verify the old `.slubkp` in Admin and restore it.
Restoration replaces inventory/persistence and re-encrypts archived secrets with
the destination installation's encryption key. Sign in with the archived account.
Browser sessions and personal API credentials are invalidated; archived recovery
codes and pending MFA enrollment are discarded. Observe the
[authenticator restore rules](personal-api.md) before retrying an enabled factor.
Revalidate local automated-backup destinations and secret files before enabling
scheduled exports, and review notifications, SSH trust and metrics credentials.
Legacy v0.4.16 archives have no automated-backup configuration: automation stays
disabled and unvalidated. Existing candidate restore tests cover suspension of
archived enabled automation.

Rollback means restoring the **pre-upgrade archive** into a separate installation
of the recorded old version, with its matching assets, a new data directory or
Docker volume, and a restricted listener. Initialize that installation, verify
and restore the pre-upgrade archive there, then check login/data before moving
traffic. Keep the candidate stopped while changing traffic. Do not start an old
binary/container on a migrated database: downgrade compatibility is not asserted.
Do not restore a candidate-exported archive into an old version without an
explicit compatibility guarantee. Keep both recovery installations isolated
from target hosts until policy and credential review is complete.

## Reproduce qualification

From a candidate checkout, build a fresh Linux amd64 executable (CI does this),
then run with Python 3.12+ and Docker on Linux:

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /tmp/slu-upgrade-candidate .
python3 tools/ci/upgrade/negative_control.py \
  --candidate /tmp/slu-upgrade-candidate --candidate-sha "$(git rev-parse HEAD)" \
  --output /tmp/slu-upgrade-negative
python3 tools/ci/upgrade/run.py \
  --candidate /tmp/slu-upgrade-candidate --candidate-sha "$(git rev-parse HEAD)" \
  --output /tmp/slu-upgrade-positive
```

A supplied static Linux amd64 executable can replace the build command; the harness itself needs
no Go. Record the exact source SHA used to build it. `--release-cache DIR` is an
optional untrusted cache: all three release files are still verified. No cache
or private historical campaign directory is required. `--source-root DIR`
selects the candidate checkout's tracked assets when using trusted helpers from
another directory, as the release workflow does.

The official old archive is unchanged. Both real executables use a disposable
installation, a real login, encrypted synthetic SSH password, SSH trust, disabled
Server and disabled daily/weekly policies in Europe/Paris. Offline SQL adds
pre-crash jobs in queued/running/waiting approval, one interrupted APT mutation,
one terminal job, and a linked Scheduled Run. The old archive is exported before
those synthetic crash rows are added; restart recovery checks them separately
on the migrated database. This fixture does not claim a
real SSH/APT execution. Migration, API projections, startup recovery, old archive
export/restore and second-start session continuity are real production behavior.

The negative control deletes only disposable legacy inventory immediately before
candidate startup. The harness must exit 1 at the inventory-conservation assertion;
its wrapper exits 0 only after confirming that specific failure, later `not-run`
steps and successful cleanup. It never patches the production binary or enables
a production bypass. The following positive run must exit 0 independently.

Candidates use `CGO_ENABLED=0`, matching release archives and avoiding a dependency
on the runner's libc in the pinned Debian runtime.

The pinned Python runtime has `--network none`, only loopback, dropped
capabilities, no Docker socket and a read-only mount of owned executables/assets.
The application environment is constructed from an allowlist. An owned loopback
SSH sentinel must receive zero connections; assertions also forbid new
maintenance jobs/audits and active Scheduled Runs. No host maintenance, DNS,
provider call or real inventory is used. The old release's workers are confined
by the same network namespace even where newer environment toggles do not exist.

Only `report.json` is published. It records release/source/binary and harness
hashes, individual passed/failed/not-run steps, real container/global exits,
negative-control provenance and cleanup. Interrupted starts without an observable
scenario report are explicitly marked `not-observed`, rather than falsely `not-run`. Logs, raw DB/config, ciphertext,
passwords, cookies, keys, backup passphrases and archives stay private and are
deleted. Startup/shutdown and container execution have deadlines. The negative-control
wrapper forwards cancellation to the runner and waits for its bounded cleanup;
a timeout cannot count as the expected diagnostic failure. Signals trigger
owned-process shutdown, label-checked container cleanup and independent temporary
file cleanup. CI bounds the whole job to 15 minutes and retains just the two
redacted reports for one day. This qualifies Linux amd64 persistence from
v0.4.16; multiarch image startup and SSH/APT behavior retain their separate gates.
