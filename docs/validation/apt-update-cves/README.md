# CVEs scoped to available APT updates

These screenshots use the disposable browser composition and synthetic package/advisory data. They do not represent a production security assessment.

- [Compact Status summary, desktop](status-desktop.png)
- [Package-level CVE details, mobile](package-details-mobile.png)

The browser scenarios in `tests/e2e/update-cve-scope.spec.js` cover available-update inspection outside approval, candidate-version CVE details, disclosure persistence during refresh, absence of standalone scanner requests, and approval of security updates when CVE lookup is unavailable. The Status suite also exercises existing approvals, deferrals, drawer scrolling, SSE refresh and responsive navigation. The installation-health scenario deliberately sets the retired scan-enabled environment variable to true and still observes a disabled scanner worker.

Go regressions cover retired authenticated endpoints (`410` without job admission), legacy observation retention without contribution to Status, candidate evidence freshness/identity/provenance, distinct CVE aggregation, partial corrections, cancelled update enrichment without a persistent vulnerability watch, and inspection of successful scheduled APT discovery without approval.

Local validation uses Go 1.26.10. The full local Go/race suites exclude `TestDeferralHookRequiresCompleteApprovedTransaction`: the macOS system awk lacks `systime`. The unfiltered test remains required on Linux CI. No production scan, maintenance or deployment is part of this validation.
