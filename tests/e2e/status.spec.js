const {
    test,
    expect,
    username,
    password,
    changedPassword,
    ensureAuthenticatedSession,
    signIn,
} = require("./support/fixtures");
const {
    fulfillJson,
    backupRecoveryHealthFixture,
    dismissTypedConfirm,
    acceptTypedConfirm,
    makeTimeline,
    makeDashboardSummary,
    makeHealthTrends,
    stubDashboardApi,
    makeServer,
    makePendingUpdates,
    stubAdminApi,
    stubManageApi,
    capturePolicy472Panel,
    prepareImportConfigureResult,
    captureDeferral473,
} = require("./support/builders");
const fs = require("node:fs");
const path = require("node:path");

test("scheduled wave shows its release time and predecessor caveat", async ({
    baseURL,
    page,
}) => {
    const caveat = "Wave waiting for preceding batch success";
    const servers = [
        makeServer("wave-host", "updated", [], {
            next_run: {
                state: "scheduled",
                policy_name: "Security waves",
                status: "scheduled",
                scheduled_for_utc: "2026-09-09T03:10:00Z",
                scheduled_for_display: "Sep 9, 2026 03:10",
                reason: "rollout_waiting",
                summary: caveat,
            },
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    const row = page.locator('#servers-table tbody tr[data-name="wave-host"]');
    await expect(row).toBeVisible();
    await page.locator("#status-supporting-details > summary").click();
    await expect(page.locator("#selected-host-panel")).toContainText(
        "Sep 9, 2026 03:10",
    );
    await expect(page.locator("#scheduled-runs")).toContainText(caveat);
    await expect(page.locator("#selected-host-panel")).toContainText(caveat);
    await expect(page.locator("#selected-host-panel")).toContainText(
        "Security waves",
    );
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.locator("#selected-host-panel")).toContainText(caveat);
    expect(
        await page.evaluate(
            () =>
                document.body.scrollWidth <=
                document.documentElement.clientWidth,
        ),
    ).toBe(true);
});

test("pending updates drawer keeps scroll position after server refresh", async ({
    baseURL,
    page,
}) => {
    let servers = [
        makeServer("demo-host", "pending_approval", makePendingUpdates(80), {
            tags: ["prod"],
            has_key: true,
        }),
        makeServer("runner-host", "updating", [], {
            tags: ["web"],
            has_password: true,
            next_run: {
                state: "scheduled",
                policy_name: "Nightly security",
                scheduled_for_utc: "2026-05-29T06:00:00Z",
                scheduled_for_display: "May 29, 2026 06:00",
                status: "scheduled",
            },
        }),
    ];
    const state = {};
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/approve/demo-host", (route) => {
        state.approveAll = (state.approveAll || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/approve-security/demo-host", (route) => {
        state.approveSecurity = (state.approveSecurity || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/cancel/demo-host", (route) => {
        state.cancel = (state.cancel || 0) + 1;
        return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    await expect(page.locator(".fleet-rail h2")).toHaveText("Fleet filters");
    await expect(page.locator("#maintenance-timeline-title")).toBeVisible();
    await expect(
        page.locator("#status-supporting-details"),
    ).not.toHaveAttribute("open", "");
    await page.locator("#status-supporting-details > summary").click();
    await expect(page.locator("#approval-triage-title")).toBeVisible();
    await expect(page.locator("#selected-host-title")).toBeVisible();
    await expect(
        page.getByRole("heading", { name: "Scheduled runs" }),
    ).toBeVisible();
    await expect(
        page.getByRole("heading", { name: "Running operations" }),
    ).toHaveCount(0);
    await expect(
        page.getByRole("heading", { name: "Audit trail" }),
    ).toBeVisible();
    await expect(
        page.locator('#servers-table tbody tr[data-name="demo-host"]'),
    ).toBeVisible();
    await expect(page.locator("#selected-host-title")).toHaveText("demo-host");
    await expect(
        page
            .locator("#selected-host-panel")
            .getByRole("button", { name: "Update" }),
    ).toHaveCount(0);
    await expect(page.locator("#approval-triage-table tbody")).toContainText(
        "demo-host",
    );
    await expect(page.locator("#scheduled-runs")).toContainText(
        "Nightly security",
    );

    await page.goto("/observability");
    await expect(
        page.getByRole("heading", { name: "Trend summary" }),
    ).toBeVisible();
    await expect(page.locator("#health-trends-body")).toContainText(
        "demo-host",
    );
    await expect(page.locator("#health-trends-body")).toContainText(
        "runner-host",
    );
    await expect(page.locator("#trend-hosts")).toHaveText("2");
    await page.locator("#health-trend-server").selectOption("demo-host");
    await expect(page.locator("#health-trends-body")).toContainText(
        "demo-host",
    );

    await page.goto("/");
    await expect(
        page.locator('#servers-table tbody tr[data-name="demo-host"]'),
    ).toBeVisible();
    await page.locator("#status-supporting-details > summary").click();

    await page.locator("#select-all").check();
    await page.locator("#bulk-approve-security").click();
    await expect.poll(() => state.approveSecurity || 0).toBe(1);
    await expect(page.locator("#bulk-review-modal")).toHaveCount(0);
    await expect(page.locator("#typed-confirm-modal")).not.toBeVisible();

    await page
        .locator(
            '#approval-triage-table button[data-action="approve-all"][data-name="demo-host"]',
        )
        .click();
    await page
        .locator(
            '#approval-triage-table button[data-action="approve-security"][data-name="demo-host"]',
        )
        .click();
    await page
        .locator(
            '#approval-triage-table button[data-action="cancel-upgrade"][data-name="demo-host"]',
        )
        .click();
    await expect.poll(() => state.approveAll || 0).toBe(1);
    await expect.poll(() => state.approveSecurity || 0).toBe(2);
    await expect.poll(() => state.cancel || 0).toBe(1);

    await page.setViewportSize({ width: 1697, height: 825 });
    await page
        .locator(
            '#servers-table tbody button[data-action="open-drawer"][data-tab="pending"]',
        )
        .click();
    const pendingPanel = page.locator("#drawer-panel-pending");
    await expect(pendingPanel).toHaveClass(/active/);
    await expect(pendingPanel.locator("tbody tr")).toHaveCount(80);
    await expect
        .poll(() =>
            pendingPanel.evaluate((el) => el.scrollHeight - el.clientHeight),
        )
        .toBeGreaterThan(0);
    const pendingTableLayout = await pendingPanel.evaluate((panel) => {
        const approvalActions = document.querySelector(
            "#status-drawer-approval-actions",
        );
        const heading = panel.querySelector(".pending-updates > h4");
        const summary = panel.querySelector(".pending-summary");
        const wrap = panel.querySelector(".pending-updates > .table-wrap");
        const riskHeader = wrap
            .querySelector("th:last-child")
            .getBoundingClientRect();
        const approvalRect = approvalActions.getBoundingClientRect();
        const headingRect = heading.getBoundingClientRect();
        const summaryRect = summary.getBoundingClientRect();
        const wrapRect = wrap.getBoundingClientRect();
        return {
            approvalToHeadingGap: headingRect.top - approvalRect.bottom,
            headingToSummaryGap: summaryRect.top - headingRect.bottom,
            gapAbove: wrapRect.top - summaryRect.bottom,
            clientWidth: wrap.clientWidth,
            scrollWidth: wrap.scrollWidth,
            riskRight: riskHeader.right,
            visibleRight: wrapRect.right,
        };
    });
    expect(
        Math.abs(
            pendingTableLayout.approvalToHeadingGap -
                pendingTableLayout.headingToSummaryGap,
        ),
        "approval controls, pending heading, and summary must use the same vertical spacing",
    ).toBeLessThanOrEqual(1);
    expect(
        pendingTableLayout.gapAbove,
        "pending table must be separated from its summary",
    ).toBeGreaterThanOrEqual(9);
    expect(
        pendingTableLayout.scrollWidth,
        "pending updates must fit without horizontal scrolling",
    ).toBeLessThanOrEqual(pendingTableLayout.clientWidth + 1);
    expect(
        pendingTableLayout.riskRight,
        "the Risk column must remain visible inside the drawer",
    ).toBeLessThanOrEqual(pendingTableLayout.visibleRight + 1);

    await pendingPanel.evaluate((el) => {
        el.scrollTop = 520;
    });
    const beforeRefresh = await pendingPanel.evaluate((el) => el.scrollTop);
    expect(beforeRefresh).toBeGreaterThan(0);

    servers = [
        makeServer(
            "demo-host",
            "pending_approval",
            makePendingUpdates(80).map((update) => ({
                ...update,
                cve_state: "ready",
            })),
            { tags: ["prod"] },
        ),
        servers[1],
    ];
    await page.evaluate(() => window.fetchServers());

    await expect
        .poll(() => pendingPanel.evaluate((el) => el.scrollTop))
        .toBeGreaterThanOrEqual(beforeRefresh - 1);

    await page.setViewportSize({ width: 390, height: 844 });
    await expect
        .poll(() =>
            pendingPanel.locator(".table-wrap").evaluate((element) => {
                const cells = Array.from(
                    element.querySelectorAll("tbody tr:first-child td"),
                );
                return {
                    fitsWithoutHorizontalScrolling:
                        element.scrollWidth <= element.clientWidth + 1,
                    labels: cells.map((cell) =>
                        getComputedStyle(cell, "::before").content.replaceAll(
                            '"',
                            "",
                        ),
                    ),
                };
            }),
        )
        .toEqual({
            fitsWithoutHorizontalScrolling: true,
            labels: ["Package", "Version", "Risk"],
        });
});

test("stale facts recommendation renders its refresh action in the table and inspector", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("stale-host", "done", [], { facts_state: "stale" }),
    ];
    const state = { refreshFacts: 0 };
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/servers/stale-host/facts/refresh", (route) => {
        state.refreshFacts += 1;
        return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    const row = page.locator('#servers-table tbody tr[data-name="stale-host"]');
    const tableRefresh = row.getByRole("button", {
        name: "Refresh host facts",
    });
    const inspector = page.locator("#selected-host-panel");
    const inspectorRefresh = inspector
        .locator(".recommended-action")
        .getByRole("button", { name: "Refresh host facts" });
    await expect(row.locator(".recommended-row-action")).toHaveText(
        "Recommended: Refresh host facts",
    );
    await expect(tableRefresh).toBeEnabled();
    await expect(row.getByRole("button", { name: "Update" })).toHaveCount(0);
    await expect(inspector.locator(".recommended-action")).toContainText(
        "Refresh host facts",
    );
    await expect(inspectorRefresh).toBeEnabled();

    await inspectorRefresh.click();
    await expect.poll(() => state.refreshFacts).toBe(1);
});

test("status availability defaults to enabled and remembers disabled or all server views", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("enabled-host", "done"),
        makeServer("disabled-host", "error", [], {
            disabled: true,
            tags: ["paused-only"],
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    const availability = page.getByRole("combobox", {
        name: "Server availability",
    });
    const rows = page.locator("#servers-table tbody tr[data-name]");
    await expect(availability).toHaveValue("enabled");
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toHaveAttribute("data-name", "enabled-host");
    await expect(page.locator("#metric-total-hosts")).toHaveText("1");
    await expect(page.locator("#fleet-tag-list")).not.toContainText(
        "paused-only",
    );
    await availability.selectOption("disabled");
    await expect(rows).toHaveCount(1);
    await expect(rows.first()).toHaveAttribute("data-name", "disabled-host");
    await expect(rows.first()).toContainText("Disabled");
    await expect(page.locator("#fleet-tag-list")).toContainText("paused-only");
    await page.reload();
    await expect(availability).toHaveValue("disabled");
    await expect(rows.first()).toHaveAttribute("data-name", "disabled-host");
    await page.setViewportSize({ width: 390, height: 844 });
    await availability.selectOption("all");
    await expect(rows).toHaveCount(2);
    await expect(page.locator("#metric-total-hosts")).toHaveText("2");
    await page.reload();
    await expect(availability).toHaveValue("all");
    await expect(rows).toHaveCount(2);
    await availability.selectOption("enabled");
    await expect(rows).toHaveCount(1);
});

test("installed vulnerability scans show trust states and residual exposure without pending updates", async ({
    baseURL,
    page,
}) => {
    const server = makeServer("installed-host", "done");
    let scanState = "not_scanned";
    let started = 0;
    await stubDashboardApi(page, () => [server]);
    await page.route(
        "**/api/servers/installed-host/vulnerabilities/scan",
        (route) => {
            started += 1;
            return fulfillJson(route, { status: "running" }, 202);
        },
    );
    await page.route("**/api/dashboard/summary*", (route) => {
        const summary = makeDashboardSummary([server]);
        const host = summary.servers[0];
        const exposed = scanState === "partial" || scanState === "unavailable";
        host.risk = {
            level: "unknown",
            summary: exposed
                ? "1 confirmed CVE · unknown severity"
                : "Installed packages not scanned",
            vulnerability: {
                state: scanState,
                coverage: scanState === "not_scanned" ? "unknown" : scanState,
                freshness: scanState === "unavailable" ? "stale" : "fresh",
                result: exposed
                    ? "confirmed_exposure"
                    : scanState === "complete"
                      ? "no_findings"
                      : "unknown",
                inventoried: 2,
                evaluated: scanState === "partial" ? 1 : 2,
                unknown: scanState === "partial" ? 1 : 0,
                unsupported: 0,
                findings: exposed
                    ? [
                          {
                              id: "CVE-2026-1000",
                              package: "installed-package",
                              severity: "unknown",
                              disposition: "no_fix_available",
                              first_seen: "2026-10-01T00:00:00Z",
                              last_seen: "2026-10-25T00:00:00Z",
                              age_days: 24,
                              source: "Debian Security Tracker via OSV",
                          },
                      ]
                    : [],
            },
        };
        host.approval_triage = {
            ...host.approval_triage,
            cve_count: exposed ? 1 : 0,
            risk_level: "unknown",
            risk_label: host.risk.summary,
        };
        host.actions = {
            scan_vulnerabilities: {
                enabled: scanState !== "running",
                readiness: scanState === "running" ? "in_progress" : "ready",
                reason:
                    scanState === "running"
                        ? "Vulnerability scan in progress"
                        : "Ready",
            },
        };
        return fulfillJson(route, summary);
    });
    await ensureAuthenticatedSession(page);
    const section = page.getByRole("region", {
        name: "Installed package vulnerabilities",
    });
    await expect(section).toContainText("Never scanned");
    await section.getByRole("button", { name: "Scan vulnerabilities" }).click();
    await expect.poll(() => started).toBe(1);
    for (const [state, label] of [
        ["running", "Scan running"],
        ["partial", "Partial scan"],
        ["unavailable", "Scan unavailable"],
        ["complete", "Complete scan"],
    ]) {
        scanState = state;
        server.status = state === "running" ? "vulnerability_scan" : "done";
        await page.reload();
        await expect(section).toContainText(label);
        if (state === "running") {
            await expect(
                section.getByRole("button", { name: "Scan vulnerabilities" }),
            ).toBeDisabled();
            await expect(
                page.locator("#selected-host-panel .status-pill"),
            ).toHaveClass(/status-vulnerability_scan/);
            await expect(
                page.locator(
                    '#servers-table tr[data-name="installed-host"] .status-pill',
                ),
            ).toHaveClass(/status-vulnerability_scan/);
        } else if (state !== "complete") {
            await section.locator("summary").click();
            await expect(section.locator("details")).toHaveAttribute(
                "open",
                "",
            );
            await page.evaluate(() => window.fetchDashboardSummary(true));
            await expect(section.locator("details")).toHaveAttribute(
                "open",
                "",
            );
            await expect(section).toContainText("CVE-2026-1000");
            await expect(section).toContainText("No newer candidate available");
            await expect(section).toContainText("Severity: unknown");
            await expect(section).toContainText("Age: 24 days");
            if (state === "unavailable")
                await expect(section).toContainText("Freshness: stale");
            await section.locator("summary").click();
            await page.evaluate(() => window.fetchDashboardSummary(true));
            await expect(section.locator("details")).not.toHaveAttribute(
                "open",
                "",
            );
        } else {
            await expect(section).toContainText(
                "No findings in evaluated packages",
            );
            await expect(section).not.toContainText("CVE-2026-1000");
        }
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(section).toBeVisible();
    const overflow = await page.evaluate(
        () => document.documentElement.scrollWidth > window.innerWidth,
    );
    expect(overflow).toBe(false);
});

test("partial package resolution shows remaining uncertainty and inspectable proof", async ({ page }) => {
    const server = makeServer("proof-host", "done");
    let resolutionCount = 1;
    await stubDashboardApi(page, () => [server]);
    await page.route("**/api/dashboard/summary*", route => {
        const summary = makeDashboardSummary([server]);
        summary.servers[0].risk = { level: "unknown", cves: ["CVE-2026-2000"], vulnerability: {
            state: "partial", coverage: "partial", freshness: "fresh", result: "needs_review",
            inventoried: 2, evaluated: 1, unknown: 1, unsupported: 0,
            reported_cves: 1, needs_review_cves: 1, occurrences: 1,
            current_occurrences: 0, unverified_occurrences: 1, resolved_occurrences: resolutionCount,
            findings: [{id: "CVE-2026-2000", package: "unverified-package", evidence_state: "not_reverified", freshness: "stale", severity: "unknown"}],
            resolved_findings: [{id: "CVE-2026-1000", package: "updated-package", resolved_at: "2026-10-02T00:00:00Z", resolution: {reason: "installed_version_changed", scan_id: "proof-528", verified_at: "2026-10-02T00:00:00Z", source: "https://example.invalid/querybatch"}}],
            enrichment_errors: ["Optional advisory details unavailable"]
        }};
        return fulfillJson(route, summary);
    });
    await ensureAuthenticatedSession(page);
    const section = page.getByRole("region", {name: "Installed package vulnerabilities"});
    await expect(section).toContainText("Partial scan");
    await expect(section).toContainText("0 current package findings · 1 not reverified · 1 resolved by scan evidence");
    await expect(section).toContainText("Advisory enrichment incomplete");
    await section.getByText("1 active package findings", {exact: true}).click();
    await expect(section).toContainText("CVE-2026-2000");
    await section.getByText("1 resolved package findings", {exact: true}).click();
    await expect(section).toContainText("CVE-2026-1000");
    await expect(section).toContainText("No longer reported after installed version changed");
    await expect(section).toContainText("Scan: proof-528");
    resolutionCount = 120;
    await page.evaluate(() => window.fetchDashboardSummary(true));
    await expect(section).toContainText("1 of 120 resolved package findings");
    await expect(section).toContainText("Showing the most recent resolutions.");
    await expect(section.locator(".resolved-vulnerability-findings")).toHaveAttribute("open", "");
    await page.setViewportSize({width: 390, height: 844});
    expect(await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth)).toBe(false);
});

test("attention metrics and filters use the same facts and CVE semantics", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("fresh-host", "done"),
        makeServer("stale-host", "done", [], { facts_state: "stale" }),
        makeServer("unknown-host", "done", [], { facts_state: "unknown" }),
        makeServer("cve-host", "pending_approval", makePendingUpdates(1)),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);

    const factsMetric = page.locator('[data-metric-filter="stale_facts"]');
    const cveMetric = page.locator('[data-metric-filter="high_risk"]');
    await expect(factsMetric).toContainText("Host facts to refresh");
    await expect(factsMetric.locator("strong")).toHaveText("2");
    await expect(cveMetric).toContainText("CVE matches");
    await expect(cveMetric.locator("strong")).toHaveText("1");

    const factsFilter = page.locator('[data-fleet-filter="stale_facts"]');
    await expect(factsFilter).toContainText("Facts refresh");
    await expect(factsFilter.locator("strong")).toHaveText("2");
    await factsFilter.evaluate((button) => button.click());
    const factsRows = page.locator("#servers-table tbody tr[data-name]");
    await expect(factsRows).toHaveCount(2);
    await expect
        .poll(() =>
            factsRows.evaluateAll((rows) =>
                rows.map((row) => row.dataset.name).sort(),
            ),
        )
        .toEqual(["stale-host", "unknown-host"]);

    const cveFilter = page.locator('[data-fleet-filter="high_risk"]');
    await expect(cveFilter).toContainText("CVE matches");
    await expect(cveFilter.locator("strong")).toHaveText("1");
    await cveFilter.evaluate((button) => button.click());
    const cveRows = page.locator("#servers-table tbody tr[data-name]");
    await expect(cveRows).toHaveCount(1);
    await expect(cveRows.first()).toHaveAttribute("data-name", "cve-host");
});

test("maintenance queue starts by name and lets the operator reverse the order", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("healthy-host", "done"),
        makeServer("monitor-host", "upgrading"),
        makeServer("refresh-host", "done", [], { facts_state: "stale" }),
        makeServer("approval-host", "pending_approval", makePendingUpdates(1)),
        makeServer("failure-host", "error"),
        makeServer("repair-host", "needs_reconciliation"),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);

    const rows = page.locator("#servers-table tbody tr[data-name]");
    await expect(rows).toHaveCount(6);
    await expect(
        page.locator('#servers-table th[data-sort-key="name"]'),
    ).toHaveAttribute("aria-sort", "ascending");
    await expect
        .poll(() =>
            rows.evaluateAll((elements) =>
                elements.map((element) => element.dataset.name),
            ),
        )
        .toEqual([
            "approval-host",
            "failure-host",
            "healthy-host",
            "monitor-host",
            "refresh-host",
            "repair-host",
        ]);

    await page.getByRole("button", { name: "Sort by Server" }).click();
    await expect(
        page.locator('#servers-table th[data-sort-key="name"]'),
    ).toHaveAttribute("aria-sort", "descending");
    await expect
        .poll(() =>
            rows.evaluateAll((elements) =>
                elements.map((element) => element.dataset.name),
            ),
        )
        .toEqual([
            "repair-host",
            "refresh-host",
            "monitor-host",
            "healthy-host",
            "failure-host",
            "approval-host",
        ]);
});

test("degraded Status synchronization stays visible beside the timeline until recovery succeeds", async ({
    baseURL,
    page,
}) => {
    const servers = Array.from({ length: 25 }, (_, index) =>
        makeServer(`sync-host-${index + 1}`, "done"),
    );
    let degraded = false;
    let delayRecovery = false;
    await stubDashboardApi(page, () => servers);

    const degradeRoute = async (route) => {
        if (degraded) {
            await route.fulfill({
                status: 503,
                contentType: "application/json",
                body: '{"error":"offline"}',
            });
            return;
        }
        if (delayRecovery)
            await new Promise((resolve) => setTimeout(resolve, 250));
        await route.fallback();
    };
    await page.route("**/api/dashboard/summary*", degradeRoute);
    await page.route("**/api/audit-events*", degradeRoute);
    await page.route("**/api/observability/summary*", degradeRoute);
    await page.route("**/api/update-policies", degradeRoute);

    await ensureAuthenticatedSession(page);
    await expect(page.locator("#last-sync-label")).toContainText("Synced");

    degraded = true;
    await page.evaluate(() => {
        window.fetchDashboardExtras("degraded-sync-test");
    });

    const notice = page.locator("#sync-degraded-notice");
    await expect(notice).toBeVisible();
    await expect(notice.locator("#sync-degraded-summary")).toContainText(
        "Dashboard",
    );
    await expect(notice.locator("#sync-degraded-summary")).toContainText(
        "Audit trail",
    );
    await expect(notice.locator("#sync-degraded-summary")).toContainText(
        "Observability",
    );
    await expect(notice.locator("#sync-degraded-summary")).toContainText(
        "Policies",
    );
    await expect(notice.locator("#sync-degraded-summary")).toContainText(
        "may be out of date",
    );
    await expect(notice.locator("#sync-degraded-last-success")).toContainText(
        "Last successful refresh",
    );
    await expect
        .poll(() =>
            notice.evaluate((element) => getComputedStyle(element).position),
        )
        .toBe("sticky");
    const timelineScrollTarget = await page
        .locator(".timeline-workspace")
        .evaluate(
            (element) =>
                element.getBoundingClientRect().top + window.scrollY + 180,
        );
    await page.evaluate(
        (scrollTarget) => window.scrollTo(0, scrollTarget),
        timelineScrollTarget,
    );
    await expect
        .poll(() => notice.boundingBox().then((box) => Math.round(box?.y || 0)))
        .toBe(68);

    degraded = false;
    delayRecovery = true;
    await page.evaluate(() => {
        window.fetchDashboardExtras("degraded-sync-recovery-test");
    });
    await page.waitForTimeout(100);
    await expect(notice).toBeVisible();
    await expect(notice).toBeHidden();
});

test("cancelled approval refreshes to idle after the short cancellation state", async ({
    baseURL,
    page,
}) => {
    let servers = [
        makeServer(
            "cancelled-host",
            "pending_approval",
            makePendingUpdates(1),
            { has_key: true },
        ),
    ];
    let cancellationCompletedAt = 0;
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/cancel/cancelled-host", async (route) => {
        await fulfillJson(route, { ok: true });
        setTimeout(() => {
            servers = [
                makeServer("cancelled-host", "idle", [], { has_key: true }),
            ];
            cancellationCompletedAt = Date.now();
        }, 200);
    });
    await ensureAuthenticatedSession(page);

    await page.locator("#status-supporting-details > summary").click();
    await page
        .locator(
            '#approval-triage-table button[data-action="cancel-upgrade"][data-name="cancelled-host"]',
        )
        .click();
    await expect.poll(() => cancellationCompletedAt).toBeGreaterThan(0);
    await expect
        .poll(
            () =>
                page
                    .locator(
                        '#servers-table tbody tr[data-name="cancelled-host"] .status-pill',
                    )
                    .textContent(),
            { timeout: 1500 },
        )
        .toBe("idle");
});

test("APT upgrade log drawer renders carriage-return progress during refresh", async ({
    baseURL,
    page,
}) => {
    await page.addInitScript(() => {
        window.EventSource = undefined;
    });
    let servers = [
        makeServer("apt-live-host", "upgrading", [], {
            job_id: "job-polling",
            logs: "Running apt upgrade...\nReading database ... 25%\rReading database ... 50%",
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);

    await page
        .locator(
            '#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]',
        )
        .click();
    const logs = page.locator("#drawer-logs");
    await expect(logs.locator(".log-line")).toHaveText([
        "Running apt upgrade...",
        "Reading database ... 50%",
    ]);

    servers = [
        makeServer("apt-live-host", "upgrading", [], {
            job_id: "job-polling",
            logs: "Running apt upgrade...\nReading database ... 25%\rReading database ... 50%\rUnpacking openssl",
        }),
    ];
    await page.evaluate(() => window.fetchServers(true, "apt-live-output"));

    await expect(logs.locator(".log-line").last()).toHaveText(
        "Unpacking openssl",
    );
    await expect
        .poll(() =>
            logs.evaluate(
                (element) =>
                    element.scrollHeight -
                    element.scrollTop -
                    element.clientHeight,
            ),
        )
        .toBeLessThanOrEqual(1);
});

test("structured SSE job logs preserve drawer scroll and avoid server refetches", async ({
    baseURL,
    page,
}) => {
    await page.addInitScript(() => {
        class TestEventSource {
            constructor() {
                this.listeners = new Map();
                window.__statusEventSource = this;
                setTimeout(() => this.emit("open", {}), 0);
            }
            addEventListener(name, handler) {
                if (!this.listeners.has(name)) this.listeners.set(name, []);
                this.listeners.get(name).push(handler);
            }
            emit(name, event) {
                (this.listeners.get(name) || []).forEach((handler) =>
                    handler(event),
                );
            }
            close() {}
        }
        window.EventSource = TestEventSource;
    });
    const servers = [
        makeServer("stream-host", "idle", [], {
            job_id: "job-live",
            logs: "",
        }),
    ];
    let serverRequests = 0;
    page.on("request", (request) => {
        if (new URL(request.url()).pathname === "/api/servers")
            serverRequests += 1;
    });
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/jobs/job-live/logs*", (route) => {
        const afterSequence = Number(
            new URL(route.request().url()).searchParams.get("after_seq") || 0,
        );
        return fulfillJson(route, {
            job_id: "job-live",
            fragments: [],
            next_sequence: afterSequence,
            has_more: false,
            expired: false,
            truncated: false,
            retention_days: 30,
        });
    });
    await ensureAuthenticatedSession(page);
    await page
        .locator(
            '#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]',
        )
        .click();
    const logs = page.locator("#drawer-logs");
    await expect(logs).toHaveAttribute("role", "log");
    await expect(logs).toHaveAttribute("aria-live", "polite");
    await expect
        .poll(() => page.evaluate(() => !!window.__statusEventSource))
        .toBe(true);
    const requestsBeforeLogs = serverRequests;

    const initialData =
        Array.from({ length: 100 }, (_, index) => `line-${index + 1}`).join(
            "\n",
        ) + "\nReading 10%\rReading 50%\r";
    await page.evaluate(
        (payload) => {
            window.__statusEventSource.emit("dashboard", {
                data: JSON.stringify(payload),
            });
        },
        {
            reason: "job.log",
            server_name: "stream-host",
            job_id: "job-live",
            sequence: 1,
            stream: "stdout",
            data: initialData,
        },
    );
    await expect(logs.locator(".log-line").last()).toHaveText("Reading 50%");
    await expect
        .poll(() =>
            logs.evaluate(
                (element) =>
                    element.scrollHeight -
                    element.scrollTop -
                    element.clientHeight,
            ),
        )
        .toBeLessThanOrEqual(1);

    await logs.evaluate((element) => {
        element.scrollTop = 120;
        element.dispatchEvent(new Event("scroll"));
    });
    const pausedScrollTop = await logs.evaluate((element) => element.scrollTop);
    await page.evaluate(
        (payload) => {
            window.__statusEventSource.emit("dashboard", {
                data: JSON.stringify(payload),
            });
        },
        {
            reason: "job.log",
            server_name: "stream-host",
            job_id: "job-live",
            sequence: 2,
            stream: "stderr",
            data: "warning while paused\n",
        },
    );
    await expect
        .poll(() => logs.evaluate((element) => element.scrollTop))
        .toBe(pausedScrollTop);
    await expect
        .poll(() =>
            page.evaluate(() =>
                window.statusPageInteraction
                    .getView()
                    .jobLogs["stream-host"].rawText.includes(
                        "warning while paused",
                    ),
            ),
        )
        .toBe(true);

    await logs.evaluate((element) => {
        element.scrollTop = element.scrollHeight;
        element.dispatchEvent(new Event("scroll"));
    });
    await page.evaluate(
        (payload) => {
            window.__statusEventSource.emit("dashboard", {
                data: JSON.stringify(payload),
            });
        },
        {
            reason: "job.log",
            server_name: "stream-host",
            job_id: "job-live",
            sequence: 3,
            stream: "stdout",
            data: "followed tail\n",
        },
    );
    await expect
        .poll(() =>
            logs.evaluate(
                (element) =>
                    element.scrollHeight -
                    element.scrollTop -
                    element.clientHeight,
            ),
        )
        .toBeLessThanOrEqual(1);

    await page.locator("#status-drawer-close").click();
    await page.evaluate(
        (payload) => {
            window.__statusEventSource.emit("dashboard", {
                data: JSON.stringify(payload),
            });
        },
        {
            reason: "job.log",
            server_name: "stream-host",
            job_id: "job-live",
            sequence: 4,
            stream: "stdout",
            data: "<b>escaped while closed</b>\n",
        },
    );
    await page
        .locator(
            '#servers-table tbody button[data-action="open-drawer"][data-tab="logs"]',
        )
        .click();
    await expect(logs).toContainText("<b>escaped while closed</b>");
    await expect(logs.locator("b")).toHaveCount(0);
    expect(serverRequests).toBe(requestsBeforeLogs);
});

for (const responseTiming of ["offline", "reconnected"]) {
    test(`SSE reconnect recovers a completed unselected job after an ${responseTiming} log response`, async ({
        baseURL,
        page,
    }) => {
        await ensureAuthenticatedSession(page);
        await page.addInitScript(() => {
            window.__recoveryEventSources = [];
            window.EventSource = class {
                constructor() {
                    this.listeners = new Map();
                    window.__recoveryEventSources.push(this);
                    if (window.__recoveryEventSources.length === 1)
                        setTimeout(() => this.emit("open"), 0);
                }
                addEventListener(name, handler) {
                    if (!this.listeners.has(name)) this.listeners.set(name, []);
                    this.listeners.get(name).push(handler);
                }
                emit(name) {
                    (this.listeners.get(name) || []).forEach((handler) =>
                        handler({}),
                    );
                }
                close() {}
            };
        });
        let servers = [
            makeServer("anchor", "done", [], { job_id: "job-anchor" }),
            makeServer("worker", "updating", [], { job_id: "job-worker" }),
        ];
        await stubDashboardApi(page, () => servers);
        let releaseFirst;
        const firstPage = new Promise((resolve) => {
            releaseFirst = resolve;
        });
        const cursors = [];
        await page.route("**/api/jobs/*/logs*", async (route) => {
            if (!route.request().url().includes("/job-worker/"))
                return fulfillJson(route, { fragments: [], has_more: false });
            cursors.push(
                Number(
                    new URL(route.request().url()).searchParams.get(
                        "after_seq",
                    ),
                ),
            );
            const first = cursors.length === 1;
            if (first) await firstPage;
            return fulfillJson(route, {
                fragments: [
                    {
                        sequence: first ? 1 : 2,
                        data: first
                            ? "before outage\n"
                            : "completed during outage\n",
                    },
                ],
                has_more: false,
            });
        });
        try {
            await page.goto("/");
            await expect.poll(() => cursors.length).toBe(1);
            await expect
                .poll(() =>
                    page.evaluate(
                        () =>
                            window.statusPageInteraction.getView()
                                .primaryServerName,
                    ),
                )
                .toBe("anchor");
            await page.evaluate(() =>
                window.__recoveryEventSources[0].emit("error"),
            );
            servers = servers.map((server) => ({
                ...server,
                status: "done",
                logs: "bounded terminal preview",
            }));
            await page.evaluate(() =>
                window.fetchServers(true, "offline-completion"),
            );
            if (responseTiming === "offline") {
                releaseFirst();
                await expect
                    .poll(() =>
                        page.evaluate(
                            () =>
                                window.statusPageInteraction.getView().jobLogs
                                    .worker.lastSequence,
                        ),
                    )
                    .toBe(1);
            }
            await expect
                .poll(() =>
                    page.evaluate(() => window.__recoveryEventSources.length),
                )
                .toBe(2);
            await page.evaluate(() =>
                window.__recoveryEventSources[1].emit("open"),
            );
            if (responseTiming === "reconnected") releaseFirst();
            await expect
                .poll(() =>
                    page.evaluate(
                        () =>
                            window.statusPageInteraction.getView().jobLogs
                                .worker.rawText,
                    ),
                )
                .toBe("before outage\ncompleted during outage\n");
            expect(cursors).toEqual([0, 1]);
            await page
                .locator(
                    '#servers-table button[data-action="open-drawer"][data-name="worker"][data-tab="logs"]',
                )
                .click();
            await expect(page.locator("#drawer-logs")).toContainText(
                "completed during outage",
            );
        } finally {
            releaseFirst();
        }
    });
}

test("bulk actions execute immediately, skip ineligible hosts, and report partial failures", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("idle-host", "idle", [], { has_key: true }),
        makeServer("ok-sec-host", "pending_approval", makePendingUpdates(3), {
            has_password: true,
            upgrade_plan: {
                standard_package_count: 2,
                kept_back_package_count: 1,
                standard_security_count: 1,
                total_security_count: 2,
                kept_back_security_plan_available: true,
                kept_back_security_removed_packages: ["old-sec-lib"],
            },
        }),
        makeServer("fail-sec-host", "pending_approval", makePendingUpdates(3), {
            has_password: true,
        }),
        makeServer("no-sec-host", "pending_approval", [], {
            has_password: true,
        }),
    ];
    const state = {};
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/update/idle-host", (route) => {
        state.updateIdle = (state.updateIdle || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/approve/ok-sec-host", (route) => {
        state.approveStandard = (state.approveStandard || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route(
        "**/api/approve-security-kept-back/ok-sec-host",
        (route) => {
            state.approveKept = (state.approveKept || 0) + 1;
            return fulfillJson(route, { ok: true });
        },
    );
    await page.route("**/api/approve-security/ok-sec-host", (route) => {
        state.approveSecurityOk = (state.approveSecurityOk || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/approve-security/fail-sec-host", (route) => {
        state.approveSecurityFail = (state.approveSecurityFail || 0) + 1;
        return route.fulfill({
            status: 500,
            contentType: "application/json",
            body: JSON.stringify({ error: "backend down" }),
        });
    });
    await page.route("**/api/approve-security/no-sec-host", (route) => {
        state.approveSecuritySkipped = (state.approveSecuritySkipped || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/cancel/ok-sec-host", (route) => {
        state.cancelOk = (state.cancelOk || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/autoremove/idle-host", (route) => {
        state.autoremoveIdle = (state.autoremoveIdle || 0) + 1;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/servers/idle-host/facts/refresh", (route) => {
        state.refreshIdle = (state.refreshIdle || 0) + 1;
        return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    await page
        .locator('#servers-table tbody tr[data-name="idle-host"] .row-select')
        .check();
    await page
        .locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select')
        .check();
    await page.locator("#auth-filter").selectOption("key");
    await expect(page.locator("#bulk-update")).toHaveText("Update (1)");
    await expect(page.locator("#bulk-action-hint")).toHaveText(
        "2 selected · 1 visible · 1 outside current view",
    );
    await page.locator("#bulk-update").click();
    await expect.poll(() => state.updateIdle || 0).toBe(1);
    await expect(page.locator("#bulk-action-hint")).toHaveText(
        "2 selected · 1 executed · 1 skipped",
    );
    await expect(page.locator("#bulk-review-modal")).toHaveCount(0);
    await expect(page.locator("#typed-confirm-modal")).not.toBeVisible();
    await page.locator("#auth-filter").selectOption("");
    await page
        .locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select')
        .uncheck();

    await page
        .locator('#servers-table tbody tr[data-name="idle-host"] .row-select')
        .uncheck();
    await page
        .locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select')
        .check();
    await expect(page.locator("#bulk-approve")).toHaveText(
        "Approve standard (1)",
    );
    await page.locator("#bulk-approve").click();
    await expect.poll(() => state.approveStandard || 0).toBe(1);

    await page.locator("#bulk-approve-kept-security").click();
    await expect.poll(() => state.approveKept || 0).toBe(1);

    await page
        .locator(
            '#servers-table tbody tr[data-name="fail-sec-host"] .row-select',
        )
        .check();
    await page
        .locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select')
        .check();
    await page.locator("#bulk-approve-security").click();
    await expect.poll(() => state.approveSecurityOk || 0).toBe(1);
    await expect.poll(() => state.approveSecurityFail || 0).toBe(1);
    await expect.poll(() => state.approveSecuritySkipped || 0).toBe(0);
    const failureNotice = page.locator('#app-feedback-region[role="alert"]');
    await expect(failureNotice).toContainText("fail-sec-host: backend down");
    await expect(failureNotice).not.toContainText("no-sec-host: backend down");

    await page
        .locator(
            '#servers-table tbody tr[data-name="fail-sec-host"] .row-select',
        )
        .uncheck();
    await page
        .locator('#servers-table tbody tr[data-name="no-sec-host"] .row-select')
        .uncheck();
    await page.locator("#bulk-cancel").click();
    await expect.poll(() => state.cancelOk || 0).toBe(1);

    await page
        .locator('#servers-table tbody tr[data-name="ok-sec-host"] .row-select')
        .uncheck();
    await page
        .locator('#servers-table tbody tr[data-name="idle-host"] .row-select')
        .check();
    await expect(page.locator("#bulk-autoremove")).toHaveText("Autoremove (1)");
    await expect(page.locator("#refresh-all-facts")).toHaveText(
        "Refresh facts (1)",
    );
    await page.locator("#bulk-autoremove").click();
    await expect.poll(() => state.autoremoveIdle || 0).toBe(1);

    await page.locator("#refresh-all-facts").click();
    await expect.poll(() => state.refreshIdle || 0).toBe(1);
});

test("auto refresh defers table replacement while an update action is being clicked", async ({
    baseURL,
    page,
}) => {
    let servers = [makeServer("demo-host")];
    let updateRequests = 0;
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/update/demo-host", (route) => {
        updateRequests += 1;
        return fulfillJson(route, { ok: true });
    });

    await ensureAuthenticatedSession(page);

    const updateButton = page.locator(
        '#servers-table tbody button[data-action="update-server"][data-name="demo-host"]',
    );
    await expect(updateButton).toBeVisible();

    await updateButton.hover();
    await page.mouse.down();
    await page.waitForTimeout(25);

    servers = [makeServer("renamed-host")];
    await page.evaluate(() => window.fetchServers());
    await expect(
        page.locator('#servers-table tbody tr[data-name="demo-host"]'),
    ).toBeVisible();

    await page.mouse.up();

    await expect.poll(() => updateRequests).toBe(1);
});

test("auto refresh resumes when an action press loses page focus", async ({
    baseURL,
    page,
}) => {
    let servers = [makeServer("demo-host")];
    await stubDashboardApi(page, () => servers);

    await ensureAuthenticatedSession(page);

    const updateButton = page.locator(
        '#servers-table tbody button[data-action="update-server"][data-name="demo-host"]',
    );
    await expect(updateButton).toBeVisible();

    await updateButton.hover();
    await page.mouse.down();
    await page.waitForTimeout(25);

    servers = [makeServer("renamed-host")];
    await page.evaluate(() => window.fetchServers());
    await expect(
        page.locator('#servers-table tbody tr[data-name="demo-host"]'),
    ).toBeVisible();

    await page.evaluate(() => window.dispatchEvent(new Event("blur")));

    await expect(
        page.locator('#servers-table tbody tr[data-name="renamed-host"]'),
    ).toBeVisible();
    await page.mouse.up();
});

test("status metrics stay compact without secondary descriptions", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    for (const viewport of [
        { width: 1512, height: 1100 },
        { width: 1920, height: 1100 },
        { width: 1216, height: 879 },
        { width: 980, height: 1100 },
        { width: 390, height: 844 },
    ]) {
        await page.setViewportSize(viewport);
        await page.goto("/");

        const metricState = await page
            .locator(".metric-strip")
            .evaluate((element) => ({
                height: element.getBoundingClientRect().height,
                timelineGap: Math.round(
                    document
                        .querySelector(".timeline-workspace")
                        .getBoundingClientRect().top -
                        element.getBoundingClientRect().bottom,
                ),
                rowTops: [...element.querySelectorAll(".metric-item")]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .map((item) =>
                        Math.round(item.getBoundingClientRect().top),
                    ),
                labelTops: [...element.querySelectorAll(".metric-item")]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .map((item) =>
                        Math.round(
                            item.querySelector("span").getBoundingClientRect()
                                .top,
                        ),
                    ),
                numberTops: [...element.querySelectorAll(".metric-item")]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .map((item) =>
                        Math.round(
                            item.querySelector("strong").getBoundingClientRect()
                                .top,
                        ),
                    ),
                overlappingLabels: [...element.querySelectorAll(".metric-item")]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .filter((item) => {
                        const range = document.createRange();
                        range.selectNodeContents(item.querySelector("span"));
                        return (
                            range.getBoundingClientRect().bottom >
                            item.querySelector("strong").getBoundingClientRect()
                                .top
                        );
                    })
                    .map((item) =>
                        item.querySelector("span").textContent.trim(),
                    ),
                visibleDescriptions: [...element.querySelectorAll("small")]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .map((item) => item.textContent.trim()),
                visibleLegacyMetrics: [
                    ...element.querySelectorAll(".metric-hidden"),
                ]
                    .filter((item) => getComputedStyle(item).display !== "none")
                    .map((item) => item.textContent.trim()),
            }));

        expect(metricState.visibleDescriptions).toEqual([]);
        expect(metricState.visibleLegacyMetrics).toEqual([]);
        expect(
            metricState.overlappingLabels,
            `Labels must not overlap values at ${viewport.width}px`,
        ).toEqual([]);
        if (viewport.width >= 981) {
            expect(new Set(metricState.rowTops).size).toBe(1);
            expect(new Set(metricState.labelTops).size).toBe(1);
            expect(metricState.timelineGap).toBeLessThanOrEqual(24);
        }
        const numbersByRow = new Map();
        metricState.rowTops.forEach((rowTop, index) => {
            if (!numbersByRow.has(rowTop)) numbersByRow.set(rowTop, new Set());
            numbersByRow.get(rowTop).add(metricState.numberTops[index]);
        });
        expect(
            [...numbersByRow.values()].every((values) => values.size === 1),
            "Numbers remain aligned within each row",
        ).toBe(true);
        if (viewport.width >= 981) expect(metricState.height).toBeLessThan(300);
        else
            expect(
                metricState.height / new Set(metricState.rowTops).size,
            ).toBeLessThan(120);
    }
});

test("status renders accepted secondary snapshots through the page interaction state", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("secondary-host", "idle", [], {
            has_key: false,
            has_password: false,
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await page.route("**/api/keys/global", (route) =>
        fulfillJson(route, { has_key: true }),
    );
    await page.route("**/api/audit-events*", (route) =>
        fulfillJson(route, {
            items: [
                {
                    id: 42,
                    created_at: "2026-05-28T12:00:00Z",
                    created_at_display: "May 28, 2026 12:00",
                    action: "server.update",
                    target_type: "server",
                    target_name: "secondary-host",
                    status: "success",
                    message: "Update completed",
                },
            ],
        }),
    );
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            totals: { updates_total: 4, success_rate_pct: 75 },
        }),
    );
    await page.route("**/api/update-policies", (route) =>
        fulfillJson(route, [{ id: 1 }, { id: 2 }]),
    );

    await ensureAuthenticatedSession(page);
    await page.goto("/");

    await expect(page.locator("#recent-activity")).toContainText(
        "server.update",
    );
    await expect(page.locator("#observability-summary-label")).toHaveText(
        "7d 75%",
    );
    await expect(page.locator("#policy-summary-label")).toHaveText(
        "Policies 2",
    );
    await expect(page.locator("body")).toContainText("Global SSH key");

    const accepted = await page.evaluate(() => {
        const view = window.statusPageInteraction.getView();
        return {
            auditID: view.extras.recentActivity[0]?.id,
            observabilityUpdates:
                view.extras.observabilitySummary?.totals?.updates_total,
            policyCount: view.extras.policySummary.length,
            globalKeyAvailable: view.globalKeyAvailable,
        };
    });
    expect(accepted).toEqual({
        auditID: 42,
        observabilityUpdates: 4,
        policyCount: 2,
        globalKeyAvailable: true,
    });
});

test("status command history keeps quiet hosts visible and noisy hosts bounded to eight items", async ({
    baseURL,
    page,
}) => {
    const noisyHistory = Array.from({ length: 8 }, (_, index) => ({
        created_at: `2026-05-28T${String(20 - index).padStart(2, "0")}:00:00Z`,
        created_at_display: `May 28, 2026 ${String(20 - index).padStart(2, "0")}:00`,
        action: `server.noisy.${index}`,
        status: "success",
        message: `Noisy command ${index}`,
        actor: "operator",
    }));
    const servers = [
        makeServer("noisy-host", "idle", [], { command_history: noisyHistory }),
        makeServer("quiet-host", "idle", [], {
            command_history: [
                {
                    created_at: "2026-05-28T01:00:00Z",
                    created_at_display: "May 28, 2026 01:00",
                    action: "server.quiet.only",
                    status: "success",
                    message: "Quiet command remains visible",
                    actor: "operator",
                },
            ],
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.goto("/");
    await page.locator("#status-supporting-details > summary").click();

    await expect(page.locator("#command-history-count")).toHaveText("8");
    await expect(
        page.locator("#command-history-panel .activity-row"),
    ).toHaveCount(3);
    await expect(
        page.locator(
            '#command-history-panel [data-toggle-mini-list="command-history-panel"]',
        ),
    ).toHaveText("Show all 8 commands");
    await page
        .locator(
            '#command-history-panel [data-toggle-mini-list="command-history-panel"]',
        )
        .click();
    await expect(
        page.locator("#command-history-panel .activity-row"),
    ).toHaveCount(8);

    await page
        .locator('#servers-table tbody tr[data-name="quiet-host"]')
        .click();
    await expect(page.locator("#command-history-count")).toHaveText("1");
    await expect(page.locator("#command-history-panel")).toContainText(
        "server.quiet.only",
    );
    await expect(page.locator("#command-history-panel")).not.toContainText(
        "server.noisy.",
    );
});

test("status rows reduce repetition and bulk controls follow selection", async ({
    baseURL,
    page,
}) => {
    await stubDashboardApi(page, () => [
        makeServer("approval-host", "pending_approval", makePendingUpdates(3)),
        makeServer("running-host", "upgrading"),
        makeServer("done-host", "done"),
    ]);
    await ensureAuthenticatedSession(page);
    await page.goto("/");
    const approval = page.locator(
        '#servers-table tr[data-name="approval-host"]',
    );
    await expect(approval).toBeVisible();
    await expect(
        approval.locator(".timeline-progress-copy strong"),
    ).toHaveCount(0);
    await expect(approval.locator(".package-summary")).toContainText(
        "3 pending packages",
    );
    await expect(approval.locator(".package-summary")).not.toContainText(
        "0 kept",
    );
    for (const width of [1196, 1920, 390]) {
        await page.setViewportSize({ width, height: 875 });
        const summary = await approval
            .locator(".package-summary")
            .evaluate((element) => ({
                clientWidth: element.clientWidth,
                scrollWidth: element.scrollWidth,
                clientHeight: element.clientHeight,
                scrollHeight: element.scrollHeight,
            }));
        expect(summary.scrollWidth).toBeLessThanOrEqual(summary.clientWidth);
        expect(summary.scrollHeight).toBeLessThanOrEqual(summary.clientHeight);
    }
    await expect(
        page.locator('tr[data-name="done-host"] .package-summary'),
    ).toHaveCount(0);
    const colors = await page
        .locator("#servers-table .status-pill")
        .evaluateAll((elements) =>
            elements.map((element) => getComputedStyle(element).color),
        );
    expect(new Set(colors).size).toBe(3);
    await expect(page.locator("#select-all")).toBeVisible();
    await expect(page.locator("#bulk-update")).toBeHidden();
    await approval.locator(".row-select").check();
    await expect(page.locator("#bulk-update")).toBeVisible();
    await expect(page.locator("#bulk-approve")).toBeEnabled();
    await approval.locator(".row-select").uncheck();
    await expect(page.locator("#bulk-update")).toBeHidden();
    await page.locator("#select-all").check();
    await expect(page.locator("#bulk-update")).toBeVisible();
    await page.locator("#select-all").uncheck();
    await expect(page.locator("#bulk-update")).toBeHidden();
});

test("desktop bulk actions share one width and keep labels fully visible", async ({
    baseURL,
    page,
}) => {
    await stubDashboardApi(page, () => [makeServer("busy-host", "upgrading")]);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1565, height: 875 });
    await page.goto("/");

    await page.locator("#select-all").check();
    await expect(page.locator("#bulk-update")).toBeVisible();

    const labelLayout = await page
        .locator(".rail-bulk .bulk-actions")
        .evaluate((element) =>
            [
                "bulk-update",
                "bulk-approve",
                "bulk-approve-security",
                "bulk-approve-kept-security",
                "bulk-cancel",
                "bulk-autoremove",
            ]
                .map((id) => element.querySelector(`#${id}`))
                .map((button) => ({
                    id: button.id,
                    clientWidth: button.clientWidth,
                    scrollWidth: button.scrollWidth,
                    clientHeight: button.clientHeight,
                    scrollHeight: button.scrollHeight,
                    backgroundColor: getComputedStyle(button).backgroundColor,
                    borderColor: getComputedStyle(button).borderColor,
                    opacity: getComputedStyle(button).opacity,
                    textOverflow: getComputedStyle(button).textOverflow,
                })),
        );

    expect(labelLayout).not.toEqual([]);
    expect(
        new Set(labelLayout.map((button) => button.clientWidth)).size,
        "bulk actions must share one width",
    ).toBe(1);
    expect(
        new Set(labelLayout.map((button) => button.clientHeight)).size,
        "bulk actions must share one height",
    ).toBe(1);
    expect(
        new Set(labelLayout.map((button) => button.backgroundColor)).size,
        "disabled bulk actions must share one neutral background",
    ).toBe(1);
    expect(
        new Set(labelLayout.map((button) => button.borderColor)).size,
        "disabled bulk actions must share one neutral border",
    ).toBe(1);
    for (const button of labelLayout) {
        expect(
            button.scrollWidth,
            `${button.id} must not clip horizontally`,
        ).toBeLessThanOrEqual(button.clientWidth);
        expect(
            button.scrollHeight,
            `${button.id} must not clip vertically`,
        ).toBeLessThanOrEqual(button.clientHeight);
        expect(
            button.opacity,
            `${button.id} must keep its disabled label legible`,
        ).toBe("1");
        expect(
            button.textOverflow,
            `${button.id} must not use an ellipsis`,
        ).not.toBe("ellipsis");
    }
});

test("selected host inspector stays accessible at common desktop widths and follows the timeline on mobile", async ({
    baseURL,
    page,
}) => {
    // Enough rows to exercise sticky positioning even when bulk controls are collapsed.
    const servers = [
        makeServer("inspector-host", "pending_approval", makePendingUpdates(3)),
        ...Array.from({ length: 10 }, (_, index) =>
            makeServer(`other-host-${index}`),
        ),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);

    for (const viewport of [
        { width: 1440, height: 900 },
        { width: 1366, height: 768 },
    ]) {
        await page.setViewportSize(viewport);
        await page.goto("/");
        await page
            .locator('#servers-table tbody tr[data-name="inspector-host"]')
            .click();
        await expect(page.locator("#selected-host-title")).toHaveText(
            "inspector-host",
        );

        const layout = await page
            .locator(".context-panel")
            .evaluate(async (element) => {
                // Clicking the row can scroll it into view. Measure the initial layout
                // at a known scroll position before testing the sticky state below.
                window.scrollTo({ top: 0, behavior: "instant" });
                await new Promise((resolve) =>
                    requestAnimationFrame(() => requestAnimationFrame(resolve)),
                );
                const bounds = element.getBoundingClientRect();
                const timeline = document
                    .querySelector(".timeline-workspace")
                    .getBoundingClientRect();
                const table = document
                    .querySelector(".timeline-workspace .table-wrap")
                    .getBoundingClientRect();
                const lastAction = document
                    .querySelector(
                        '#servers-table tbody tr[data-name="inspector-host"] .timeline-actions button:last-child',
                    )
                    .getBoundingClientRect();
                return {
                    position: getComputedStyle(element).position,
                    overflowY: getComputedStyle(element).overflowY,
                    scrollY: window.scrollY,
                    top: Math.round(bounds.top),
                    timelineTop: Math.round(timeline.top),
                    viewportHeight: window.innerHeight,
                    clientWidth: element.clientWidth,
                    scrollWidth: element.scrollWidth,
                    lastActionRight: lastAction.right,
                    tableRight: table.right,
                };
            });
        expect(layout.position).toBe("sticky");
        expect(layout.overflowY).toBe("auto");
        expect(layout.scrollY).toBe(0);
        expect(layout.top).toBeLessThanOrEqual(layout.timelineTop);
        expect(layout.top).toBeLessThan(layout.viewportHeight);
        expect(layout.scrollWidth).toBeLessThanOrEqual(layout.clientWidth);
        expect(layout.lastActionRight).toBeLessThanOrEqual(
            layout.tableRight + 1,
        );

        const stickyBounds = await page.evaluate(async () => {
            window.scrollTo(
                0,
                document.querySelector(".timeline-workspace").offsetTop,
            );
            await new Promise((resolve) =>
                requestAnimationFrame(() => requestAnimationFrame(resolve)),
            );
            const bounds = document
                .querySelector(".context-panel")
                .getBoundingClientRect();
            return {
                top: Math.round(bounds.top),
                bottom: Math.round(bounds.bottom),
                viewportHeight: window.innerHeight,
            };
        });
        expect(stickyBounds.top).toBeGreaterThanOrEqual(0);
        expect(stickyBounds.top).toBeLessThanOrEqual(68);
        expect(stickyBounds.bottom).toBeLessThanOrEqual(
            stickyBounds.viewportHeight + 1,
        );
    }

    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/");
    const mobileOrder = await page.evaluate(() => ({
        timeline: document
            .querySelector(".timeline-workspace")
            .getBoundingClientRect().top,
        inspector: document
            .querySelector(".context-panel")
            .getBoundingClientRect().top,
        supporting: document
            .querySelector("#status-supporting-details")
            .getBoundingClientRect().top,
    }));
    expect(mobileOrder.timeline).toBeLessThan(mobileOrder.inspector);
    expect(mobileOrder.inspector).toBeLessThan(mobileOrder.supporting);
});

test("maintenance timeline uses one compact progress ring per server", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("ring-host", "updating", [], { tags: ["test"] }),
        makeServer("done-host", "idle", [], {
            tags: ["test"],
            timeline_status: "done",
        }),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1565, height: 875 });
    await page.goto("/");

    const row = page.locator('#servers-table tbody tr[data-name="ring-host"]');
    await expect(row.locator(".timeline-progress-ring")).toHaveCount(1);
    await expect(row.locator(".timeline-progress-ring")).toContainText("32%");
    await expect(row.locator(".timeline-progress-copy")).toContainText(
        "Pre-checks",
    );
    await expect(row.locator(".timeline-dot")).toHaveCount(0);
    await expect(page.locator("#servers-table thead")).toContainText(
        "Maintenance",
    );
    await expect(
        page.locator(
            '#servers-table tbody tr[data-name="done-host"] .timeline-progress-copy',
        ),
    ).toContainText("Last run: Done");
    await expect(
        page.locator(
            '#servers-table tbody tr[data-name="done-host"] .timeline-progress-copy',
        ),
    ).not.toContainText("Done / Error");
    const doneRingColor = await page
        .locator(
            '#servers-table tbody tr[data-name="done-host"] .timeline-progress-ring',
        )
        .evaluate((element) => getComputedStyle(element).color);
    expect(
        doneRingColor,
        "completed maintenance must use the success color",
    ).toBe("rgb(137, 209, 133)");

    const badgeLayout = await page.evaluate(() => {
        const labels = [
            document.querySelector("#policy-summary-label"),
            document.querySelector("#priority-failures-count"),
        ];
        labels[0].textContent = "Policies 0";
        labels[1].textContent = "14";
        return labels.map((element) => ({
            whiteSpace: getComputedStyle(element).whiteSpace,
            clientWidth: element.clientWidth,
            scrollWidth: element.scrollWidth,
            clientHeight: element.clientHeight,
            scrollHeight: element.scrollHeight,
        }));
    });
    for (const badge of badgeLayout) {
        expect(
            badge.whiteSpace,
            "summary badges must keep their value on one line",
        ).toBe("nowrap");
        expect(
            badge.scrollWidth,
            "summary badges must not clip horizontally",
        ).toBeLessThanOrEqual(badge.clientWidth);
        expect(
            badge.scrollHeight,
            "summary badges must not stack value characters",
        ).toBeLessThanOrEqual(badge.clientHeight);
    }

    const busyActions = row.locator(".timeline-actions");
    await expect(
        busyActions.getByRole("button", { name: "Updating…" }),
    ).toBeDisabled();
    await expect(
        busyActions.getByRole("button", { name: "Logs" }),
    ).toBeEnabled();
    const busyActionLayout = await busyActions.evaluate((element) => {
        const buttons = Array.from(element.querySelectorAll("button"));
        return {
            containerWidth: element.getBoundingClientRect().width,
            buttonWidths: buttons.map(
                (button) => button.getBoundingClientRect().width,
            ),
        };
    });
    expect(busyActionLayout.buttonWidths).toHaveLength(2);
    expect(
        Math.abs(
            busyActionLayout.buttonWidths[0] - busyActionLayout.buttonWidths[1],
        ),
    ).toBeLessThanOrEqual(1);
    expect(
        busyActionLayout.buttonWidths[1],
        "Logs must keep its compact grid width while updating",
    ).toBeLessThan(busyActionLayout.containerWidth / 2);
    const supportingDetailsNestedInTimeline = await page
        .locator("#status-supporting-details")
        .evaluate((element) =>
            element.parentElement.classList.contains("timeline-column"),
        );
    expect(supportingDetailsNestedInTimeline).toBe(true);

    const ringSize = await row
        .locator(".timeline-progress-ring")
        .evaluate((element) => ({
            width: element.getBoundingClientRect().width,
            height: element.getBoundingClientRect().height,
        }));
    expect(ringSize.width).toBeLessThanOrEqual(44);
    expect(ringSize.height).toBeLessThanOrEqual(44);

    const overflowSamples = await row
        .locator(".timeline-progress-ring")
        .evaluate(async (element) => {
            const tableWrap = element.closest(".table-wrap");
            const samples = [];
            for (let frame = 0; frame < 12; frame += 1) {
                await new Promise((resolve) => requestAnimationFrame(resolve));
                samples.push({
                    clientHeight: tableWrap.clientHeight,
                    scrollHeight: tableWrap.scrollHeight,
                });
            }
            return samples;
        });
    for (const sample of overflowSamples) {
        expect(
            sample.scrollHeight,
            "the orbit must not create transient vertical overflow",
        ).toBe(sample.clientHeight);
    }

    await page.setViewportSize({ width: 1440, height: 900 });
    await page.reload();
    const responsiveTable = await page
        .locator(".timeline-workspace .table-wrap")
        .evaluate((element) => {
            const logs = element
                .querySelector(
                    'tbody tr[data-name="ring-host"] .timeline-actions button:last-child',
                )
                .getBoundingClientRect();
            const bounds = element.getBoundingClientRect();
            return { logsRight: logs.right, visibleRight: bounds.right };
        });
    expect(
        responsiveTable.logsRight,
        "maintenance actions must be visible without horizontal scrolling at 1440px",
    ).toBeLessThanOrEqual(responsiveTable.visibleRight + 1);

    const triageHeaders = await page
        .locator("#approval-triage-table th")
        .evaluateAll((headers) =>
            headers.map((header) => ({
                clientWidth: header.clientWidth,
                scrollWidth: header.scrollWidth,
                clientHeight: header.clientHeight,
                scrollHeight: header.scrollHeight,
            })),
        );
    for (const header of triageHeaders) {
        expect(
            header.scrollWidth,
            "approval header must not overlap horizontally",
        ).toBeLessThanOrEqual(header.clientWidth);
        expect(
            header.scrollHeight,
            "approval header must not clip vertically",
        ).toBeLessThanOrEqual(header.clientHeight);
    }

    await page.setViewportSize({ width: 390, height: 844 });
    await page.reload();
    const mobileHead = await page
        .locator(".dashboard-head")
        .evaluate((element) => {
            const description = element
                .querySelector(".muted")
                .getBoundingClientRect();
            const bounds = element.getBoundingClientRect();
            return {
                descriptionWidth: description.width,
                headWidth: bounds.width,
            };
        });
    expect(
        mobileHead.descriptionWidth,
        "mobile dashboard description must use the available row",
    ).toBeGreaterThan(mobileHead.headWidth * 0.8);
});

test("maintenance row borders align across every column", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("approval-host", "pending_approval", makePendingUpdates(15)),
        makeServer("error-host", "error"),
        makeServer("running-host", "upgrading"),
        makeServer("done-host", "done"),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.goto("/");
    const rows = page.locator("#servers-table tbody tr[data-name]");
    await expect(rows).toHaveCount(4);
    for (const width of [1196, 1024, 1920, 390]) {
        await page.setViewportSize({ width, height: 875 });
        const layouts = await rows.evaluateAll((elements) =>
            elements.map((row) => ({
                name: row.dataset.name,
                cells: [...row.cells].map((cell) => {
                    const rect = cell.getBoundingClientRect();
                    return { top: rect.top, bottom: rect.bottom };
                }),
            })),
        );
        for (const row of layouts) {
            for (const edge of ["top", "bottom"]) {
                const positions = row.cells.map((cell) => cell[edge]);
                expect(
                    Math.max(...positions) - Math.min(...positions),
                    `${row.name} ${edge} borders at ${width}px`,
                ).toBeLessThanOrEqual(1);
            }
        }
    }
});

test("pending approval actions keep cancel and complete labels visible", async ({
    baseURL,
    page,
}) => {
    const servers = [
        makeServer("approval-host", "pending_approval", makePendingUpdates(15)),
    ];
    await stubDashboardApi(page, () => servers);
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 1922, height: 1176 });
    await page.goto("/");

    const row = page.locator(
        '#servers-table tbody tr[data-name="approval-host"]',
    );
    await expect(row.locator(".timeline-progress-ring")).toContainText("60%");
    await expect(
        row.locator('button[data-action="cancel-upgrade"]'),
    ).toHaveText("Cancel");
    await expect(
        row.locator('button[data-action="approve-security"]'),
    ).toHaveText("Standard security (5)");
    const selectedActions = page.locator(
        "#selected-host-panel .inspector-actions-primary",
    );
    await expect(
        selectedActions.locator('button[data-action="cancel-upgrade"]'),
    ).toHaveText("Cancel");
    await expect(
        selectedActions.locator('button[data-action="approve-security"]'),
    ).toHaveText("Standard security (5)");

    const labelLayout = await page
        .locator(
            [
                '#servers-table tbody tr[data-name="approval-host"] .timeline-actions button[data-action="approve-security"]',
                '#selected-host-panel .inspector-actions-primary button[data-action="approve-security"]',
                '#fleet-tag-list button[aria-label="Show hosts tagged untagged"] span',
            ].join(", "),
        )
        .evaluateAll((elements) =>
            elements.map((element) => ({
                clientWidth: element.clientWidth,
                scrollWidth: element.scrollWidth,
                clientHeight: element.clientHeight,
                scrollHeight: element.scrollHeight,
                textOverflow: getComputedStyle(element).textOverflow,
            })),
        );
    expect(labelLayout).toHaveLength(3);
    for (const label of labelLayout) {
        expect(
            label.scrollWidth,
            "action and tag labels must not clip horizontally",
        ).toBeLessThanOrEqual(label.clientWidth);
        expect(
            label.scrollHeight,
            "action and tag labels must not clip vertically",
        ).toBeLessThanOrEqual(label.clientHeight);
        expect(
            label.textOverflow,
            "action and tag labels must not use an ellipsis",
        ).not.toBe("ellipsis");
    }
});

test("operator pages share one responsive and accessible application shell", async ({
    baseURL,
    page,
}, testInfo) => {
    await ensureAuthenticatedSession(page);
    const pages = [
        ["/", "/", "Status"],
        ["/manage", "/manage", "Manage Servers"],
        ["/observability", "/observability", "Observability"],
        ["/admin", "/admin", "Admin"],
    ];
    const expectedLinks = [
        ["Status", "/"],
        ["Manage Servers", "/manage"],
        ["Observability", "/observability"],
        ["Admin", "/admin"],
        ["Account", "/account"],
    ];

    for (const viewport of [
        { width: 1920, height: 1080 },
        { width: 390, height: 844 },
    ]) {
        await page.setViewportSize(viewport);
        let expectedHeaderHeight = 0;
        for (const [route, currentHref, pageLabel] of pages) {
            await page.goto(route);
            const pageFontFamily = await page
                .locator("body")
                .evaluate((element) => getComputedStyle(element).fontFamily);
            expect(
                pageFontFamily,
                `${route} must use the global Segoe UI font stack`,
            ).toContain("Segoe UI");
            const shell = page.locator(".app-header");
            await expect(shell).toHaveCount(1);
            await expect(shell).toHaveAttribute(
                "aria-label",
                `${pageLabel} application shell`,
            );
            const current = shell.locator('.app-nav a[aria-current="page"]');
            await expect(current).toHaveCount(1);
            await expect(current).toHaveAttribute("href", currentHref);

            const links = shell.locator(".app-nav a");
            await expect(links).toHaveCount(expectedLinks.length);
            for (let index = 0; index < expectedLinks.length; index += 1) {
                await expect(links.nth(index)).toHaveText(
                    expectedLinks[index][0],
                );
                await expect(links.nth(index)).toHaveAttribute(
                    "href",
                    expectedLinks[index][1],
                );
            }

            const box = await shell.boundingBox();
            expect(box).not.toBeNull();
            expect(box.x).toBeGreaterThanOrEqual(0);
            expect(box.x + box.width).toBeLessThanOrEqual(viewport.width + 1);
            if (expectedHeaderHeight === 0) expectedHeaderHeight = box.height;
            expect(
                Math.abs(box.height - expectedHeaderHeight),
            ).toBeLessThanOrEqual(1);

            const hoverTarget = links.nth(currentHref === "/" ? 1 : 0);
            await hoverTarget.hover();
            await expect
                .poll(() =>
                    hoverTarget.evaluate(
                        (element) => getComputedStyle(element).transform,
                    ),
                )
                .not.toBe("none");
            await expect
                .poll(() =>
                    hoverTarget.evaluate(
                        (element) => getComputedStyle(element).boxShadow,
                    ),
                )
                .not.toBe("none");

            await page.mouse.move(0, 0);
            await page.keyboard.press("Tab");
            const focused = page.locator(":focus");
            await expect(focused).toBeVisible();
            const focusShadow = await focused.evaluate(
                (element) => getComputedStyle(element).boxShadow,
            );
            expect(focusShadow).not.toBe("none");

            if (route === "/manage") {
                const screenshot = await page.screenshot({ fullPage: false });
                await testInfo.attach(
                    `manage-${viewport.width}x${viewport.height}`,
                    {
                        body: screenshot,
                        contentType: "image/png",
                    },
                );
                if (process.env.UI_EVIDENCE_DIR) {
                    fs.mkdirSync(process.env.UI_EVIDENCE_DIR, {
                        recursive: true,
                    });
                    fs.writeFileSync(
                        path.join(
                            process.env.UI_EVIDENCE_DIR,
                            `manage-after-${viewport.width}x${viewport.height}.png`,
                        ),
                        screenshot,
                    );
                }
            }
        }
    }

    const browser = page.context().browser();
    const noScriptContext = await browser.newContext({
        baseURL: baseURL,
        javaScriptEnabled: false,
    });
    await noScriptContext.addCookies(await page.context().cookies());
    const noScriptPage = await noScriptContext.newPage();
    await noScriptPage.goto("/observability");
    await noScriptPage.getByRole("link", { name: "Manage Servers" }).click();
    await expect(noScriptPage).toHaveURL(new URL("/manage", page.url()).href);
    await noScriptContext.close();
});

test("an old full-upgrade dialog keeps its approval identity after another tab refreshes the plan", async ({
    baseURL,
    page,
}) => {
    let servers = [
        makeServer(
            "approval-host",
            "pending_approval",
            [{ package: "openssl", security: true }],
            {
                has_password: true,
                job_id: "approval-job",
                approval_generation: 1,
                upgrade_plan: {
                    standard_package_count: 1,
                    full_upgrade_package_count: 1,
                    full_upgrade_plan_available: true,
                    full_upgrade_removed_packages: ["old-obsolete"],
                },
            },
        ),
    ];
    const requests = [];
    const second = await page.context().newPage();
    try {
        for (const tab of [page, second]) {
            await stubDashboardApi(tab, () => servers);
            await tab.route("**/api/jobs/*/logs*", (route) =>
                fulfillJson(route, { fragments: [], has_more: false }),
            );
            await tab.route("**/api/approve-full/approval-host", (route) => {
                const body = route.request().postDataJSON();
                requests.push(body);
                if (
                    body.job_id !== "approval-job" ||
                    body.approval_generation !== servers[0].approval_generation
                ) {
                    return route.fulfill({
                        status: 409,
                        contentType: "application/json",
                        body: JSON.stringify({
                            error: "The pending update changed. Refresh the package plan and confirm again.",
                        }),
                    });
                }
                servers = [
                    {
                        ...servers[0],
                        approval_generation: 2,
                        upgrade_plan: {
                            ...servers[0].upgrade_plan,
                            full_upgrade_removed_packages: [
                                "application-service",
                            ],
                        },
                    },
                ];
                return fulfillJson(route, { message: "Full upgrade approved" });
            });
        }
        await ensureAuthenticatedSession(page);
        await second.goto("/");
        const fullButton = (tab) =>
            tab.locator(
                '#servers-table [data-action="approve-full"][data-name="approval-host"]',
            );
        await fullButton(second).click();
        await expect(second.locator("#action-confirm-message")).toContainText(
            "old-obsolete",
        );
        await fullButton(page).click();
        await page.locator("#action-confirm-submit").click();
        await expect.poll(() => requests.length).toBe(1);
        await second.evaluate(() =>
            fetchServers(true, "approval-generation-test"),
        );
        await expect
            .poll(() =>
                second.evaluate(
                    () =>
                        window.StatusPage.getServerByName("approval-host")
                            ?.approval_generation,
                ),
            )
            .toBe(2);
        await expect(second.locator("#action-confirm-message")).toContainText(
            "old-obsolete",
        );
        await second.locator("#action-confirm-submit").click();
        await expect.poll(() => requests.length).toBe(2);
        expect(requests).toEqual([
            {
                job_id: "approval-job",
                approval_generation: 1,
                confirm_removals: true,
            },
            {
                job_id: "approval-job",
                approval_generation: 1,
                confirm_removals: true,
            },
        ]);
        await expect(second.locator(".app-feedback")).toContainText(
            "pending update changed",
        );
        await fullButton(second).click();
        await expect(second.locator("#action-confirm-message")).toContainText(
            "application-service",
        );
        await second.locator("#action-confirm-cancel").click();
    } finally {
        await second.close();
    }
});

test("package deferrals 473 approval shows executable transactions CVE exposure and frozen rule identity", async ({
    baseURL,
    page,
}) => {
    const revision = "a".repeat(64),
        rules = [
            {
                id: "rule",
                selector: "openssl:amd64",
                reason: "Compatibility investigation",
                origin: "Policy: Production (revision 3)",
                author: "fixture-admin",
                expires_at: "2030-11-10T17:00:00Z",
                expires_at_display: "2030-11-10 12:00:00 EST",
            },
        ];
    const safe = {
        available: true,
        selected_packages: ["curl:amd64"],
        transaction: ["curl:amd64=2"],
        new_packages: [],
        removed_packages: [],
        dependency_impacts: [],
    };
    const plan = {
        standard_package_count: 1,
        standard_security_count: 1,
        total_security_count: 2,
        full_upgrade_package_count: 1,
        full_upgrade_plan_available: true,
        full_upgrade_removed_packages: ["obsolete-runtime"],
        package_deferrals: { revision, rules },
        deferred_packages: ["openssl:amd64"],
        deferred_security_packages: ["openssl:amd64"],
        deferral_scopes: {
            all: safe,
            security: {
                ...safe,
                available: false,
                block_reason:
                    "Dependency conflict: APT would change deferred package openssl:amd64",
            },
            security_kept_back: {
                ...safe,
                selected_packages: [],
                transaction: [],
            },
            full_upgrade: {
                ...safe,
                removed_packages: ["obsolete-runtime"],
                transaction: ["curl:amd64=2", "obsolete-runtime:amd64=-"],
            },
        },
    };
    let servers = [
        makeServer(
            "deferral-host",
            "pending_approval",
            [
                {
                    package: "openssl:amd64",
                    current_version: "1.0",
                    candidate_version: "2.0",
                    security: true,
                    deferred: true,
                    cve_state: "ready",
                    cve_findings: [
                        {
                            id: "CVE-2026-1000",
                            disposition: "fixed_by_candidate",
                            fixed_by_version: "2.0",
                            source: "Debian Security Tracker",
                        },
                    ],
                    cves: ["CVE-2026-1000"],
                },
                {
                    package: "curl:amd64",
                    current_version: "1.0",
                    candidate_version: "2.0",
                    security: true,
                    cve_state: "ready",
                    cves: [],
                },
            ],
            {
                has_password: true,
                job_id: "deferral-job",
                approval_generation: 1,
                upgrade_plan: plan,
            },
        ),
    ];
    await stubDashboardApi(page, () => servers);
    const requests = [];
    await page.route("**/api/approve-full/deferral-host", (route) => {
        requests.push(route.request().postDataJSON());
        return fulfillJson(
            route,
            {
                error: "The pending update changed. Refresh the package plan and confirm again.",
            },
            409,
        );
    });
    await ensureAuthenticatedSession(page);
    await page
        .locator(
            '#servers-table [data-action="open-drawer"][data-tab="pending"][data-name="deferral-host"]',
        )
        .click();
    const panel = page.locator("#drawer-panel-pending");
    await expect(panel).toContainText("Deferred — security exposure remains");
    await expect(panel).toContainText("Dependency conflict");
    await expect(panel).toContainText("CVE");
    await expect(panel).toContainText("Install curl:amd64 version 2");
    await panel.locator("details.cve-findings summary").click();
    await expect(panel).toContainText("CVE-2026-1000");
    await captureDeferral473(
        page,
        panel.locator(".pending-updates"),
        "desktop-approval-affected.png",
    );
    await captureDeferral473(
        page,
        panel.locator(".pending-updates"),
        "mobile-approval-affected.png",
        true,
    );
    await page.locator("#status-drawer-close").click();
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page
        .locator(
            '#servers-table [data-action="approve-full"][data-name="deferral-host"]',
        )
        .click();
    servers = [
        {
            ...servers[0],
            approval_generation: 2,
            upgrade_plan: {
                ...plan,
                package_deferrals: { revision: "b".repeat(64), rules: [] },
            },
        },
    ];
    await page.evaluate(() => fetchServers(true, "deferral-drift"));
    await page.locator("#action-confirm-submit").click();
    await expect.poll(() => requests.length).toBe(1);
    expect(requests[0]).toEqual({
        job_id: "deferral-job",
        approval_generation: 1,
        deferral_revision: revision,
        confirm_removals: true,
    });
    await expect(page.locator(".app-feedback")).toContainText(
        "pending update changed",
    );
});

test("distribution qualification explains a critical CVSS source match without confirming exposure", async ({ page }) => {
    const server=makeServer("triage-host","done");
    await stubDashboardApi(page,()=>[server]);
    await page.route("**/api/dashboard/summary*",route=>{
        const summary=makeDashboardSummary([server]);
        summary.servers[0].risk={level:"unknown",summary:"1 reported CVE · 0 vendor-confirmed source matches · 1 vendor-qualified",vulnerability:{
            state:"complete",freshness:"fresh",result:"qualified_findings",inventoried:1,evaluated:1,unknown:0,unsupported:0,
            reported_cves:1,confirmed_cves:0,needs_review_cves:0,vendor_qualified_cves:1,withdrawn_cves:0,occurrences:1,
            findings:[{id:"CVE-2007-2243",package:"example-client",severity:"critical",disposition:"no_fix_available",qualification:"vendor_qualified",assessment_freshness:"fresh",assessment:{
                vendor_status:"unimportant",vendor_priority:"unimportant",applicability:"source_match",confidence:"vendor_qualified",
                vendor_reason:"Vendor triage retained <not executable>",ecosystem:"Debian:13",source_package:"openssh",source_version:"1:1.0-1",
                source:"https://example.invalid/advisory",modified_at:"2026-10-01T00:00:00Z",observed_at:"2026-10-02T00:00:00Z"
            }}]
        }};
        return fulfillJson(route,summary);
    });
    await ensureAuthenticatedSession(page);
    const section=page.getByRole("region",{name:"Installed package vulnerabilities"});
    await expect(section).toContainText("1 reported CVEs · 0 vendor-confirmed source matches");
    await section.locator("summary").click();
    await expect(section).toContainText("Vendor-qualified: inspect rationale");
    await expect(section).toContainText("Distribution triage: unimportant");
    await expect(section).toContainText("Vendor triage retained <not executable>");
    await expect(section).toContainText("Severity: critical");
    await expect(section).toContainText("Debian:13 / openssh 1:1.0-1");
    await expect(section).not.toContainText("Confirmed exposure");
    await page.setViewportSize({width:390,height:844});
    await expect(section).toContainText("Vendor-qualified: inspect rationale");
});
