# Human access refusal (#518)

Chromium captures of synthetic reader/operator accounts on disposable installations:
`{reader,operator}-{admin,manage}-{320,390,1284}.png`. Captures are taken only on
the refusal page, with no login fields, tokens, MFA codes or host data present.

```sh
SLU_CAPTURE_UI_518=1 npm run test:e2e -- tests/e2e/access-denied.spec.js tests/e2e/identity.spec.js tests/e2e/auth.spec.js --workers=2 --retries=0
```

The new six cases cover each role/width visiting both forbidden pages. They assert
403 + HTML, a useful document title and heading, the real role in the shell, no
Admin/Manage links, no API request initiated by the refusal page, strict CSP without
browser violations, no document overflow, keyboard return to Status/Account and
functional logout. Existing identity/MFA/token and login/setup scenarios also run.
The six new tests fail on the prior authorization middleware (JSON instead of HTML),
then pass with the final source restored. Browser engine coverage is Chromium.

## Boundary and HTTP evidence

`authorizeRequest` makes the same route/role/scope decision as before. Only after
an authenticated cookie is refused on a declared GET `/admin` or `/manage` does it
consider the page presentation. The principal is passed directly, since the allowed
context principal has not yet been installed. Rendering additionally requires a
role permitted to read Status, so an empty/invalid role cannot trigger the shell's
legacy absent-role fallback. The protected handler and its data reads never run.

Explicit acceptable `text/html` is required. Missing Accept, wildcard-only and JSON
requests retain JSON403; qualities, specific overrides, q=0 and JSON tie preference
are covered. Both page representations are no-store and vary on Accept while
preserving the session middleware's separate Vary: Cookie value.

The existing exhaustive route permission matrix now repeats every denied cookie
or Bearer case with absent, JSON, HTML and wildcard Accept. It keeps the production
auth/session middleware and real credentials, using a 204 admission probe to avoid
executing maintenance commands. All technical403 responses retain the exact
`{"error":"permission denied"}` body; incompatible Bearer routes remain401JSON.

Additional full-router checks cover three roles, allowed admin pages, lower-role
refusals and filtered navigation, scope-limited admin tokens, strict security
headers, unknown GET routes, undeclared POST `/admin`, and anonymous login redirects
and API401 responses. The new template is included in strict CSP inventory. No
route permission, SSH, apt, credential or backup behavior is changed.
