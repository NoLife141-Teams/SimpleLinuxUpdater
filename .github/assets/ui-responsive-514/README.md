# Responsive UI regression evidence (#514)

Captured with Chromium against the disposable Go E2E installation and deterministic
inventory fixtures. No production hosts or credentials are involved.

Reproduce with:

```sh
SLU_CAPTURE_UI_514=1 npm run test:e2e -- tests/e2e/ui-responsive.spec.js --workers=2 --retries=0
```

- `manage-{390,700,768,900,1284}.png` and `admin-{390,700,768,900,1284}.png`:
  section navigation after a deep link, scrolling and mobile → tablet → desktop
  → tablet → mobile transitions. The sticky offset follows the measured header,
  including the static-header breakpoint. Capture waits for the real wheel scroll
  to settle so the fixed header and nav are painted at their final positions.
- `admin-loading-{320,390}.png` and `admin-unavailable-{320,390}.png`:
  delayed/failed Installation Health reads; wrapped controls fit without root overflow.
- `observability-{320,390}.png`: metadata reflow with a long host option; tables
  retain their own horizontal scroll container.
- `status-filters-1284.png`: whole human words and visible filter counters. Long
  technical tags retain wrapping.

The E2E assertions check actual rectangles, document width, focus, and word ranges.
They do not depend on a fixed header height or screenshot pixel equality.
