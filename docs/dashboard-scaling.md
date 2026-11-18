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
full Dashboard Projection. A fingerprint of the compact rows lets the browser
reject a partial update if other rows changed, including when host facts or CVE
evidence expire independently. The measured gain is in browser transfer and fewer
requests. Scheduled transitions, inventory changes, unrecognized events and
reconnects retain full snapshots. A missed revision, missing host, changed
inventory identity, overlapping read or rejected patch also triggers a fleet read.
Job logs keep their independent sequence and bounded recovery. Contiguous
persisted audit events refresh snapshots without requesting all active logs again.

Run the standalone scenario with the same fixture on the reference
`4088ea51e776d65477141101e867dd843609d6a7` and the measured candidate
`ec83f6219111ec5b6786215d5942404bcc897141`. To instrument the reference, apply
only the candidate diff for `dashboard_scaling_test.go` and
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
| 100 | 1 | One host | 3 → 2 | 96,420 → 3,989 | 161 → 172 ms | 167 → 182 ms |
| 100 | 1 | Three hosts | 3 → 2 | 96,326 → 4,733 | 154 → 164 ms | 158 → 172 ms |
| 100 | 3 | One host | 9 → 6 | 289,260 → 11,967 | 208 → 213 ms | 251 → 252 ms |
| 100 | 3 | Three hosts | 9 → 6 | 288,978 → 14,199 | 171 → 193 ms | 215 → 217 ms |
| 1,000 | 1 | One host | 3 → 2 | 932,989 → 3,984 | 351 → 471 ms | 388 → 523 ms |
| 1,000 | 1 | Three hosts | 3 → 2 | 933,713 → 4,733 | 318 → 427 ms | 393 → 505 ms |
| 1,000 | 3 | One host | 9 → 6 | 2,798,967 → 11,952 | 617 → 726 ms | 843 → 861 ms |
| 1,000 | 3 | Three hosts | 9 → 6 | 2,801,139 → 14,199 | 553 → 578 ms | 609 → 770 ms |

Chromium 159.0.8154.6 used the same fixture manifest before and after on one
machine with the scenarios run sequentially. All targeted trials avoided both
fleet reads. In this loopback run, render p50 and p95 increased in every
scenario. The targeted update is retained for the transfer reduction, which
exceeds 99% at 1,000 hosts. CI compares the partial rows and fleet counters
to full reads for admin and reader. It caps the uncompressed response for
three hosts at 16 KiB and 5% of the two full responses, and checks one request
for a scoped update in Chromium. Latency has no CI threshold.

## Status refresh latency (#593)

The targeted scenario now captures five server phases through `Server-Timing`.
**Wait** covers admission to the serialized projection. **Collect** includes
reading SQLite and preparing the inputs from runtime, readiness and policy
facts. **Project** constructs the Dashboard Projection from those inputs.
**Compact** prepares and fingerprints its compact rows. **Encode** serializes
the selected rows and compresses them when negotiated. In Chromium the fixture
also records elapsed time between decoding the scoped JSON and detecting the
updated rows. The full render time starts just before the fixture publishes the
change, so it includes notification, transport, server and browser time.

The browser change reads a small defensive synchronization snapshot for the
sync indicator and interaction-depth checks. These frequent reads previously
rebuilt the fleet view. The accepted dashboard keeps one owned set of compact
rows, copies only new rows on a targeted refresh, and copies the dashboard once
in each defensive view. Revision and row-version checks continue to take the
full-resynchronization path when required.

An experimental variant shared the next Dashboard Projection among same-revision
callers already queued behind an active calculation. It closed the group before
sampling the clock and calculated a fresh projection for later arrivals. The
three-tab browser requests were staggered. None of the measured bursts shared a
projection, so the shipped change keeps the existing server calculation path.

The runs used Chromium 159.0.8154.6 on the same installation and fixture
manifest. Each cell uses five trials after a warmup, with the 5 or 15 tab
observations ranked together for latency. Reference is `4bd324b` with the
instrumentation applied in `ceea02846772aa8fb060084f90c1cfedc81219f3`. Copy
the candidate's `tools/performance/dashboard-browser.cjs` into that reference
checkout and run the standalone commands above in both checkouts. The JSON
reports include `server_ms` and `after_response_ms` for each tab. “Sync only”
applies the small synchronization
snapshot. “Server experiment” adds the pending-group change to sync only.
“Accepted” applies both browser changes with the reference server.

The table shows render p50/p95 in milliseconds. Request and encoded response
body byte medians were identical across variants. All runs verified one
scoped fleet request per tab and recovered both full snapshots on reconnect.

| Hosts | Tabs | Change | Requests / bytes | Reference | Sync only | Server experiment | Accepted |
| ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: |
| 100 | 1 | One host | 2 / 3,989 | 225 / 235 | 198 / 237 | 198 / 255 | 161 / 174 |
| 100 | 1 | Three hosts | 2 / 4,714 | 197 / 214 | 184 / 230 | 170 / 198 | 164 / 168 |
| 100 | 3 | One host | 6 / 11,967 | 303 / 480 | 270 / 345 | 270 / 357 | 202 / 235 |
| 100 | 3 | Three hosts | 6 / 14,142 | 349 / 433 | 231 / 347 | 240 / 384 | 184 / 226 |
| 1,000 | 1 | One host | 2 / 3,984 | 685 / 866 | 620 / 785 | 548 / 637 | 408 / 521 |
| 1,000 | 1 | Three hosts | 2 / 4,714 | 664 / 874 | 567 / 661 | 559 / 658 | 337 / 373 |
| 1,000 | 3 | One host | 6 / 11,952 | 1,207 / 1,967 | 1,100 / 2,017 | 1,105 / 1,877 | 663 / 911 |
| 1,000 | 3 | Three hosts | 6 / 14,142 | 941 / 1,312 | 750 / 1,371 | 753 / 1,147 | 475 / 584 |

Phase medians for reference → accepted are in milliseconds. The browser
column covers JSON decode completion through the observed row change. The
phase medians summarize different observations and need not sum to the
median end-to-end time. The server calculation is unchanged between these
two variants, so its duration differences describe run-to-run variation.

| Hosts | Tabs | Change | Wait | Collect | Project | Compact | Encode | Browser |
| ---: | ---: | --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 100 | 1 | One host | 0.0 → 0.0 | 10.2 → 5.5 | 0.8 → 0.4 | 2.5 → 1.0 | 0.2 → 0.3 | 68 → 34 |
| 100 | 1 | Three hosts | 0.0 → 0.0 | 10.4 → 5.3 | 0.8 → 0.3 | 2.6 → 1.1 | 0.4 → 0.3 | 53 → 31 |
| 100 | 3 | One host | 0.0 → 0.0 | 12.7 → 6.5 | 1.0 → 0.4 | 2.6 → 1.4 | 0.4 → 0.8 | 123 → 62 |
| 100 | 3 | Three hosts | 0.0 → 0.0 | 24.0 → 6.0 | 2.1 → 0.4 | 5.7 → 2.0 | 1.3 → 0.4 | 103 → 49 |
| 1,000 | 1 | One host | 0.0 → 0.0 | 134.4 → 65.7 | 9.8 → 4.6 | 30.7 → 16.9 | 0.2 → 0.2 | 303 → 123 |
| 1,000 | 1 | Three hosts | 0.0 → 0.0 | 103.4 → 54.3 | 9.4 → 4.8 | 29.3 → 13.5 | 0.5 → 0.3 | 327 → 133 |
| 1,000 | 3 | One host | 0.0 → 0.0 | 149.5 → 74.8 | 15.1 → 7.4 | 33.9 → 19.8 | 0.3 → 0.2 | 466 → 236 |
| 1,000 | 3 | Three hosts | 0.0 → 0.0 | 123.1 → 56.7 | 12.3 → 6.6 | 20.9 → 15.2 | 0.3 → 0.3 | 366 → 162 |

At 1,000 hosts and three tabs, the accepted browser change reduced the p95
from 1,967 to 911 ms for one host and from 1,312 to 584 ms for a burst
on this machine. CI checks that sync reads skip fleet projection and that
adapter and consumer inputs stay isolated. Existing browser and API budgets
still cover row consistency, permissions, reconnect behavior and transferred
volume. Timings describe this local fixture and have no CI threshold.
