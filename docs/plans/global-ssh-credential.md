# Global SSH Credential implementation plan

## Outcome

Deepen Global SSH Credential into one app-scoped module that owns effective-key resolution, encrypted persistence, cache coherence, guarded mutation, and restored-database handling. Preserve existing routes, response shapes, audit vocabulary, Server credential precedence, active-action blocking, and backup rollback behavior.

## Domain decisions

- A Server's per-server SSH key always takes precedence.
- Global SSH Credential is considered only when the Server has no per-server key.
- SQLite is the source of truth; an app-scoped cache is a last-known-good optimization.
- The credential is never returned through JSON or included in logs, audit metadata, or errors.
- Replace and clear remain blocked while any Server Action is active because reconnects may need the credential.
- Clear is idempotent.
- New operator replacements reject unparseable and passphrase-encrypted private keys.
- Historical unusable credentials remain stored and do not prevent startup; they fail only when authentication needs them.
- Backup restore re-encrypts the restored credential with the current installation key, invalidates the live cache after the database swap, and restores the previous credential on rollback.
- No database migration or ADR is required.

## Target architecture

### Deep module

Place the Global SSH Credential module under `internal/servers`, alongside Server secret persistence and authentication fallback. Runtime Composition receives one module instance instead of four credential function fields.

The module interface presents three cohesive capabilities:

1. Resolve the effective SSH key from a per-server candidate and the app-wide fallback, returning structured source and degraded-state facts without exposing the key beyond the authentication adapter.
2. Report non-secret configured state while distinguishing persistence failures from absence.
3. Execute guarded replace and clear commands with transport-neutral outcomes, active Server names when blocked, and audit facts for the route adapter.

The implementation hides:

- the `global_ssh_key` storage identifier;
- encryption and decryption calls;
- app-scoped cache state and locking;
- retry behavior for transient SQLite locks;
- restored-database validation and re-encryption;
- cache invalidation and reload rules.

### Adapters

- The production SQLite adapter binds the module to the live application database and current encryption key.
- The same SQLite adapter can bind to a detached restored database with an explicit encryption key.
- An in-memory adapter provides deterministic module tests.
- Route adapters retain multipart parsing, upload limits, HTTP/JSON mapping, and audit emission.
- Host Maintenance Session retains SSH signer construction and connection behavior.
- Backup Operation Lifecycle retains archive extraction, file replacement, maintenance mode, session invalidation, and rollback orchestration.

## Delivery slices

### Slice 1: Centralize the live credential lifecycle

Introduce the app-scoped module and migrate the complete live path: configured-state reads, replace, idempotent clear, effective credential resolution, Host Maintenance Session authentication, and Runtime Composition.

Preserve current operator behavior during this slice. The existing setting remains byte-compatible, and parsing remains at authentication time until Slice 3.

Remove the process-global credential cache, duplicate legacy getter/setter implementation, and the four `AppDeps` credential function fields once all live callers use the module.

Verification:

- module interface tests for absence, encrypted round-trip, per-server precedence, idempotent clear, transient read failure, stale-cache resolution, and concurrent access;
- command tests for active Server Action blocking and audit facts;
- route contract tests for unchanged status codes, JSON, and audit vocabulary;
- Host Maintenance Session tests for per-server and global resolution;
- an architecture guard preventing restoration of legacy globals and four-function dependency wiring.

### Slice 2: Move restored credential handling behind the module

Use the module's SQLite adapter against detached restored databases for credential presence, decryption validation, and re-encryption. Backup code must stop knowing the setting identifier or encrypted representation.

After a successful database replacement, invalidate and reload the live module. If replacement or runtime reload fails, rollback must restore the previous database and credential cache together.

Verification:

- restore with no Global SSH Credential;
- restore with a credential encrypted by a different backup key;
- verification failure for undecryptable stored data before file replacement;
- successful cache handoff after restore;
- rollback restores the previous credential;
- backup response and audit metadata remain compatible;
- an architecture guard ensures raw Global SSH Credential SQL exists only in its persistence adapter.

### Slice 3: Reject unusable operator replacements

Validate new replacements before persistence with the SSH parser used by production authentication. Accept supported unencrypted OpenSSH/PEM private keys; reject malformed and passphrase-encrypted keys because the application has no runtime passphrase flow.

Do not delete or block startup for historical unusable credentials. Keep the existing `has_key` compatibility response based on persisted presence, preserve per-server-key precedence, and return a clear validation failure only for new replacement commands.

Verification:

- supported private-key formats are accepted;
- malformed, public-only, and passphrase-encrypted uploads are rejected before persistence;
- validation errors never include credential contents;
- a valid per-server key still works when a historical Global SSH Credential is unusable;
- HTTP and audit results distinguish invalid input from persistence failure;
- Manage Page Interaction continues to refresh configured state after successful commands.

## Compatibility constraints

- Keep `/api/keys/global` methods and response keys unchanged.
- Keep active Server Action conflict behavior and `active_servers` facts unchanged.
- Keep the `global_ssh_key` SQLite row byte-compatible.
- Keep backup archives and restored databases compatible with existing releases.
- Keep per-server password and SSH key behavior unchanged.
- Do not expose new secret-bearing fields to Dashboard Projection or Manage Page Interaction.

## Validation gate

Each slice must pass its targeted module, route, Host Maintenance Session, or backup tests plus:

```bash
go build ./...
go test ./...
go test -race -count=1 ./...
go vet ./...
npm run test:unit
npm run test:e2e
```

The implementation is complete when all live and restored Global SSH Credential behavior crosses the new module interface, no process-global credential state or duplicate implementation remains, compatibility contracts pass, and the architecture guards prevent the old shape from returning.
