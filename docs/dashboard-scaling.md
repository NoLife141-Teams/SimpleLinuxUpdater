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

Regular CI runs `TestDashboardScalingBudgets` at all three sizes. It bounds JSON
bytes, actual executed SQL statements and allocations, checks global counts and
action eligibility, validates role redaction, and rejects detailed log chunks in
list payloads. There is no hardware-dependent latency threshold. See the
[measured reference and comparison](validation/dashboard-scaling-475/README.md)
for exact budgets, results and limitations.
