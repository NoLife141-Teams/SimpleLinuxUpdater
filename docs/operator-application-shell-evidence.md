# Operator Application Shell evidence

This record compares the same disposable authenticated application before and after issues #258–#262. Desktop captures use a 1920 × 1080 viewport; mobile captures use a 390 × 844 viewport.

## Complete operator pages

| Page | Desktop before | Desktop after | Mobile before | Mobile after |
| --- | --- | --- | --- | --- |
| Status | [Before](../.github/assets/operator-shell-20260713/status-before-desktop-1080p.png) | [After](../.github/assets/operator-shell-20260713/status-after-desktop-1080p.png) | [Before](../.github/assets/operator-shell-20260713/status-before-mobile.png) | [After](../.github/assets/operator-shell-20260713/status-after-mobile.png) |
| Manage Servers | [Before](../.github/assets/operator-shell-20260713/manage-before-desktop-1080p.png) | [After](../.github/assets/operator-shell-20260713/manage-after-desktop-1080p.png) | [Before](../.github/assets/operator-shell-20260713/manage-before-mobile.png) | [After](../.github/assets/operator-shell-20260713/manage-after-mobile.png) |
| Observability | [Before](../.github/assets/operator-shell-20260713/observability-before-desktop-1080p.png) | [After](../.github/assets/operator-shell-20260713/observability-after-desktop-1080p.png) | [Before](../.github/assets/operator-shell-20260713/observability-before-mobile.png) | [After](../.github/assets/operator-shell-20260713/observability-after-mobile.png) |
| Admin | [Before](../.github/assets/operator-shell-20260713/admin-before-desktop-1080p.png) | [After](../.github/assets/operator-shell-20260713/admin-after-desktop-1080p.png) | [Before](../.github/assets/operator-shell-20260713/admin-before-mobile.png) | [After](../.github/assets/operator-shell-20260713/admin-after-mobile.png) |

## Review notes

- The same routes, navigation labels, destinations, page labels, and page-local content are present in each pair.
- The Observability shell is the visual baseline. Status now uses the same shared brand, navigation, active state, responsive layout, and focus treatment.
- At mobile width, every page uses the same compact stacked shell; Status no longer places its brand and navigation in competing columns.
- Navigation remains server-rendered and usable without client-side initialization.
- Every page exposes exactly one shell and one `aria-current="page"` navigation item.
- Browser verification exercises the shared hover elevation and shadow treatment plus the shared keyboard-focus treatment on every page at desktop and mobile widths.
