# Candidate publication qualification — 2026-09-07

Validated from `codex/release-candidate-promotion`, based on `252b2f7`.

- Go 1.26.6: full race and atomic-coverage suites passed, 73.9% coverage; vet,
  build, staticcheck, Govulncheck and Actionlint passed.
- 21 Python regressions passed through the Go release-policy wrapper. Stateful
  external-service fakes cover runtime/scan failures on both architectures,
  record/version/latest/GitHub writes failing before or after acceptance,
  conflicting/missing records, read failures and no-op partial retries.
- Workflow order checks were strengthened to reject absent as well as reordered
  steps, including a regression that removes each required step.
- Real Docker smoke and Trivy OS/Go scans passed for local amd64 and arm64
  images. A fresh amd64 build with `--pull --no-cache-filter runtime` passed
  runtime/persistence smoke and the expanded scan.
- Repeating the same release-style amd64 build showed the Go compilation step
  `CACHED`, while the runtime APK command executed again (1.8 seconds), upgrading
  libcrypto3/libssl3 from 3.5.7-r0 to 3.5.8-r0. This is cache-behavior evidence,
  not a claim about total CI performance.
- Docker failure tests preserve initial/restarted logs and filtered state before
  cleanup, even if cleanup fails; the original failure exit status survives.
  An actual failing archive-server fixture retains stdout after extraction
  cleanup. Scanner failure fixtures retain JSON/stderr and reject missing Go
  binary detection.

Read-only GHCR resolution froze the distributed `latest` digest as
`sha256:66d007ce2e056d8552b9e7aebc12a8fe1946ba0058fa132a58379de244d75f6a`.
Both architecture scans returned failure for CVE-2026-14456 in libcrypto3 and
libssl3 3.5.7-r0, fixed in 3.5.8-r0. Both also detected `app/webserver` Go
metadata without HIGH/CRITICAL findings. Rebuilding a fixed local image does
not remove these findings from the distributed image.

No GitHub release or GHCR write was performed. Publication side effects were
simulated with persistent service state; a real coordinated publication remains
to be verified during the next authorized release. Existing distributed images
were not changed. The optional concurrency queue expansion is deferred.
