# PR #485 CI regression follow-up

Validated on 2026-10-26 after commit `cacd1df`.

## Confirmed causes and corrections

The unchanged #469 real SSH/APT harness reproduced the Debian 13 failure with
`root-upgrade: failed`, error `'approved_packages'`. The upgrade itself reached
version 2.0 and completed its real HTTP post-check. Application logs recorded
`audit metadata truncated from 4261 bytes across 38 fields`: the added deferral
facts pushed the completion audit beyond 4096 bytes, and the compact audit
representation omitted the approved package array consumed by the harness.

The audit serializer now preserves complete approved package sets whose JSON
fits within 1024 bytes, with an additional total-budget check reserving space
for health-check facts. It never publishes a partial approved set. Larger sets
remain available in the persisted job identified by `job_id`. Regression tests
cover ordinary/multiarch sets, oversized sets, invalid element types, the 4 KiB
limit and retained outcome/count facts. The existing SSH/APT assertions are
unchanged.

CodeQL identified a lowercase-only negative HTML assertion in the new unit
test. The assertion is now case-insensitive, and the fixture checks lowercase,
uppercase and mixed-case script tags, including an attribute. Production HTML
escaping is unchanged. CodeQL CLI is unavailable locally; the next remote
analysis must confirm the annotation is resolved.

CI logs also confirmed that `upload-artifact` found no reports under the hidden
`.tmp-ssh-apt/` directory. The CI and identical extended-workflow collection
paths now use visible `ssh-apt-evidence/`, with only these explicit files:

- `ssh-apt-evidence/report.json`
- `ssh-apt-evidence/app.log`

Hidden-file upload is not enabled. New harness tests enforce that allowlist and
verify redaction and exclusion of private database, configuration, key and
known-host files. The runtime fixture directory remains separate and is removed
during cleanup.

## Executed validation

Go commands used `GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache GOMAXPROCS=2`.

| Command | Result |
| --- | --- |
| `go test ./...` | Passed |
| `go test -race -count=1 ./internal/audit` | Passed |
| `go vet ./...` | Passed |
| `go build ./...` | Passed |
| `staticcheck ./...` with v0.7.0 | Passed |
| `npm run test:unit` | 197 passed |
| `python3 -m unittest discover -s tools/ci/ssh-apt -p 'test_*.py' -v` | 16 passed |
| `actionlint .github/workflows/ci.yml .github/workflows/ssh-apt-extended.yml` | Passed |

```sh
python3 tools/ci/ssh-apt/run.py --distro debian13 \
  --output /tmp/slu-473-ci-fixed-debian13
python3 tools/ci/ssh-apt/run.py --distro ubuntu2404 \
  --output /tmp/slu-473-ci-fixed-ubuntu2404
```

Both full harness runs passed all seven executed scenarios: root upgrade,
operator upgrade, misconfigured helper, active APT lock, stale approval/plan
drift, SSH disconnection with recovery, and changed host key. Both retained the
three expected `not_executed` cases for systemd, real reboot and official
provenance, which require other qualification boundaries. All task-owned
containers, networks, images and private temporary runtime files were cleaned.

Raw redacted reports: [Debian 13](ssh-apt-ci-fixed-debian13.json),
[Ubuntu 24.04](ssh-apt-ci-fixed-ubuntu2404.json). The failing local reproduction
is retained under `/tmp/slu-473-ci-repro-before-debian13/` for comparison.

The production helper was not changed. Its SHA256 remains
`9919ee2f34f1cd12b6368e6d3dbe4173e7bdcbb1dbfe6543e032daf34f805ce5`;
the two existing 11-scenario protected-APT reports remain applicable. Browser
product code was unchanged, so the previously passing 94-scenario Playwright
suite was not repeated for this correction. Remote CodeQL and artifact upload
remain to be confirmed by the next CI run.
