# Dashboard scaling qualification — issue #475

Only canonical version 2 results are retained here. An exploratory fixture used a
runtime status in the persisted job status column; it was rejected, corrected to
the Jobs constants, and all performance measurements were regenerated. Version 2
asserts 10% in progress, 20% awaiting approval and 70% done in inventory, SQLite and
Dashboard Projection for every measured size.

## Provenance and protocol

Reference production code: `22b7c20d1a40128876b528781ead3277a76c3c19` (including
#474 named accounts, MFA and personal API tokens). The reference server read only
tracked files extracted by `git archive` into `/tmp/slu-475-base-v2`; the final
server read this checkout. The same precompiled SSH-blocked test binary served
both. [Provenance](provenance.json) records full source/backend/binary/tool hashes;
[before](before/provenance.json) and [after](after/provenance.json) record the source
roots and runtime environment. Production Go is identical; only three frontend
JavaScript files and asset version parameters in `templates/index.html` differ.

Environment: Linux 6.8.0-147-generic, x86-64 KVM, AMD Ryzen 9 9950X exposed as four
logical CPUs, 12,248,248 kB RAM, Go 1.26.10, GOMAXPROCS=2, Node 22.23.3, installed
Playwright 1.63.0, Chromium 153.0.8010.12, headless 1440×1000, reduced motion. This
records the installed browser tooling; latency is not a CI gate or a production
capacity estimate. No other Go/PW/race test workload ran during the browser pair.

Seed 475, fixed business/presentation clock `2026-10-25T12:00:00Z`, six of ten
hosts with twenty packages (five security packages), 4,096-byte previews and
separate 32,768-byte detailed log chunks per host, ten command and four update
audit events per host, three daily tag-targeted scan-only policies, 80% synthetic
trusted keys and 20% missing trust. No disabled hosts, workers, scheduler or real
SSH. Auth/session expiry uses real time and newly issued disposable credentials;
auth randomness and its setup audit rows are excluded from the business digest.

| Servers | Active / approval / terminal | Manifest SHA-256 |
| --- | --- | --- |
| 10 | 1 / 2 / 7 | `83a0a075c1d957f02724f6ebae2b4850154a942c3e7c03e340ea26a4b745be4e` |
| 100 | 10 / 20 / 70 | `577321b8c735cb4b79938f769dae6108da5529f8a1c686abe66f45af2af1354d` |
| 1000 | 100 / 200 / 700 | `78acab0892ff0e71054b40242cb900c22ba14f4e686974461baccb40b31670ee` |

Go HTTP/service/JSON measurements use fifteen repetitions after one warmup;
packages run sequentially with `-p=1`. SQL counts are measured separately using an
executed-statement wrapper, including prepared statements and excluding driver
transaction-control methods. Browser measurements retain all five repetitions
after one warmup, with tracing and 1-ms CPU sampling enabled in both revisions.
Quantiles use nearest rank: browser p95 is the maximum of five, not a precise tail
estimate. Go p95 is also a small-sample estimate. See the [full protocol](../../dashboard-scaling.md).

## HTTP, SQLite, JSON and allocations

Milliseconds p50 / p95 below use the normal driver, real cookie authentication and
current-role middleware. Setup/password hashing/MFA enrollment are not timed read
operations. The cookie reader and the admin-owned PAT use existing #474 redaction;
those payload differences are not gains introduced by #475. PAT scopes contain
only `operational:read`; every request must succeed with HTTP 200 and the actual
120/minute limit remains enabled.

| Servers | Auth status cookie | Inventory admin | Dashboard without history | Legacy dashboard |
| --- | --- | --- | --- | --- |
| 10 | 0.18 / 0.27 | 0.53 / 1.37 | 1.41 / 2.11 | 2.28 / 3.44 |
| 100 | 0.13 / 0.18 | 2.33 / 3.42 | 7.33 / 9.15 | 11.97 / 15.07 |
| 1000 | 0.14 / 0.43 | 35.71 / 52.60 | 71.08 / 99.72 | 120.48 / 134.83 |

| Servers | Inventory reader cookie | Inventory read-only PAT | Dashboard read-only PAT |
| --- | --- | --- | --- |
| 10 | 0.31 / 0.77 | 0.27 / 0.66 | 1.35 / 1.67 |
| 100 | 1.43 / 2.00 | 1.66 / 2.55 | 7.08 / 8.95 |
| 1000 | 28.91 / 43.41 | 27.95 / 38.25 | 72.94 / 87.38 |

Uncompressed response bytes and statement counts (inventory admin / reader-PAT /
dashboard without history / legacy dashboard):

| Servers | Inventory admin bytes | Reader/PAT bytes | Dashboard bytes | Legacy bytes | SQL admin / reader / PAT / dashboard / legacy |
| --- | --- | --- | --- | --- | --- |
| 10 | 85,697 | 43,377 | 47,698 | 61,988 | 4 / 4 / 3 / 18 / 19 |
| 100 | 856,961 | 433,761 | 473,549 | 616,449 | 4 / 4 / 3 / 18 / 19 |
| 1000 | 8,569,601 | 4,337,601 | 4,731,996 | 6,160,996 | 4 / 4 / 3 / 19 / 21 |

Service measurements include data collection and domain projection; JSON is timed
separately. Collector microbenchmarks use the same SQLite business dataset with
default UTC formatting and ready facts, so they are a separate boundary from the
fully composed HTTP service. These phases and the auth endpoint are not an additive
latency decomposition. Service rows deliberately omit `json_bytes`.

| Servers | Inventory service | Dashboard service | Collector | Pure projection | Dashboard JSON |
| --- | --- | --- | --- | --- | --- |
| 10 | 0.04 / 0.07 | 0.96 / 2.01 | 0.68 / 1.71 | 0.03 / 0.34 | 0.06 / 0.11 |
| 100 | 0.52 / 2.26 | 5.04 / 7.18 | 3.63 / 4.64 | 0.50 / 1.34 | 0.64 / 1.23 |
| 1000 | 21.90 / 25.32 | 61.26 / 71.36 | 29.79 / 33.91 | 4.33 / 8.22 | 11.52 / 14.79 |

Allocation means are per operation, with HTTP response buffers included. Heap is
Go live heap after GC with the fixture loaded, not RSS or an isolated domain delta.

| Servers | Inventory allocations / bytes | Dashboard allocations / bytes | Go live heap |
| --- | --- | --- | --- |
| 10 | 872 / 303,925 | 4,209 / 706,949 | 1,851,048 |
| 100 | 4,908 / 3,362,530 | 31,681 / 4,761,973 | 3,476,296 |
| 1000 | 45,050 / 29,842,226 | 305,877 / 52,119,143 | 22,916,368 |

The [five-repeat public-boundary benchmarks](api/bench-http.txt),
[collector/projection benchmarks](api/bench-phases.txt), [CPU profile](api/cpu-top.txt)
and [allocation profile](api/alloc-top.txt) retain the underlying measurements.
JSON, SQLite scanning, snapshot copies and GC are visible backend costs; there is
no backend production rewrite. SQL stays grouped (one additional batch at 1,000).
Metered and unmetered runs are both retained in `api/go-*-metered-*.json`; counting
adds mutex/map work, so timings and allocations are not silently mixed.

## Browser before/after

The scenario uses real APIs: initial fleet rendering, filter to a pending host,
selection, hide/restore that selection, unchanged global fleet counters, package
and full-log detail, and an SSE-triggered refresh. Three real server-side stream
cancellations then create exactly four SSE connections in the final context.
Refresh waits for the response body and two animation frames; Playwright action
and tracing overhead is included. No timeout or slow successful trial is removed.


| Initial render — servers | Before p50 / p95 ms | After p50 / p95 ms |
| --- | --- | --- |
| 10 | 490.71 / 522.89 | 395.29 / 431.93 |
| 100 | 1898.60 / 1985.73 | 813.40 / 828.38 |
| 1000 | 17047.79 / 18447.54 | 3581.40 / 3797.56 |

| Filter — servers | Before p50 / p95 ms | After p50 / p95 ms |
| --- | --- | --- |
| 10 | 98.96 / 100.86 | 85.95 / 105.76 |
| 100 | 161.32 / 174.43 | 102.42 / 110.98 |
| 1000 | 895.19 / 959.36 | 195.61 / 214.07 |

| Selection — servers | Before p50 / p95 ms | After p50 / p95 ms |
| --- | --- | --- |
| 10 | 80.98 / 96.97 | 78.88 / 80.80 |
| 100 | 109.74 / 126.78 | 95.14 / 96.50 |
| 1000 | 512.09 / 529.85 | 208.24 / 223.80 |

| Detail — servers | Before p50 / p95 ms | After p50 / p95 ms |
| --- | --- | --- |
| 10 | 246.75 / 260.86 | 233.90 / 242.24 |
| 100 | 260.97 / 309.18 | 260.23 / 289.17 |
| 1000 | 662.02 / 3570.58 | 596.73 / 681.40 |

| Refresh — servers | Before p50 / p95 ms | After p50 / p95 ms |
| --- | --- | --- |
| 10 | 136.03 / 148.50 | 112.02 / 117.62 |
| 100 | 571.23 / 619.27 | 277.62 / 286.84 |
| 1000 | 6287.97 / 6986.10 | 1948.06 / 2027.63 |

At 1,000 hosts, substantial browser work remains: about 3.6 s initial and 1.95 s
refresh at p50 on this instrumented local runner. The 8.57 MB inventory response,
full domain snapshots, live-job logs, projection and DOM work still have costs.
This is not evidence that a production 1,000-host installation will meet an SLO.
Small timing differences, especially at ten hosts and for detail, do not establish
a general speedup. No UI layout, endpoint, permission registry or OpenAPI contract
changed.

Three production corrections follow the measured costs:

- One defensive domain snapshot is reused only inside a synchronous DOM render,
  including nested render calls; `finally` releases it. Unscoped reads and the
  next accepted state remain fresh.
- Bulk previews project navigation/selection rather than copying unrelated fleet
  logs/history and evaluating every Server's actions. Current `getAction`, frozen
  action interaction context and defensive public command plans remain intact.
- Reconnect reads target active/pending jobs, inspected detail and jobs followed
  before disconnection, even if they finish while offline. Unopened terminal detail
  stays on demand; superseded job responses are rejected.

| Servers | Log targets per reconnect before → after | Reconnect cycles | SSE connections | Errors |
| --- | --- | --- | --- | --- |
| 10 | 10 → 3 | 3 | 4 | 0 |
| 100 | 100 → 30 | 3 | 4 | 0 |
| 1000 | 1000 → 300 | 3 | 4 | 0 |

The final-context sampled CPU profile identifies `projectView` as a hotspot:
29.24 s inclusive before and 5.18 s after, across the same actions and
three reconnect cycles. Captured wall duration is 54.52 s / 16.20 s. Inclusive function
times overlap and must not be added. Raw profiles/traces remain local; sanitized
function summaries and every measured numeric sample are retained. Browser JS heap
snapshots are not GC-normalized; no retained-memory improvement or long-run leak
claim is made. Fixed browser clock instrumentation does not expose resource-timing
byte entries; logical JSON bytes come from the HTTP measurements above, not a
browser wire-transfer estimate.

## SSE delivery and cleanup

Each size separately runs one then five authenticated clients, three connection/
reconnection cycles, ten 256-byte log events at 5 Hz per two-second cycle. Latency
is from publish to receipt by every client. All thirty publications are delivered
(30 / 150 client deliveries), with zero errors/timeouts and no goroutine growth
within the measured window. This is a short delivery test, not a soak test.

| Servers | 1 client p50 / p95 ms | 5 clients p50 / p95 ms | SQL total 1 / 5 | Retained Go heap delta 1 / 5 bytes |
| --- | --- | --- | --- | --- |
| 10 | 0.604 / 0.920 | 1.515 / 1.868 | 117 / 585 | 24,784 / 87,456 |
| 100 | 0.592 / 0.895 | 1.573 / 2.002 | 117 / 585 | 27,688 / 82,176 |
| 1000 | 0.668 / 0.922 | 1.486 / 1.774 | 117 / 585 | 18,560 / 86,040 |

## CI budgets and behavioral protections

`TestDashboardScalingBudgets` checks all three sizes with no absolute latency gate.
Byte limits include a fixed 1,024-byte allowance; allocation-count limits include
2,000 fixed allocations. SQL limits count actual executed statements.


| Inventory admin — servers | Max JSON bytes | Max SQL | Max allocations/op |
| --- | --- | --- | --- |
| 10 | 91,024 | 4 | 2,600 |
| 100 | 901,024 | 4 | 8,000 |
| 1000 | 9,001,024 | 4 | 62,000 |

| Inventory reader / PAT — servers | Max JSON bytes | Max SQL | Max allocations/op |
| --- | --- | --- | --- |
| 10 | 47,024 | 4 / 3 | 2,600 |
| 100 | 461,024 | 4 / 3 | 8,000 |
| 1000 | 4,601,024 | 4 / 3 | 62,000 |

| Dashboard without history — servers | Max JSON bytes | Max SQL | Max allocations/op |
| --- | --- | --- | --- |
| 10 | 53,024 | 18 | 6,000 |
| 100 | 521,024 | 18 | 42,000 |
| 1000 | 5,201,024 | 19 | 402,000 |

| Legacy dashboard — servers | Max JSON bytes | Max SQL | Max allocations/op |
| --- | --- | --- | --- |
| 10 | 69,024 | 19 | 7,500 |
| 100 | 681,024 | 19 | 57,000 |
| 1000 | 6,801,024 | 21 | 552,000 |

Tests also protect global fleet/package counts, exact status proportions, action
eligibility, full inventory, eight-row legacy history versus on-demand detail,
role-based preview redaction, and absence of detailed log chunks in list payloads.
Frontend tests cover selection outside the filter, canonical approvals/actions,
new eligibility after a snapshot, defensive plans, obsolete replies, log completion
during an outage, selected terminal detail, unopened terminal jobs, and render
snapshot freshness/nesting/exception cleanup. The bulk preview budget intentionally
uses empty filters; global package/risk filtering can still inspect the whole fleet.

## Reproduction and validation

Run the [measurement script](../../../tools/performance/dashboard.sh) in an isolated
output directory. For this exact frontend pair, compile the current test harness
once, verify production Go/go.mod/go.sum have no changes against the base, archive
tracked reference files, and serve both roots with that identical binary:

```sh
repo_root="$PWD"
reference_root="$(mktemp -d /tmp/slu-dashboard-reference.XXXXXX)"
git archive --format=tar -o /tmp/slu-dashboard-reference.tar 22b7c20d1a40128876b528781ead3277a76c3c19
tar -xf /tmp/slu-dashboard-reference.tar -C "$reference_root"
GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache GOMAXPROCS=2 go test -c -o /tmp/slu-dashboard.test .
(cd "$reference_root" && node "$repo_root/tools/performance/dashboard-browser.cjs" /tmp/slu-dashboard.test /tmp/slu-before)
node tools/performance/dashboard-browser.cjs /tmp/slu-dashboard.test /tmp/slu-after
python3 tools/performance/provenance.py "$reference_root" 22b7c20d1a40128876b528781ead3277a76c3c19
python3 tools/performance/provenance.py
```

The same binary is valid here because the backend hash is identical; a future
backend change requires separately built exact reference sources. Raw trace ZIPs
can contain disposable session cookies and remain in `/tmp/slu-475-v2-before` and
`/tmp/slu-475-v2-after`. No raw trace, credential or installation data is committed.
Only version 2 evidence is included.

[Validation results](validation.json) record commands, exit codes and local logs.
The complete Go suite, full race suite, 76.3% coverage (73% required), vet, build,
staticcheck, 205 real Node tests and 97 Playwright cases passed. The final browser
supervisor records separate Playwright/server exit codes of zero. An earlier
invocation printed 97 passed but returned 143 during webServer group shutdown; it
was not accepted as success and was replaced with the owned test installation.
Staticcheck initially found deprecated test-driver methods; context-aware statement
adapters replaced them and the final run passed.

The full browser suite runs through `dashboard-e2e.cjs`, with an empty owned DB,
config and known-hosts file; scheduler and host workers never start, SSH dial is
forbidden and host-key scanning uses an unavailable transport. Coverage and race
validation use only existing synthetic fixtures.

Presentation smoke evidence: [desktop](../../../.github/assets/dashboard-scaling-475/desktop.png),
[mobile](../../../.github/assets/dashboard-scaling-475/mobile.png). Layout, theme,
CSP and mobile containment are preserved.
