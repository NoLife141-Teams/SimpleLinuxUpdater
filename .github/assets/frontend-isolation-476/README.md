# Frontend isolation #476 presentation fixtures

Only inspected synthetic views are included. `before/` uses an exact archive of
main `1de9e99173a341719fc28cf764557d4f6f1cc50e`; `after/` uses the extracted adapters.
Both use the same disposable real-router composition, accepted API fixtures,
UTC/en-US browser context and fixed clock (`2026-05-28T12:00:00Z`).

Status, Admin and Manage are captured at 1440×1000 and 390×844, with full-page
screenshots. Each before/after pair has the same SHA-256. No real host, inventory,
password, token, MFA secret, session cookie or installed configuration is shown.

See [the validation record](../../../docs/validation/frontend-isolation-476/README.md)
for exact source provenance, hashes, lifecycle/focus evidence and reproduction.
The capture entry point is `tools/frontend/capture.cjs`.
