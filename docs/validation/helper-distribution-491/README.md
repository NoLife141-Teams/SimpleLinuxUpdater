# Managed helper distribution #491 qualification

Validated on 2026-10-27, based on main `767e9cdd090db23925cd8dda0da4cf1504fb94da`,
with Go 1.26.10. The implementation source hashes and actual command exits are
retained in the private `/tmp/slu-491-validation/report.json` and associated
manifests/logs. Intermediate runs are retained separately from final qualification.
No release version changed.

## Reproduction

Build a fresh production binary and fixture data from the same checkout:

```sh
go build -o /tmp/slu-491-webserver .
go run ./tools/ci/helper-distribution > /tmp/slu-491-fixtures.json
python3 tools/ci/ssh-apt/helper-distribution.py --distro debian13 \
  --binary /tmp/slu-491-webserver --fixtures /tmp/slu-491-fixtures.json \
  --output /tmp/slu-491-debian13
python3 tools/ci/ssh-apt/helper-distribution.py --distro ubuntu2404 \
  --binary /tmp/slu-491-webserver --fixtures /tmp/slu-491-fixtures.json \
  --output /tmp/slu-491-ubuntu2404
```

The developer fixture generator exports production content and the shared
read-only probe; it is not needed by operators. Operator instructions use only
`webserver helper export --user SSH_USER` from the shipped binary or image.

The qualification extends the existing `tools/ci/ssh-apt/run.py` harness, pinned
base images and ownership checks. It allocates new labelled containers, a private
network and an isolated application directory; it never accepts existing targets,
inventory, configuration, credentials, known-hosts or provider data. SSH root is
disabled before onboarding. Host identities are compared with the owned target's
console key before the harness explicitly establishes trust. Only fixture
credentials are created; reports redact them before writing. Cleanup verifies
labels/markers before removing owned resources. No host APT command is run.

## Results

Both Debian 13 and Ubuntu 24.04 passed all 16 distribution scenarios, with zero
cleanup errors, using the same final binary and fixture pair:

- Binary SHA256: `d777cda11bd275b93991b8216c6844b99167f1ec7d5684a6ca25137ef9080ece`.
- Fixture SHA256: `1cd1d2827b3699ae5bd2b76999c74ee1cca560e764f775fcf0122083a3ac1772`.

- Fresh operator Check blocks clearly, without changing managed files, dpkg state
  or inventory. Missing/old helper preflight blocks root and non-root deferrals.
- Helpers with a protocol 2 header and historical protocol 3 without capabilities
  produce upgrade instructions. No protected-command fallback is executed.
- Explicit installation produces byte-identical helper and sudoers content from
  the production generators, with root ownership and expected modes. Check and
  onboarding then succeed using operator SSH, with root SSH disabled.
- Missing sudo policy and mixed PASSWD policy fail with the distinct sudo-denied
  diagnostic. The mixed fixture proves `sudo -n -l` exits 0 while the actual
  noninteractive read-only `dpkg-audit` remains denied.
- Unmarked files and symlinks at all three managed/legacy paths are refused with
  exit 65 and unchanged snapshots. Customized legacy content, including only an
  extra trailing newline, is refused; the exact legacy rule migrates successfully.
- An injected fixture-only `visudo` failure returns 79, preserves managed files,
  and leaves no installer temporary files.

The extra-newline legacy regression was demonstrated red against the earlier
binary, then green after preserving trailing bytes in the comparison. The mixed
PASSWD regression was similarly demonstrated against the earlier probe before
restoring the actual read-only audit check. Failed attempts remain in private
evidence; they are not attributed to the final binary.

The existing offline real-APT deferrals harness also passed 12 scenarios on each
of Debian 13 and Ubuntu 24.04 using the generated helper. It uses `--network none`,
no mounts, and locally built fixture packages, and removed both owned containers.
The APT transaction hook implementation was unchanged.

Verification: full `go test -count=1 ./...`, `go vet ./...`, production build,
247/247 JavaScript unit tests, and 26/26 Manage Playwright scenarios with a fresh
test binary and `--retries=0`. Export tests cover process startup with unusable
installation/listener configuration, exact generator bytes, invalid arguments,
malicious usernames, help and output errors. Runner tests assert the shared
preflight on all protected scopes together with job/audit metadata. An initial
sandboxed Go run failed because localhost sockets were disallowed; the authorized
localhost rerun passed. This is recorded as an environment failure.

This campaign qualifies helper distribution and the affected onboarding flow.
It does not claim fresh five-distribution, reboot, systemd or official repository
provenance qualification. Docker target images and public build cache remain;
private fixture runtime files and owned containers/networks are removed.
