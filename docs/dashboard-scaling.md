# Measuring Dashboard Projection

The synthetic measurement harness exercises Server inventory, Dashboard Projection,
Status Page Interaction and authenticated dashboard events at 10, 100 and 1,000
Servers. It is a test composition: it never calls the application's `main`, starts
a scheduler or host-facts worker, or connects to SSH. A failing transport and an
assertion protect against accidental SSH dispatch. SQLite, known hosts, listeners
and accounts belong to temporary fixtures. Do not use an installation database.

Run from the repository root with the Go version in `go.mod`, the dependencies
from `npm ci`, and Playwright Chromium installed:

```sh
GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache GOMAXPROCS=2 \
  tools/performance/dashboard.sh /tmp/slu-dashboard-measurement
```

This needs local loopback sockets and Chromium IPC. It does not need Docker,
network access, a real Server, or operator credentials. The test binary exposes
fixture-only controls on a temporary loopback listener; the production binary has
no measurement routes or authentication override. Exit status is nonzero on an
HTTP error, fixture failure, assertion or browser timeout. Stop and investigate
errors rather than removing slow or failed trials from the distribution.

The version 2 generator fixes seed 475 and UTC time `2026-10-25T12:00:00Z`. Each ten hosts
contain one active job, two pending approvals and seven terminal jobs. Six have
20 packages (five security packages); every host has a 4,096-byte compatibility
preview and a separate 32,768-byte detailed log chunk. Each host has ten command
audit events, four completed updates and fresh health facts. Three daily scan-only
policies target three of five tags. Eight of ten hosts have a trusted, purely
synthetic Ed25519 key; the others have missing trust. No host is disabled. The
manifest records these dimensions and a SHA-256 of the generated public facts and
history recipe. `provenance.json` records production source identity and runtime
environment. Match both the manifest and tool version when comparing revisions.
Persisted job statuses/phases use the Jobs constants; tests cross-check inventory
and Dashboard Projection against all three status proportions. Auth/session expiry
continues to use the real clock with fresh disposable credentials on each run.

Go measurement packages run sequentially (`-p=1`) and report 15 operations after one warmup, nearest-rank p50/p95,
allocations, allocated bytes and uncompressed JSON size. The regular driver and a
separately measured SQL-counting wrapper exercise the same fixture. Cookie admin,
cookie reader and an admin-owned `operational:read` PAT pass through the real auth,
current-role and response-redaction middleware. The PAT retains the production
120/minute limit and makes 32 calls per fixture, below that limit; distinct fixture
sizes represent distinct clients. MFA enrollment/password verification are outside
these authenticated read-operation timings. The auth status endpoint reports the
request authentication floor; service and JSON phases give additional boundaries,
not an additive subtraction model for noisy timings.

Go benchmarks repeat five times for HTTP, services, collection and pure projection.
The collector microbenchmark uses real SQLite health, policy, job and audit reads,
with default UTC timestamp formatting and readiness facts; its timings are not
interchangeable with the fully composed HTTP service. CPU and allocation profiles
cover the public 1,000-Server Dashboard HTTP path.

The browser scenario uses real authenticated APIs, no mocked payloads, a desktop
viewport, five fresh contexts after one warmup, Playwright tracing and 1-ms Chromium
CPU sampling. It filters, selects a host, hides and restores the selection, checks
global fleet counts, opens package/log detail, and requests a refresh through SSE.
The final context performs three controlled stream disconnections and verifies
that new SSE requests open and the log request burst drains. Browser timings
include Playwright actions and two animation frames; refresh waits for the response
body and two frames. Small-n p95 is the maximum of five measured trials. The
standalone SSE scenario separately delivers 256-byte log events at 5 Hz to one and
five clients for three two-second cycles per size, checking all deliveries and
reporting allocation, retained Go heap and goroutine deltas. These short windows
are not a soak test or a production capacity claim.
Browser `Date` is fixed to the manifest's UTC clock; timers and performance clocks
continue normally. Browser reads use the UI's 7-day window; Go API benchmarks use
24 hours. All fixture history is within 14 hours, so both read the same records.

HTTP Go benchmarks use `httptest.ResponseRecorder`, including its response-buffer
allocations; browser tests use real loopback HTTP. SQL counts include executed
query/DML calls and prepared statements, excluding driver transaction-control
methods. The collector microbenchmark has simpler readiness/time formatting than
the composed HTTP application. Budgeted bulk-preview tests use empty filters;
global package/risk filtering still inspects the fleet when required.

Inspect traces locally with `npx playwright show-trace /tmp/slu-dashboard-measurement/chromium-1000.zip`.
Traces can contain disposable session cookies. Keep raw traces/profiles private;
share only inspected summaries or regenerate traces in a disposable environment.
`summarize-browser-profile.cjs` produces function-level CPU summaries without
request credentials. The harness removes its executable; Node closes each browser
context, stops each server, waits for its exit and deletes fixture connection facts.

## Compact Status projection (#569)

In the authenticated browser scenario, Status requests the compact projection of
the inventory and Dashboard Projection. Inventory sends filter, selection, and
approval facts. The dashboard still supplies global totals and canonical action
eligibility. The previous inventory repeated package rows, CVE evidence, package
transactions, and log previews on every refresh. The dashboard also repeated CVE
IDs and risk fields already represented by the per-host triage counters and labels.
The browser fetches packages, approval impact and bounded log previews per host.
It matches the selected inventory identity, job, approval generation, deferral
revision and remediation digest before accepting details.

Same Chromium, fixture, viewport and five-trial protocol on one machine. The
reference is `fc7b5b5`. Bytes are the sum of the two fleet response bodies
reported by Playwright, including dashboard gzip in the transferred column. The
first pair on initial load and the pair requested by a single SSE event had the
same sizes in each trial.

| Hosts | Transferred bytes before → after | Decoded bytes before → after | First row p50 before → after | SSE refresh p50 before → after |
| ---: | ---: | ---: | ---: | ---: |
| 100 | 889,523 → 93,965 | 1,303,153 → 493,493 | 819 → 614 ms | 257 → 168 ms |
| 1,000 | 8,886,914 → 931,352 | 13,028,719 → 4,932,119 | 3,324 → 2,114 ms | 1,797 → 979 ms |

The on-demand package and log interaction took 550 → 412 ms p50 at 1,000 hosts.
In the three forced reconnects, Chromium still requested logs for 300 distinct
jobs per cycle at that size. These times describe a loopback fixture. CI budgets
the compact JSON at 760 bytes per inventory row and 4,300 bytes per dashboard row,
each with 1 KiB for fixed overhead. The legacy API budget and redaction assertions
continue to run alongside the compact assertions.

## Targeted SSE refresh and bounded log recovery (#577)

The browser fixture emits 12 `job.update` events at 8 ms intervals after
changing one visible host. Status groups invalidations within a 125 ms window.
Job changes invalidate inventory and Dashboard Projection. Policy changes can
also invalidate the policy summary and recent audit. Initial page reads wait
briefly for the subscription so a fast connection needs no extra startup refresh.
If the initial reads start before the stream opens, another set reconciles
changes missed during startup. A reconnect or an unknown event also refreshes
the full snapshot set. Structured job log events continue to bypass snapshot
refreshes.

Reference `941f880` and candidate were measured on Chromium 159.0.8154.6 with
five measured trials per size. Counts and transferred body bytes are medians.
The aggregate median is calculated per trial. The audit path includes command
history requested for the inspected host after Dashboard Projection changes.

| Endpoint | 100 hosts requests before → after | 100 hosts bytes before → after | 1,000 hosts requests before → after | 1,000 hosts bytes before → after |
| --- | ---: | ---: | ---: | ---: |
| `/api/servers` | 3 → 1 | 198,165 → 66,055 | 2 → 1 | 1,320,830 → 660,415 |
| `/api/dashboard/summary` | 3 → 1 | 83,397 → 27,910 | 2 → 1 | 541,874 → 270,937 |
| `/api/keys/global` | 3 → 0 | 51 → 0 | 2 → 0 | 34 → 0 |
| `/api/audit-events` | 5 → 1 | 12,520 → 2,369 | 4 → 2 | 9,944 → 4,738 |
| `/api/observability/summary` | 3 → 0 | 6,588 → 0 | 2 → 0 | 35,008 → 0 |
| `/api/update-policies` | 2 → 0 | 5,802 → 0 | 2 → 0 | 24,164 → 0 |
| **Total** | **18 → 3** | **304,435 → 96,334** | **15 → 4** | **1,932,925 → 936,090** |

The changed row rendered in 446 → 306 ms p50 at 100 hosts and 2,435 → 966 ms
at 1,000. Three forced reconnects per size each requested 30 and 300 distinct log
jobs respectively, as before. Peak concurrent log reads fell from 30 to 8 and
from 300 to 8. Status puts the open log ahead of queued background jobs and
continues recovery by sequence for jobs followed before the outage.

Regular CI runs `TestDashboardScalingBudgets` at all three sizes. It bounds JSON
bytes, actual executed SQL statements and allocations, checks global counts and
action eligibility, validates role redaction, and rejects detailed log chunks in
list payloads. There is no hardware-dependent latency threshold. See the
[measured reference and comparison](validation/dashboard-scaling-475/README.md)
for exact budgets, results and limitations.

The recorded browser comparison is pinned to its measured source revisions.
Later reconnect race corrections are qualified with separate unit/browser
regressions in that report; the historical latency samples are not measurements
of those subsequent changes.

## Per-host Status refresh (#587)

Standalone manual update transitions include a host name and a dashboard
revision. Status groups up to 16 distinct names per burst and requests their
compact inventory and dashboard rows together. The same response carries the
authoritative global counters. The server still computes those counters from the
full Dashboard Projection. The measured gain is in browser transfer and fewer
requests. Scheduled transitions, inventory changes, unrecognized events and
reconnects retain full snapshots. A missed revision, missing host, changed
inventory identity, overlapping read or rejected patch also triggers a fleet read.
Job logs keep their independent sequence and bounded recovery.

Run the standalone scenario with the same fixture on the reference
`4088ea51e776d65477141101e867dd843609d6a7` and the candidate. To instrument the
reference, apply only the candidate diff for `dashboard_scaling_test.go` and
`tools/performance/dashboard-browser.cjs` to a separate reference checkout.
With the Go and Playwright dependencies installed, run in each checkout:

```sh
out=$(mktemp -d)
go test -c -o "$out/scaling.test" .
SLU_SCALING_TARGETED=1 node tools/performance/dashboard-browser.cjs "$out/scaling.test" "$out"
```

The two `host-refresh-*.json` files record five trials after one warmup at each
size and tab count. A trial changes one host, then three hosts within one burst.
Each tab records when the changed row or rows reach their expected status.
Requests and encoded response body bytes are medians of each trial's total
across the tabs, including the selected host's command-history read. Latencies
use nearest-rank percentiles across the five or fifteen tab observations. The
last trial forces a disconnect and checks that all tabs reopen SSE and request
both fleet snapshots. The original scenario above separately exercises bounded
log recovery.

| Hosts | Tabs | Change | Requests before → after | Encoded bytes before → after | Render p50 before → after | Render p95 before → after |
| ---: | ---: | --- | ---: | ---: | ---: | ---: |
| 100 | 1 | One host | 3 → 2 | 96,420 → 3,953 | 164 → 171 ms | 172 → 178 ms |
| 100 | 1 | Three hosts | 3 → 2 | 96,326 → 4,670 | 149 → 169 ms | 199 → 177 ms |
| 100 | 3 | One host | 9 → 6 | 289,260 → 11,859 | 197 → 227 ms | 220 → 300 ms |
| 100 | 3 | Three hosts | 9 → 6 | 288,978 → 14,010 | 171 → 187 ms | 186 → 230 ms |
| 1,000 | 1 | One host | 3 → 2 | 932,989 → 3,948 | 376 → 410 ms | 416 → 439 ms |
| 1,000 | 1 | Three hosts | 3 → 2 | 933,713 → 4,669 | 335 → 374 ms | 350 → 397 ms |
| 1,000 | 3 | One host | 9 → 6 | 2,798,967 → 11,844 | 627 → 659 ms | 957 → 896 ms |
| 1,000 | 3 | Three hosts | 9 → 6 | 2,801,139 → 14,007 | 517 → 654 ms | 650 → 731 ms |

Chromium 159.0.8154.6 used the same fixture manifest before and after on one
machine. All targeted trials avoided both fleet reads. The latency results
vary by scenario. The targeted update is retained for the transfer reduction,
which exceeds 99% at 1,000 hosts. CI compares the partial rows and fleet
counters to full reads for admin and reader. It caps the uncompressed response
for three hosts at 16 KiB and 5% of the two full responses, and checks one
request for a scoped update in Chromium. Latency has no CI threshold.
