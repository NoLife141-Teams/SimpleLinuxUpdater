[Documentation map](README.md) | [Deployment](deployment.md) | [Accounts and API](personal-api.md) | [CI](ci-and-release.md)

# Upgrading from v0.4.16 to v0.5.0

These instructions cover the v0.5.0 release candidate and its compatibility with
published v0.4.16. Read the [v0.5.0 compatibility notes](../CHANGELOG.md#upgrade-notes).
The preparation PR does not publish v0.5.0. Use the download commands below only
after publication, with the image digest from that release's `publication.json`.

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
VERSION='0.5.0'
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
  password hash and password, and becomes administrator. Sign in again once:
  pre-migration browser sessions are invalidated. Newly created sessions survive
  subsequent application restarts. Named reader/operator/admin accounts,
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
- Update the managed root helper before protected maintenance. Export the
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
