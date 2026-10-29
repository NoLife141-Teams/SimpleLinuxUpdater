# Account UI consistency (#516)

Chromium captures from disposable E2E installations. Visible accounts, token rows,
sessions and dates are synthetic. No screenshot, trace or video is recorded while
an actual one-time token or MFA recovery secret is present.

```sh
SLU_CAPTURE_UI_516=1 npm run test:e2e -- tests/e2e/account-ui.spec.js --workers=2 --retries=0
```

- `password-error-{1284,390}.png`: mismatched passwords are rejected before a PUT,
  fields are cleared and the local error receives focus with a link back to the field.
- `tokens-loading-390.png` / `tokens-error-390.png`: independent loading and failure
  states with Retry; other lists and non-secret form drafts remain usable.
- `empty-lists-1284.png`: valid empty results are distinct from loading/failure.
- `reauthentication-1284.png`: the identity dialog uses the shared primary-action
  convention; inputs are empty in the capture.
- `dates-1284.png`: application-zone dates around both Toronto DST transitions,
  with a different browser timezone. Raw UTC instants remain in semantic `datetime`
  attributes and diagnostic titles; browser-local expiry entry is unchanged.

Functional tests also verify stale-row retention, protected-secret retry admission,
late timezone completion without additional list reads, confirmations and focus.
An actual delayed timezone 401 after MFA activation must also preserve the frozen
recovery-code view until the user explicitly acknowledges the codes and signs in.
A negative control against the previous files reproduces the missing contextual
errors/loading states and date presentation. Original files are restored afterward.

MFA validation is also checked at 320×568 and 390×568: the local feedback and
protected code input remain visible together after rejection, while focus returns
to the code field. These checks record geometry only and do not capture secrets.
