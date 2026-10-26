# Auth Session Command — issue 474 validation

Validated on 2026-10-26 with Go 1.26.10, Node 22.23.3 and staticcheck 2026.1
(v0.7.0). All data, keys, passwords, factors and tokens were synthetic. No existing
SSH host, host APT operation, Docker context or persistent user configuration was
changed. Real SSH/APT qualification used only owned disposable containers. The pre-existing untracked `static/prototypes/` files and stashes were
excluded from changes.

## Delivered behavior and boundaries

- Transactional migration keeps account 1, username/password hash, encrypted
  inventory and closed setup. Only pre-identity sessions are signed out once;
  newly issued sessions survive subsequent schema checks and restarts.
- Central authorization declares 102 protected method/route pairs. The matrix
  exercises reader/operator/administrator with real cookies, personal Bearer
  credentials and anonymous requests, including an undeclared-route refusal.
  The 25 personal Bearer operations are covered by the versioned OpenAPI
  contract. Credentials/inventory, policies, settings and backup remain cookie
  surfaces; `/metrics` retains its separate Metrics Access Credential.
- Session/password/revocation commands target one stable identity. Per-account
  credential epochs isolate in-flight login/MFA from changes to other accounts.
  An installation epoch rejects old authentication after persistence replacement
  even when restored account IDs and counters coincide. Atomic last-admin
  triggers, concurrent revocation and already-open SSE are covered.
- RFC 4226 and 6238 vectors, confirmed encrypted enrollment, incomplete MFA with
  no authenticated session, OTP replay, concurrent single-use recovery codes,
  challenge expiry, rate limits and sensitive reauthentication are covered.
- Personal tokens are named, expiring, hash-only and single-display. Tests cover
  current-role intersection, disablement, revocation, isolation, safe usage
  metadata and secret absence from listings/database fields. Stable audit actors
  include account and non-secret token IDs. An anonymous failed login cannot be
  attributed to a named account whose username is `unknown`.
- Real local recovery CLI tests target one account among several for each of
  `password`, `mfa` and `password,mfa`, checking installation locking, audit,
  unaffected accounts, sessions and token revocation.
- A real encrypted archive export/HTTP restore contains MFA, sessions and a PAT
  that is revoked after export. Restore cannot resurrect its cookie/token or
  recovery codes. The archived metrics credential becomes valid and the
  post-export metrics credential becomes invalid, preserving its old contract.
  A separate test verifies MFA rewrapping between different installation keys.
- A cookie-free HTTP workflow creates a token, runs discovery, reads the exact
  plan identity, rejects a read-only approval, approves with the permitted token,
  completes the real maintenance runner through a scripted SSH transport, and
  verifies job/audit actor plus audit metadata. Existing Approval Scope and
  package-deferral contracts remain enforced.

## Executed checks

All Go commands used `GOCACHE=/tmp/simplelinuxupdater-analysis-go-cache` and
`GOMAXPROCS=2`. Local HTTP/SSE integration checks and Playwright used the required
loopback sandbox escalation.

| Check | Result | Local evidence |
| --- | --- | --- |
| `go test -timeout=180s ./...` | Pass, complete command exit 0; root package 56.094 s | `/tmp/slu-474-go-delivery-final.log` |
| `go test -race -count=1 -timeout=600s ./...` | Pass, complete command exit 0; root package 232.204 s, no race report | `/tmp/slu-474-race-delivery.log` |
| `go test -count=1 -covermode=atomic -coverprofile=/tmp/slu-474-coverage-delivery.out -timeout=240s ./...` | Pass, complete command exit 0; total **76.2%**, minimum 73% | `/tmp/slu-474-coverage-delivery.log`, `/tmp/slu-474-coverage-delivery.out` |
| `go vet ./...`, `go build ./...`, `gofmt`, `git diff --check` | Pass | Each command exit 0; `/tmp/slu-474-quality-delivery-final.log` and `.json` |
| `staticcheck ./...` with a temporary writable cache | Exit 0, v0.7.0 | `/tmp/slu-474-quality-delivery-final.log`, `/tmp/slu-474-staticcheck-delivery.log` |
| `npm run test:unit` with local IPC enabled | **200 individual tests passed** | `/tmp/slu-474-jsunit-real-final.log` |
| `npm run test:e2e -- --workers=1` | **96 passed**, 2.7 min: 94 historical + 2 isolated identity flows | `/tmp/slu-474-playwright-full-second.log` |
| Final-binary setup/login + accounts/PAT/MFA Playwright selection | **5 passed**, 15.5 s, after per-account/restore epoch changes | `/tmp/slu-474-playwright-final.log` |
| Final audit-sentinel correction, route matrix and obsolete-credential tests with `-race` | Pass, 7.779 s; vet/staticcheck/build rechecked | `/tmp/slu-474-audit-regression.log` |
| Production-config concurrent MFA/last-admin repetition, `go test -race -count=20` | Pass, exit 0, 16.822 s | `/tmp/slu-474-auth-concurrency-repeat.log` |
| SSH/APT harness safety and cleanup unit checks | 19 passed | `/tmp/slu-474-harness-safety.log` |
| Real Debian 13 / Ubuntu 24.04 SSH/APT qualification | 7 real scenarios passed per distribution; cleanup passed | `debian13/report.json`, `ubuntu2404/report.json` |

One earlier complete run (`/tmp/slu-474-go-delivery.log`) exited 1: the new
concurrent recovery-code test used SQLite's unlimited default connection pool
instead of production's `MaxOpenConns(1)` and `busy_timeout=5000`, producing a
transient SQL-lock error under load. The fixture now explicitly matches those
production settings. The assertion remains strict: exactly one acceptance and
all other results must be `ErrFactorInvalid`, with unexpected SQL errors failing
the test. Twenty race-enabled repetitions passed, followed by the corrected
complete Go command above (exit 0). This failed attempt is not counted as a pass.
The full race/coverage commands exited 0 on unchanged production code; the final
fixture-only correction has the repeated race proof and complete Go rerun.

The full Go/race/coverage runs cover the final account/installation epochs,
async runner audit identity, bounded metadata retention and audit-sentinel
correction. The last focused race run supplies additional regression evidence. The 96-test browser run precedes the epoch
refinement; the five sensitive flows were rerun against the resulting final
backend binary. Sandboxed Node initially reported only file wrappers: enabling
local IPC produced the actual 200-test result, also confirmed with Node's
`--experimental-test-isolation=none`. No file-wrapper count is presented as an
individual-test count.

Every new browser scenario starts and removes its own process, loopback port,
database, encryption config and lock directory. It cannot alter the historical
suite's passwords, MFA or sessions. The complete suite fixed and verified the
hidden MFA form's interaction with the existing form layout and its shared
navigation contract.

## Captures and operational notes

- [Desktop account page](account-desktop.png)
- [390 px mobile account page](account-mobile.png)

Captures show cleared password inputs, no displayed PAT/MFA/recovery secret and
only synthetic account/token names and masked loopback session metadata.

See [behavior, permissions, migration, recovery and curl workflow](../../personal-api.md)
and [OpenAPI v1](../../openapi-v1.json). No new environment variables or public
exposure are introduced. Previously admitted work can finish after a later
revocation; every subsequent request/stream emission revalidates authorization.
Restore invalidates archived recovery codes and pending enrollment, preserves
the encrypted confirmed authenticator and rejects current/adjacent OTP steps
for up to 90 seconds. Regenerate recovery codes after restoration. Real SSH/APT
qualification passed on Debian 13 (18.518 s) and Ubuntu 24.04 (17.970 s).
Each report explicitly marks systemd, reboot and official-provenance qualification
not executed: these containers have no systemd PID1, do not prove an OS reboot,
and use a local synthetic package repository. No fake systemctl/reboot proof was
introduced. The root helper/business commands were unchanged.

The harness now verifies setup account ID 1, username `fixture-admin`, enabled
administrator role, and exact `account:1` job/completion actor with
`account_id: 1` and no personal-token actor field for its cookie session.
A real completion larger than 4096 bytes exercised the truncation boundary;
stable account/token facts are now explicitly retained in bounded metadata.
Reports and redacted application logs are included in [Debian evidence](debian13/report.json)
and [Ubuntu evidence](ubuntu2404/report.json). Harness cleanup plus a final Docker
label query confirmed no owned container, network or image remained. Private
fixture directories were removed, and evidence contains no password/private-key
material. The qualification logs are `/tmp/slu-474-ssh-apt-debian13-final.log` and
`/tmp/slu-474-ssh-apt-ubuntu2404-final.log`.
