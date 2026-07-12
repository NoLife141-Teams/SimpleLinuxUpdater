# UI quality pass evidence

This record compares the same disposable `variant-c` demo data before and after the code and UI quality pass. Desktop captures use a 1440 x 1000 viewport; narrow captures use a 390 x 844 viewport. The screenshots are implementation evidence for issues #235, #238, #239, #240, and #241.

## Status overview

| View | Before | After |
| --- | --- | --- |
| Desktop overview, including urgent, active, approval, and routine states | [Before](../.github/assets/ui-quality-20260711/status-overview-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/status-overview-after-desktop.png) |
| Narrow overview | [Before](../.github/assets/ui-quality-20260711/status-overview-before-mobile.png) | [After](../.github/assets/ui-quality-20260711/status-overview-after-mobile.png) |
| Approval-focused fleet | [Before](../.github/assets/ui-quality-20260711/status-approval-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/status-approval-after-desktop.png) |
| Active-maintenance fleet | [Before](../.github/assets/ui-quality-20260711/status-active-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/status-active-after-desktop.png) |

The overview data contains a failed host, a reboot-required host, an active update, two pending approvals, stale host facts, security exposure, and a selected-host context panel. This keeps the urgent, active, approval, and normal fleet states comparable without manufacturing product behavior.

## Operator pages and dialogs

| View | Before | After |
| --- | --- | --- |
| Manage Servers | [Before](../.github/assets/ui-quality-20260711/manage-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/manage-after-desktop.png) |
| Observability | [Before](../.github/assets/ui-quality-20260711/observability-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/observability-after-desktop.png) |
| Admin | [Before](../.github/assets/ui-quality-20260711/admin-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/admin-after-desktop.png) |
| Destructive confirmation | [Before](../.github/assets/ui-quality-20260711/dialog-before-desktop.png) | [After](../.github/assets/ui-quality-20260711/dialog-after-desktop.png) |

## Verification notes

- The same demo database and authenticated browser session were reused across each before/after pair.
- Browser checks covered 1440 x 1000, 980 x 900, and 390 x 844 viewports.
- Status, Manage Servers, Observability, and Admin produced no browser console errors or warnings during the after pass.
- All visible Status buttons had an accessible name.
- Dialogs exposed `role="dialog"`, `aria-modal="true"`, an accessible title, contained keyboard focus, supported Escape, and restored focus to the initiating control.
- Tables retain their own horizontal scrolling at constrained widths rather than clipping page content.
