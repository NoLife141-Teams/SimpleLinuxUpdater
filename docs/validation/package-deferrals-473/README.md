# Package deferrals #473 qualification

Validated on 2026-10-26 with Go 1.26.10. No release version was changed.

## Real APT mutation evidence

The fixture reuses #469's pinned image allowlist, Docker command adapter and
ownership label. Every run creates a new UUID-named container with a matching
ownership marker, `--network none`, resource limits, and **no mounts or user
volumes**. Packages are built with `dpkg-deb` inside that container. A local
`file:` repository replaces only the container's sources. No APT command runs
against the development host. Cleanup checks ownership before removing the
container, including after failure.

```sh
GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache GOMAXPROCS=2 \
  go run ./tools/ci/package-deferral-helper > /tmp/slu-473-root-helper.sh
python3 tools/ci/ssh-apt/deferrals.py --distro debian13 \
  --helper /tmp/slu-473-root-helper.sh \
  --output docs/validation/package-deferrals-473/apt-debian13.json
python3 tools/ci/ssh-apt/deferrals.py --distro ubuntu2404 \
  --helper /tmp/slu-473-root-helper.sh \
  --output docs/validation/package-deferrals-473/apt-ubuntu2404.json
```

Both reports record helper SHA256
`9919ee2f34f1cd12b6368e6d3dbe4173e7bdcbb1dbfe6543e032daf34f805ce5`,
the fixture source hash, pinned image, before/after installed package versions,
actual APT output and successful owned-container removal.

| Target | APT | dpkg | Result |
| --- | --- | --- | --- |
| Debian 13 | 3.0.3 | 1.22.22 | 11/11 passed |
| Ubuntu 24.04 | 2.8.3 | 1.22.6ubuntu6.6 | 11/11 passed |

The local dataset includes:

- `slu-deferred` versions 1 and 2, and independently upgradable
  `slu-independent` versions 1 and 2. Each of the four scopes actually installs
  the independent version 2 while the deferred package remains at version 1.
- `slu-dependent` version 2 requires `slu-deferred (>= 2)`. Unprotected
  simulation proves the dependency would change. Calling the actual production
  helper, even with both upgrades in the supplied transaction, fails in the
  pre-dpkg hook and leaves **every installed package unchanged**.
- A replacement that conflicts with the deferred package proves implicit
  removal is refused before dpkg. A separate replacement of a non-deferred
  victim proves explicitly confirmed safe removals succeed.
- Co-installed `slu-multi:amd64` and `slu-multi:i386` use `Multi-Arch: same`.
  Upgrading the native package would force the deferred i386 package; the real
  transaction is blocked without partial changes.
- Expired rules and an unexpected dependency transaction are blocked without
  package changes. An administrator-held package cannot be explicitly upgraded.
  Administrator hold selections and the preference-file hash remain identical,
  and the helper leaves no temporary guard directories behind.

The final successful runs supersede early fixture-bootstrap failures caused by
an empty line in generated `Packages` metadata. A subsequent protocol check
distinguished `**REMOVE**` actions from downgrade directions; both final reports
were regenerated with the corrected, final helper hash above.

## Application and browser validation

The following commands passed, using disposable databases, fake SSH/HTTP
services or localhost listeners. Go commands used
`GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache GOMAXPROCS=2`.

| Command | Result |
| --- | --- |
| `go test ./...` | Passed |
| `go test -race -count=1 ./...` | Passed |
| `go vet ./...` | Passed |
| `go build ./...` | Passed |
| `staticcheck ./...` (v0.7.0 / 2026.1) | Passed |
| `go test -count=1 -covermode=atomic -coverprofile=/tmp/slu-473-coverage.out ./...` | Passed |
| `go tool cover -func=/tmp/slu-473-coverage.out` | Total 76.2%; CI minimum 73.0% |
| `GO_COVERAGE_THRESHOLD=73.0 tools/ci/check-coverage.sh` | Passed with the measured profile linked as `coverage.out` |
| `npm run test:unit` | 197 passed |
| `npm run test:e2e -- --grep 'package deferrals 473' --workers=1` | 2 passed |
| `npm run test:e2e -- --workers=1` | 94 passed |

Go tests cover strict selectors/multiarch, inherited cumulative rules,
expiration, pending-approval drift and cancellation precedence, all four runner
scopes with persisted job plans and completion audit assertions, scheduled
auto-approval, removal gates, last-moment rule drift, API authentication actor
attribution, inventory write/rename/delete continuity, policy edit/delete and
originating manifests, additive schema initialization, real application backup
restoration, and complete snapshot recovery after a large audit is truncated.
Unplanned repair/autoremove is blocked while active rules cannot be enforced by
an approved transaction.

Playwright uses `/tmp/slu-473-playwright-server.sh`: it builds into a fresh
temporary directory and `exec`s the application with temporary database and
known-hosts paths. `CAPTURE_PACKAGE_DEFERRALS_473=1` regenerates the fixtures in
[`.github/assets/package-deferrals-473`](../../../.github/assets/package-deferrals-473).
Mobile Manage evidence is split between rule review and rule entry so the
modal's sticky actions do not hide the form. The approval fixture uses the
current structured CVE contract and opens `CVE-2026-1000`, while verifying that
the original exact rule revision stays bound to an already-open confirmation.

## Limits

The new real-APT operator was qualified on the two pinned images above; the new
fixture was not run on Debian 12 or Ubuntu 22.04/26.04. This fixture exercises the
production root helper directly, without SSH, official repositories, live CVE
feeds, systemd or reboot. Go runner/API tests and browser fixtures cover the
application paths separately; existing #469 SSH qualification is unchanged.
Root and non-root Servers with active deferrals need the current protocol 3
helper. There is no legacy/unprotected fallback. With active rules, full-scope
execution uses the displayed targeted install transaction, with every removal
still requiring explicit confirmation. Dependency conflicts block the selected
transaction instead of silently selecting a smaller subset.

The three pre-existing user prototype files and both stashes were left intact.
No actual SSH target, inventory, credential store, host package state or user
Docker volume was used.
