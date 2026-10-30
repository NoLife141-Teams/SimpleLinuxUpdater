# Named accounts and personal API v1

The existing setup account becomes account `1`, with role `admin`. Its username,
password hash, inventory and encrypted credentials are preserved. Setup stays
closed. The one-time identity migration signs out old browser sessions. Sign in
with the existing credentials. Subsequent restarts preserve newly issued sessions.
For a single administrator, keep this account and continue using the existing
pages; additional accounts, MFA and personal tokens are optional.

Administrators use **Account** to create named accounts, change roles and disable
accounts. Usernames are unique, immutable and never reused through a delete API.
Account IDs are the stable audit identity. SQLite atomically prevents deleting,
disabling or demoting the last enabled administrator. Changes revoke that
account's browser sessions; tokens immediately use the current enabled role.

## Permissions

Every protected method and route has an explicit permission in
`auth_permissions.go`. Undeclared routes fail closed. Permissions apply to direct
API requests, downloads and streams as well as pages.

| Permission | Reader | Operator | Administrator | Personal Bearer |
| --- | --- | --- | --- | --- |
| `operational:read` | Yes | Yes | Yes | Yes |
| `maintenance:write` | No | Yes | Yes | Yes |
| `approvals:write` | No | Yes | Yes | Yes |
| `logs:read` | No | Yes | Yes | Yes |
| `audit:read` | No | No | Yes | Yes |
| `inventory:write` | No | No | Yes | No |
| `policies:write` | No | No | Yes | No |
| `settings:write` | No | No | Yes | No |
| `backup:write` | No | No | Yes | No |

Operational reads include server status, package plans, vulnerabilities,
observability, health trends, applied deferral snapshots and timezone. They
exclude raw command output. `/api/servers` returns an empty `logs` preview and no
application-check profile without `logs:read`; dashboard reads omit command
history. Cookie-only dashboard SSE removes command `data`, stream and sequence
without `logs:read`. Every emission revalidates the durable session and current
identity; a downgraded or disabled account receives no later events.

Job details, raw log pagination and job report downloads require `logs:read`.
Full audit listing, audit report downloads and complete filtered JSON/CSV searches
(`POST /api/audit-events/export`) require `audit:read`. Export limits, manifest and
CSV formula neutralization are documented in [usage](usage.md#export-inventories-and-audit-searches).
Inventory JSON v1 export (`POST /api/servers/export`) requires administrator cookies. Inventory,
SSH credentials, trust management, sudo configuration, import/onboarding,
policies, deferral administration, application-check definitions, notifications,
backup exports/restores, metrics credential management and account administration
require administrator cookies. Restore additionally requires recent
reauthentication. These are intentionally outside the personal Bearer surface.

Session listing/revocation, password change, identity, factor and personal-token
management always use the current cookie identity. IDs cannot select another
account's sessions or tokens. Cookie writes retain same-origin checks. A Bearer
header on a cookie-only route is rejected even when a browser cookie is present.

## MFA and sensitive changes

Account offers optional TOTP enrollment. Add the displayed secret to an
authenticator, then confirm with its six-digit code within ten minutes. The
pending and enabled secrets are encrypted with the installation encryption key.
The implementation uses [RFC 6238](https://www.rfc-editor.org/rfc/rfc6238) and
[RFC 4226](https://www.rfc-editor.org/rfc/rfc4226): SHA-1, 30 seconds, six digits,
and at most one adjacent time step in either direction. Accepted time steps are
recorded atomically and cannot be replayed. Tests include both RFCs' vectors.

Confirmation displays ten random recovery codes once. Only SHA-256 hashes are
stored; each code is consumed atomically. Enabling or disabling MFA signs out
sessions and revokes personal tokens. Account keeps those recovery codes visible
after the deliberate session revocation and freezes all other Account commands. Choose **I have saved the
recovery codes — sign in again** to clear them and return to login. No automatic
refresh runs between confirmation and acknowledgement. Save the codes before
leaving the page; they cannot be recovered on return.
New enrollment replaces an unconfirmed enrollment; it never silently replaces
an active factor.

MFA login returns `202` with a short-lived, single-use challenge after a valid
password. It creates no authenticated session. Submit the challenge and an
authenticator or recovery code to the same-origin
`POST /api/auth/mfa/challenge`. Challenges expire after five minutes and are
invalid after account, password or factor changes. A rejected factor consumes
the challenge; restart login. IP limits and a durable account-wide limit of ten
factor attempts per ten minutes apply. Enrollment confirmation also has an
account attempt limit. No factors are accepted on anonymous recovery routes.

Login confirms identity for five minutes. Creating/changing accounts, changing
factors and issuing/revoking personal tokens require that recent confirmation.
After it expires, use Account's **Confirm identity** with password and, if MFA is
enabled, a fresh TOTP or unused recovery code. Password changes and session-IP
reveal still verify the current password; MFA accounts also require recent
factor confirmation. `401` with `reauthentication_required: true` identifies this
case. Account and Admin backup restore offer an identity-confirmation dialog
without discarding the pending action. Successful reauthentication does not
repeat that action: choose **Resume action** to explicitly submit it once, or
cancel. A second expiry stops the attempt. Passwords and factor inputs are
cleared after submission/cancellation; non-secret drafts and a selected backup
archive remain available. A cancelled restore clears its passphrase and requires
verification again before another restore.

A normal unauthenticated request returns `401` without that flag. Rejected
passwords and factors can also return `401`; Account checks the cookie identity
before deciding whether to show the validation error or return to login. Account
role/enabled-state changes, sensitive session/token revocations and MFA
disablement use application confirmations describing their effects. Changing
your own account signs you out when the server revokes its sessions.

## Local recovery

Stop the service, retain the installation/config files, and run the existing
locked local command against the installation's configured database:

```sh
./webserver admin reset-password --username ACCOUNT --factors password
./webserver admin reset-password --username ACCOUNT --factors mfa
./webserver admin reset-password --username ACCOUNT --factors password,mfa
```

`--factors password` is the backwards-compatible default of `reset-password`.
Interactive mode explicitly prompts for the account if omitted. MFA-only
recovery reads no new password. Automation must supply `--username` and
`--password-stdin`; password recovery reads exactly two newline-terminated
password lines, while MFA-only recovery reads none. Never put passwords in
arguments. Recovery takes the exclusive installation process lock, changes only
the selected account/factors, revokes its sessions and personal tokens, destroys
its pending challenges and writes an audit in the same transaction. It does not
create accounts, reenable a disabled account, change roles or reopen setup.

## Tokens, restoration and limits

Create a named token in Account after confirming identity. Choose only the
needed personal scopes and an expiration within one year. The secret appears
once; copy it into your automation's secret storage. It cannot be retrieved by
listing tokens. While an unacknowledged token or enrollment secret is displayed,
Account blocks other mutations so another result cannot replace it. Use **I have
saved the secret** to clear a token, or **Cancel and clear enrollment secret** to
abandon an unconfirmed enrollment. Secrets live only in this document and are
cleared when leaving it; browser back navigation cannot restore them. Repeated
clicks or Enter presses admit only one pending Account command.
The database contains only its SHA-256 hash. Token lists show
non-secret IDs, names, scopes, expiration, revocation and last-used time plus a
masked source address. Do not put tokens in URLs or shell history.

Effective authorization is the intersection of selected scopes and the
account's current role. Disabled accounts, expired and revoked tokens fail
authentication. Requests are limited to 120 per minute per source IP.
Revocation is checked against persistence on every request; an operation already
admitted can finish. Audit actors identify `account:<id>/token:<public-id>`;
request audit metadata includes `account_id` and `personal_token_id`.

Backup restore clears all archived browser sessions, challenges and personal
tokens before replacement. Renew personal tokens and sign in again after
restoration. Enabled TOTP secrets are rewrapped with the destination installation
key. Archived recovery codes and pending enrollments are discarded so an older
backup cannot resurrect used codes. Enabled authenticators remain usable after
the current and adjacent TOTP steps expire (up to 90 seconds). Generate fresh
recovery codes by disabling and enrolling MFA again, or use local recovery if
the authenticator was lost. The separate **Metrics
Access Credential** keeps its existing restore/rotation/disablement contract.
Personal tokens are never accepted as metrics credentials, or conversely.

No additional environment variables are required. Keep the existing local
LAN/VPN deployment, secure cookie, same-origin and HTTPS configuration. This
feature does not add SSO, external identity providers, per-server permissions or
public exposure.

## Cookie-free discovery → plan → approval

The versioned [OpenAPI 3.1 contract](openapi-v1.json) lists every personal Bearer
route, required scope, path type, error and supported pagination. Tokens are
created through the human Account page; issuing tokens is deliberately outside
the Bearer API. The example needs `operational:read`, `maintenance:write` and
`approvals:write`. Read-only monitoring needs only `operational:read`.

```sh
export SLU_BASE=http://127.0.0.1:8080
# Supply SLU_TOKEN securely; it is the once-displayed personal token.
curl --fail-with-body -H "Authorization: Bearer $SLU_TOKEN" \
  -X POST "$SLU_BASE/api/update/my-server"

# Poll until status is pending_approval and inspect the exact package plan.
curl --fail-with-body -H "Authorization: Bearer $SLU_TOKEN" \
  "$SLU_BASE/api/servers" > /tmp/slu-plan.json
jq '.[] | select(.name == "my-server") | {status,pending_updates,upgrade_plan}' /tmp/slu-plan.json

# Submit the identity of that reviewed plan, preserving package deferrals.
jq '.[] | select(.name == "my-server") | {
  job_id, approval_generation,
  deferral_revision: (.upgrade_plan.package_deferrals.revision // "")
}' /tmp/slu-plan.json > /tmp/slu-approval.json
curl --fail-with-body -H "Authorization: Bearer $SLU_TOKEN" \
  -H 'Content-Type: application/json' -X POST \
  --data-binary @/tmp/slu-approval.json "$SLU_BASE/api/approve/my-server"
```

Discovery refreshes metadata and simulates upgrades; approval is a separate
decision. A stale job ID, approval generation or deferral revision returns
`409`; fetch and review the new plan. Full/kept-back approvals also require
`confirm_removals: true` when removals are present. Read errors as JSON
`{"error":"..."}`. `401` means missing/invalid/expired/revoked credential; `403`
means a valid identity lacks the role or token scope; `429` means rate limited.
`413` means the body exceeds its [HTTP request budget](configuration.md#http-request-budgets):
JSON commands default to 1 MiB, including chunked requests; inventory imports
retain their specialized limits. Additional JSON root values or trailing
non-whitespace return `400` before mutation. Unknown fields remain compatible
except on routes with an explicitly closed schema.
Audit pagination uses `page` (default 1) and `page_size` (default 50, max 200).
Log pagination uses `after_seq` (default 0) and `limit` (default 100, max 500),
with `next_sequence` and `has_more` in the response. Server discovery returns a
complete array without pagination.

Installed vulnerability scans (`POST /api/servers/{name}/vulnerabilities/scan`)
return `202` with `{"status":"running","scope":"installed_packages"}` once
admitted. Poll `GET /api/servers/{name}/vulnerabilities` for their persisted
result. Host facts refresh and application-check tests run synchronously and
return `502` if the remote operation cannot complete, including an SSH transport
failure.

Controlled reboot (`POST /api/reboot/{name}`) and APT repair
(`POST /api/repair/{name}`) require a JSON body containing `{"confirm":true}`
and `Content-Type: application/json`. Both use `maintenance:write`; omitted or
false confirmation returns `400` before any host operation is admitted.

## Installed vulnerability scan lifecycle

`POST /api/servers/{name}/vulnerabilities/scan` requires `maintenance:write`
and accepts cookie sessions or operational personal Bearer credentials. Its
`202` response includes `scan_id`, `job_id` (the same durable execution identity),
`status: running` and `scope: installed_packages`. Two scans maximum run globally;
one operation maximum runs on each Server. Busy Server, capacity, disabled scan
or application maintenance returns `409`; an unknown Server returns `404`.

`POST /api/servers/{name}/vulnerabilities/scans/{scan_id}/cancel` uses the same
permission and transports. It returns `202` with `status: cancelling` and the
exact `scan_id` while execution drains. The Server reservation, global capacity
and Maintenance Coordination shared lease stay held until SSH, source HTTP and
persistence have returned. Repeating the command during drainage is idempotent.
An old identity, finished scan, or identity for a different Server returns `409`
without stopping any other scan. Missing authentication returns `401`; readers
and tokens without `maintenance:write` receive `403`.

`GET /api/servers/{name}/vulnerabilities` (`operational:read`) exposes
`last_attempt.scan_id`, `progress`, start/finish timestamps and state
(`running`, `complete`, `partial`, `unavailable`, `cancelled`, `interrupted`).
`progress` contains the observed phase (`inventory`, `metadata`, `evaluation`,
`persistence`) and counts for `inventoried`, `metadata`, `evaluated`, `unknown`,
`unsupported` and `findings`. Metadata counts packages with collected provenance,
not packages proven supported. First entry into each phase publishes promptly;
subsequent updates are bounded to one per second. No percentage or ETA is claimed.
Dashboard/SSE includes the same sanitary `scan_id` and `progress` facts. Readers
can inspect these facts; job detail and raw logs still require `logs:read`, and
Status exposes cancellation only to maintenance operators/administrators.

Startup and backup restoration canonically persist orphaned running attempts as
`interrupted`, retaining their durable identity and recording the terminal audit
once. Periodic interruption retry waits between five minutes and one hour (the
configured scan interval clamped to that range), measured from interruption
reconciliation; there is no immediate restart loop. Cancelling an operation does
not schedule an immediate retry. The previous accepted observation, last complete
success and finding ledger survive cancellation/interruption; freshness becomes
stale and incomplete coverage never falsely resolves findings. Scans remain
read-only and never change packages or refresh APT indexes.


## Fix publication and candidate availability

`VulnerabilityFinding.availability` is additive on both installed findings and
pending-update enrichment. It separates exact release/source publisher events
(`published`, including all ordered ranges and fixed versions), candidate source
and binary versions with signed origin, observed repository availability, local
`apt` index/refresh evidence and the four canonical Approval Scope `scopes`.
`simulated` does not authorize execution or prove installation. Deferred packages,
proven holds and removal-confirmation requirements remain subject to existing
maintenance permissions and Package Deferral. Unknown causes remain unknown.

Legacy disposition/API fields remain readable. `no_fix_available` means no
verified candidate fix in observed data, not no publisher fix;
`fixed_by_version` is the evaluated candidate source version. Legacy persisted
records are lazily normalized to unknown additive availability; no database schema
migration or replacement of existing resolution proofs is required.

Dashboard `risk.vulnerability` adds `published_fix_occurrences` and
`candidate_addressed_occurrences` (omitted when zero); existing `resolved_occurrences` still requires
installed-package evidence. Finding `availability_freshness` tracks package and
query observations while `published_fix_freshness` tracks raw advisory downloads.
Neither asserts fresh APT indexes. Complete older cached candidate absence cannot
establish a fix against newer installed positive evidence. Source-detail cache
objects remain host-independent and preserve actual download timestamps.

Server status can also expose optional `apt_metadata` for a maintenance refresh
observed by this process. Errors are sanitary reasons; authorized job logs retain
the command output. These observations do not establish that no later external
refresh happened and can be unknown after restart. Finding `availability.apt`
preserves the host metadata context recorded with that observation.
