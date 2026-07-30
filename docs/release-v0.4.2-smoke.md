# v0.4.2 Release Smoke Result

This result records the pre-tag validation performed for `v0.4.2`. Discord
webhook URLs, Telegram bot tokens, Telegram chat IDs, and notification message
contents containing operator data are intentionally omitted.

## Scope

- Date: 2026-07-30 America/Toronto
- Release implementation commit:
  `472661bb7a8fa913831289bec45a3a2ae7e2228e`
- Release preparation pull request: #346
- Release delta: native Discord and Telegram notification destinations

## Targeted Notification Smoke

The notification smoke used the project's deterministic request-capture
transports and isolated application fixture. It did not contact real Discord or
Telegram accounts.

- Discord and Telegram settings were accepted independently and encrypted at
  rest.
- Settings reads exposed configured/enabled state without returning stored
  credentials.
- Fan-out produced one destination-specific request per enabled integration.
- Discord embeds disabled mentions and Telegram requests used the expected
  structured message format.
- Destination diagnostics recorded independent outcomes.
- A slow or failed legacy webhook did not prevent Discord and Telegram
  delivery.
- Each native destination retained an independent timeout budget.
- Unofficial Discord hosts and malformed Telegram tokens were rejected.
- Notification settings, diagnostics, and test-delivery routes rejected
  unauthenticated requests.
- Unsafe replacement input and remote failure diagnostics did not echo
  credentials or untrusted remote response details.

The following focused commands passed on the release implementation commit:

- `go test -count=1 -v ./internal/notifications -run
  'TestNativeIntegration|TestNativeIntegrations'`
- `go test -count=1 -v . -run
  'TestNativeNotificationSettingsAPIProtectsSecrets|TestNotificationDeliveryDiagnosticsRedactRemoteFailureDetails|TestNotificationSettingsAPIRejectsUnsafeReplacementWithoutEchoingSecrets|TestNotificationRoutesRequireAuthentication'`

## Automated and Docker Gates

- PR #346 completed its required CI gate and all applicable CodeQL analyses.
- The post-merge `main` CI run completed frontend unit, npm audit, Go race,
  coverage, quality, Playwright, and required-gate jobs successfully on the
  release implementation commit.
- The post-merge CodeQL run completed successfully on the same commit.
- The full local and Docker candidate gates recorded in
  [v0.4.2 Release Readiness](release-v0.4.2-readiness.md) passed.
- The Docker candidate ran as non-root UID/GID `100:101`, initialized a fresh
  volume, returned HTTP `200`, and preserved state across restart.

## Intentionally Not Sent

No message was sent to a real Discord server or Telegram chat. Doing so would
require external credentials and create a third-party side effect. The
destination request construction, validation, fan-out, timeout isolation, and
outcome handling were instead exercised through controlled transports without
retaining secrets.
