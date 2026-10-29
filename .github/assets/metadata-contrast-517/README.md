# Informative metadata contrast (#517)

These Chromium screenshots and raw JSON measurements use synthetic facts in
disposable installations, at 390, 768 and 1284 CSS pixels. No real hosts or secrets.

```sh
SLU_CAPTURE_UI_517=1 npm run test:e2e -- tests/e2e/metadata-contrast.spec.js --workers=2 --retries=0
```

`manage-*`, `admin-*`, `observability-*` and `confirmation-{status,manage,account}-*`
include the rendered captures and measurements. Navigation results cover selected,
neutral, hover and actual keyboard focus; titles are also checked in neutral state.
Confirmation results cover every context label and value on all three pages.

The foreground is the computed opaque CSS text color. For the background, the
regression temporarily makes only the measured text transparent, preserving layout,
then samples the composed screenshot below all visible line pixels. It takes the
lowest unrounded ratio, accounting for gradients/translucent surfaces and excluding
the clipped portion of ellipsized text. Original inline styles are restored in a
`finally` block. The sRGB calculation follows [relative luminance](https://www.w3.org/WAI/WCAG22/Understanding/relative-luminance.html)
and the [4.5 minimum for ordinary text](https://www.w3.org/WAI/WCAG22/Understanding/contrast-minimum.html).
Black/white calibration is 21:1. Screenshot glyph antialiasing is never used as text color.

`comparison.json` records the same scenarios before and after the CSS change:

| Example at 1284px | Before foreground / background | Before | After foreground / background | After |
| --- | --- | --- | --- | --- |
| Manage selected subtitle | 111,111,111 / 40,53,68 | 2.483 | 166,166,166 / 40,53,68 | 5.126 |
| Admin selected subtitle | 111,111,111 / 39,52,67 | 2.520 | 166,166,166 / 39,52,67 | 5.201 |
| Observability run summary | 111,111,111 / 42,42,44 | 2.851 | 166,166,166 / 42,42,44 | 5.884 |
| Confirmation label | 111,111,111 / 37,37,38 | 3.048 | 166,166,166 / 37,37,38 | 6.291 |
| Confirmation value | 133,133,133 / 37,37,38 | 4.150 | 166,166,166 / 37,37,38 | 6.291 |

Ratios above are rounded for display; assertions use full precision. All measured
states exceed 4.5. The original six tests fail on the preceding CSS. A separate
measurement-only baseline run collected the remaining before values; the regular
assertions and corrected CSS were restored immediately afterward.

## Inventory and scope

The existing `--muted` replaces `--faint` for informative metric details (including
Status's local override), Manage endpoint/authentication details, Admin lifecycle
and refresh metadata, draft state, timezone context, session labels and keyboard
hints, and Status host facts, small labels, rail labels, server sublines and idle/
pending phase labels. Section navigation titles and area badges, and confirmation
values, also use `--muted` because their prior `--subtle` can miss the threshold.

Token names/values, fonts, sizes, dimensions and selected-state borders/backgrounds
are unchanged. The remaining `--faint` uses are disabled fleet actions and decorative
sort arrows (sorting remains expressed by `aria-sort`); their treatment is retained.
Maintenance defines the token but has no consumer. Login/Setup/Account use the same
base stylesheet with refreshed asset URLs. This is a targeted metadata audit, not
an app-wide WCAG certification. Browser engine coverage is Chromium.
