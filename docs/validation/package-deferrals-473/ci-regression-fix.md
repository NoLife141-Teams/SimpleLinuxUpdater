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
escaping is unchanged. The subsequent remote analysis resolved this regex
annotation; collector findings are addressed below.

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
suite was not repeated for this correction. SSH/APT jobs and artifact upload
were subsequently confirmed remotely: both artifacts contain exactly the two
allowlisted files, seven passed scenarios and successful cleanup.

## Evidence redaction follow-up

The next CodeQL run identified an actual raw intermediate write in the
collector: Docker application logs were first written to `work/app.log`, then
read back and redacted for the public output. The collector now redacts in
memory and writes only the public `output/app.log`; there is no intermediate
log file. Redaction precedes the 200,000-character tail limit, so a credential
crossing that boundary cannot survive as a fragment. Report values are redacted
recursively before JSON serialization, including multiline keys and nested or
already JSON-encoded diagnostics.

The existing exclusion test now intercepts every `Path.write_text` call during
evidence collection and verifies both its destination and the absence of raw
or JSON-escaped credential canaries at the time of each write. It still seeds
private database/configuration/key/known-host fixture files and verifies they
are excluded. The multiline JSON and tail-boundary tests failed against the
previous collector and pass after the correction. All 18 Python harness tests
pass. A real Debian 13 run with the corrected collector passed the seven
executed SSH/APT scenarios, kept the three expected non-executed cases, emitted
exactly `app.log` and `report.json`, and completed ownership-checked cleanup:

```sh
python3 -m unittest discover -s tools/ci/ssh-apt -p 'test_*.py' -v
python3 tools/ci/ssh-apt/run.py --distro debian13 \
  --output /tmp/slu-473-redaction-debian13
```

This correction changes only the Python collector and its tests. No CodeQL
suppression or analysis-model change is used. The business helper remains
unchanged; Go and Playwright were not repeated. The next remote CodeQL run must
confirm the sensitive-storage annotation is resolved before delivery.

## Final CodeQL finding: demonstrated false positive

The raw intermediate write was the real defect in alert
[11](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/security/code-scanning/11)
and is fixed. The remaining alert
[12](https://github.com/NoLife141-Teams/SimpleLinuxUpdater/security/code-scanning/12)
concerns the already-redacted output at `run.py:470` on `2887ba5`. Its SARIF paths
traverse both mandatory `str.replace` calls. The strict runtime tests still
verify every evidence write contains no credential, including multiline and
JSON-encoded keys, overlapping secrets and the log-size boundary.

Local reproduction used the checksum-verified **CodeQL CLI 2.27.3**, matching
**python-queries 1.8.13 / python-all 7.2.8**, from CodeQL source commit
`a1575b98a8cc19bcfa7bb6cb41be688cb3dae65c`. The unmodified official query reports
**one** repository finding at the same sink. The
[pinned string model](https://github.com/github/codeql/blob/a1575b98a8cc19bcfa7bb6cb41be688cb3dae65c/python/ql/lib/semmle/python/dataflow/new/internal/TaintTrackingPrivate.qll#L141-L153)
always propagates taint from a `str.replace` receiver to its result. It does not
prove that replacing a value with itself as the pattern removes it completely.
The [rule's sanitizer extension](https://github.com/github/codeql/blob/a1575b98a8cc19bcfa7bb6cb41be688cb3dae65c/python/ql/lib/semmle/python/security/dataflow/CleartextStorageCustomizations.qll#L32-L36)
has no concrete text-redaction sanitizer. This is a conservative analysis
limitation, not a remaining raw write.

### Reproduce safely

Save the following Markdown example **only in a temporary analysis directory**
as `minimal/proof.py`. Its writer is patched during every invocation: the raw
positive control is captured in memory, and no credential or log file is
written. The query nevertheless reports both line 5 (the unsafe control) and
line 10 (complete redaction); it does not report the constant control.

```python
from pathlib import Path


def unsafe_control(account):
    Path('unsafe.log').write_text(account.password)


def complete_redaction(account):
    value = account.password
    Path('redacted.log').write_text(value.replace(value, '[REDACTED]'))


def constant_control():
    Path('constant.log').write_text('[REDACTED]')


if __name__ == '__main__':
    from types import SimpleNamespace
    from unittest.mock import patch

    for canary in ('fixture-only-canary', 'quote"\\\ncanary', 'ümlaut-canary'):
        account = SimpleNamespace(password=canary)
        with patch.object(Path, 'write_text', return_value=10) as capture:
            unsafe_control(account)
            assert capture.call_args.args[0] == canary
            complete_redaction(account)
            assert capture.call_args.args[0] == '[REDACTED]'
            constant_control()
            assert capture.call_args.args[0] == '[REDACTED]'
    print('Only simulated writes; complete redaction always produces [REDACTED]')
```

Use the [official CLI 2.27.3 release](https://github.com/github/codeql-cli-binaries/releases/tag/v2.27.3),
verify its published checksum, and obtain the matching query source below.
Here `proof_dir` is the temporary directory containing `minimal/proof.py` and
`codeql_bin` is the verified CLI executable:

```sh
"$codeql_bin" version
python3 "$proof_dir/minimal/proof.py"
gh api repos/github/codeql/tarball/a1575b98a8cc19bcfa7bb6cb41be688cb3dae65c > "$proof_dir/queries.tgz"
mkdir "$proof_dir/queries"
tar -xzf "$proof_dir/queries.tgz" --strip-components=1 -C "$proof_dir/queries"
"$codeql_bin" database create "$proof_dir/db" --language=python --build-mode=none \
  --source-root="$proof_dir/minimal" --threads=2 --common-caches="$proof_dir/cache"
"$codeql_bin" database analyze "$proof_dir/db" \
  "$proof_dir/queries/python/ql/src/Security/CWE-312/CleartextStorage.ql" \
  --search-path="$proof_dir/queries" --format=sarif-latest \
  --output="$proof_dir/result.sarif" --threads=2 --ram=4096 \
  --common-caches="$proof_dir/cache"
```

The exact documented example was executed locally: all three credential
canaries produced the constant `[REDACTED]` at the complete-redaction sink,
without disk writes, and the official query produced exactly the two findings
above. For the repository reproduction, use a separate database with the
checkout at `2887ba5` as `--source-root`; the same query produces one finding at
`tools/ci/ssh-apt/run.py:470`.

The parent maintainer will record alert **12 as a false positive**, with this
justification. Alert 11's real correction remains in place. All checks and
strict canaries are preserved; no rule suppression, source/sink renaming,
analysis-model override or security-policy change is introduced. This final
step changes documentation only. GitHub triage remains the parent maintainer's
responsibility; this agent made no alert-state change.
