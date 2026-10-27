# Policy schedules #472 fixtures

These screenshots show the production Admin template with Playwright API
fixtures, never a real inventory or SSH host. The fixture clock facts are
fixed API data: a monthly day-31 occurrence in March 2028,
February 2028 skipped, and a one-time fallback occurrence blocked by a missing
explicit canary. The browser consumes the projected dates/UTC/DST facts.

- `desktop-monthly-preview.png`: monthly UTC projection, canary/wave labels and skipped February.
- `desktop-monthly-schedule.png`: monthly editor, numeric day and last-day control.
- `desktop-explicit-canary-controls.png`: explicit names, wave settings and eligibility rules.
- `desktop-once-blocked-preview.png`: unavailable canary and canonical earlier fallback occurrence.
- `mobile-monthly-schedule.png`: 390 px monthly layout.
- `mobile-once-schedule.png`: 390 px one-time editor and consumption rule.

Regenerate from the `policy schedules 472` scenarios in
`tests/e2e/admin.spec.js` with `CAPTURE_POLICY_SCHEDULES_472=1`.
The capture helper places each panel below the sticky Admin chrome and uses a
1800 px viewport height to include the whole panel. Production CSS is unchanged.
Use a disposable localhost app/database as described in the issue validation
record; the test fixtures intercept inventory/policy APIs.
