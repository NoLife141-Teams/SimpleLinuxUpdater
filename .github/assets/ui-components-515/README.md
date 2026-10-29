# Shared UI components (#515)

Chromium captures from a disposable Go E2E installation. Inventory, package rules,
post-check profiles and host-key facts are synthetic; no real SSH target is contacted.

```sh
SLU_CAPTURE_UI_515=1 npm run test:e2e -- tests/e2e/ui-components.spec.js --workers=2 --retries=0
```

- `manage-deferrals-{1284,768,700,390,320}.png` and the corresponding `admin-` files
  compare the same Package Deferral component. Labels and inputs share grid rows;
  narrow screens retain naturally sized labels and separate stacked fields.
- `typed-{status,manage,account}-{1284,390}.png` use identical confirmation content.
  Width, field/action gap, input dimensions and button weights are compared across
  pages. Tests also cover typed gating, pointer confirmation, Tab/Shift+Tab, Escape,
  cancellation and focus return.
- `inventory-file-390.png` shows the shared file trigger and one filename/state.
  Tests exercise keyboard focus, CSV/JSON selection, replacement, preview and reset.

The negative control restored the previous styles/Manage markup temporarily and
produced the expected failures for Manage alignment, confirmation footer spacing
and the old file control. Original working files were restored automatically.
