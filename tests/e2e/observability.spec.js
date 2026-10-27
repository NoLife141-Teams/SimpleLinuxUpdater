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

test("observability keeps a successful summary when health trends fail", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            totals: { updates_total: 4, success_rate_pct: 75 },
            duration: {
                avg_ms: 1250,
                samples_with_duration: 3,
                samples_without_duration: 1,
            },
            failure_causes: [],
            status_breakdown: [],
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        route.fulfill({
            status: 503,
            contentType: "application/json",
            body: JSON.stringify({ error: "temporarily unavailable" }),
        }),
    );

    await page.goto("/observability");
    await expect(page.locator("#kpi-total")).toHaveText("4");
    await expect(page.locator("#kpi-success-rate")).toContainText("75.00%");
    await expect(page.locator("#error-banner")).toContainText(
        "Health trends is unavailable (HTTP 503)",
    );
    await expect(page.locator("#summary-lifecycle")).toContainText("Current");
    await expect(page.locator("#trends-lifecycle")).toContainText(
        "Unavailable",
    );
    await expect(
        page
            .locator("#trends-lifecycle")
            .getByRole("button", { name: "Retry" }),
    ).toBeVisible();
    await expect(page.locator("#trend-hosts")).toHaveText("Unavailable");
    await expect(page.locator("#health-trends-body")).toContainText(
        "Host health unavailable",
    );
});

test("observability keeps successful health trends when the summary is unavailable", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        route.fulfill({
            status: 503,
            contentType: "application/json",
            body: JSON.stringify({ error: "temporarily unavailable" }),
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, {
            window: "7d",
            from: "2026-07-22T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: { servers_with_samples: 1, samples: 1 },
            servers: [
                {
                    name: "healthy-host",
                    samples: 1,
                    latest: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1048576,
                        apt_status: "ok",
                        disk_status: "ok",
                    },
                    first: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1048576,
                    },
                    points: [],
                },
            ],
        }),
    );

    await page.goto("/observability");
    await expect(page.locator("#summary-lifecycle")).toContainText(
        "Unavailable",
    );
    await expect(page.locator("#trends-lifecycle")).toContainText("Current");
    await expect(page.locator("#kpi-success-rate")).toHaveText("Unavailable");
    await expect(page.locator("#kpi-duration")).toHaveText("Unavailable");
    await expect(page.locator("#health-trends-body")).toContainText(
        "healthy-host",
    );
});

test("observability projects source lifecycle, fleet severity, confidence, and missing health facts", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:37Z",
            to: "2026-07-29T12:00:45Z",
            totals: {
                updates_total: 4,
                updates_success: 3,
                updates_failure: 1,
                success_rate_pct: 75,
            },
            duration: {
                avg_ms: 1250,
                samples_with_duration: 1,
                samples_without_duration: 3,
            },
            failure_causes: [
                { cause: "unknown", count: 1, servers: ["demo-host"] },
            ],
            status_breakdown: [
                { status: "success", count: 3 },
                { status: "failure", count: 1 },
            ],
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: {
                servers_with_samples: 0,
                samples: 0,
                update_failures: 0,
                scan_failures: 0,
            },
            servers: [
                {
                    name: "demo-host",
                    samples: 0,
                    last_observation: {
                        captured_at: "2026-07-26T11:00:00Z",
                        disk_free_kb: 0,
                        apt_status: "ok",
                        disk_status: "unknown",
                    },
                    update_failures: 0,
                    scan_failures: 0,
                    points: [],
                },
            ],
        }),
    );

    await page.goto("/observability?window=24h");
    await expect(page.locator("#summary-lifecycle")).toContainText("Current");
    await expect(page.locator("#trends-lifecycle")).toContainText("Current");
    await expect(
        page.locator("#summary-lifecycle .source-lifecycle-time"),
    ).toContainText("2026");
    await expect(
        page.locator("#summary-lifecycle .source-lifecycle-time"),
    ).toContainText("ago");
    await expect(
        page.locator("#trends-lifecycle .source-lifecycle-time"),
    ).toContainText("2026");
    await expect(
        page.locator("#trends-lifecycle .source-lifecycle-time"),
    ).toContainText("ago");
    await expect(page.locator("#kpi-success-rate-card")).toContainText(
        "Critical",
    );
    await expect(page.locator("#kpi-success-icon")).toHaveText("×");
    await expect(page.locator("#kpi-success-rate-card")).toContainText(
        "3 successful",
    );
    await expect(page.locator("#kpi-success-rate-card")).toContainText(
        "1 failed",
    );
    await expect(page.locator("#kpi-duration-card")).toContainText(
        "Low confidence",
    );
    await expect(page.locator("#kpi-duration-card")).toContainText(
        "1 of 4 runs",
    );
    await expect(page.locator("#failure-causes-body")).toContainText(
        "Data quality issue",
    );
    await expect(page.locator("#failure-causes-body")).toContainText(
        "Affected: demo-host",
    );
    await expect(
        page.locator('#failure-causes-body a[href*="audit_target=demo-host"]'),
    ).toHaveAttribute("href", /audit_status=failure/);
    await expect(page.locator("#health-trends-body")).toContainText(
        "Unavailable",
    );
    await expect(page.locator("#health-trends-body")).toContainText("Stale");
    await expect(page.locator("#health-trends-body")).not.toContainText(
        "119231880",
    );
    await expect(
        page.locator("#health-trends-body .health-signal-badge").nth(1),
    ).toHaveClass(/status-unknown/);
    const failureCauseLink = page
        .locator('#failure-causes-body a[href*="failure_cause=unknown"]')
        .first();
    await expect(failureCauseLink).toHaveAttribute(
        "href",
        /audit_from=2026-07-28T12%3A00%3A37Z/,
    );
    await expect(failureCauseLink).toHaveAttribute(
        "href",
        /audit_to=2026-07-29T12%3A00%3A45Z/,
    );
    await expect(failureCauseLink).toHaveAttribute("href", /#audit-trail$/);
    const causeRequest = page.waitForRequest(
        (request) =>
            request.url().includes("/api/audit-events?") &&
            new URL(request.url()).searchParams.get("failure_cause") ===
                "unknown" &&
            new URL(request.url()).searchParams.get("from") ===
                "2026-07-28T12:00:37Z" &&
            new URL(request.url()).searchParams.get("to") ===
                "2026-07-29T12:00:45Z",
    );
    await page
        .locator('#failure-causes-body a[href*="audit_target=demo-host"]')
        .click();
    await causeRequest;
    await expect(page).toHaveURL(/\/manage\?/);
    await expect(page).toHaveURL(/failure_cause=unknown/);
    await expect(page.locator("#audit-target-filter")).toHaveValue("demo-host");
    await expect(page.locator("#audit-action-filter")).toHaveValue(
        "update.complete",
    );
    await expect(page.locator("#audit-status-filter")).toHaveValue("failure");
    await expect(page.locator("#audit-failure-cause-active")).toContainText(
        "unknown",
    );
    const clearedCauseRequest = page.waitForRequest(
        (request) =>
            request.url().includes("/api/audit-events?") &&
            !new URL(request.url()).searchParams.has("failure_cause"),
    );
    await page.locator("#audit-clear-failure-cause").click();
    await clearedCauseRequest;
    await expect(page.locator("#audit-failure-cause-active")).toBeHidden();
    await expect(page).not.toHaveURL(/failure_cause=/);
});

test("observability leaves unavailable disk intervals unconnected", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            totals: {
                updates_total: 0,
                updates_success: 0,
                updates_failure: 0,
                success_rate_pct: 0,
            },
            duration: {
                avg_ms: 0,
                samples_with_duration: 0,
                samples_without_duration: 0,
            },
            failure_causes: [],
            status_breakdown: [],
        }),
    );
    const points = [
        { captured_at: "2026-07-29T08:00:00Z", disk_free_kb: 1024 },
        { captured_at: "2026-07-29T09:00:00Z", disk_free_kb: 896 },
        { captured_at: "2026-07-29T10:00:00Z", disk_free_kb: 0 },
        { captured_at: "2026-07-29T11:00:00Z", disk_free_kb: 768 },
        { captured_at: "2026-07-29T12:00:00Z", disk_free_kb: 640 },
    ];
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: { servers_with_samples: 1, samples: points.length },
            servers: [
                {
                    name: "disk-gap-host",
                    samples: points.length,
                    first: points[0],
                    latest: points.at(-1),
                    points,
                },
            ],
        }),
    );

    await page.goto("/observability?window=24h");
    await expect(page.locator("#kpi-duration")).toHaveText("No data");
    await expect(page.locator("#disk-trend-chart .trend-line")).toHaveCount(2);
    await expect(page.locator("#disk-trend-chart .trend-point")).toHaveCount(4);
});

test("observability CSV exports neutralize spreadsheet formulas", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            totals: {
                updates_total: 0,
                updates_success: 0,
                updates_failure: 0,
                success_rate_pct: 0,
            },
            duration: {
                avg_ms: 0,
                samples_with_duration: 0,
                samples_without_duration: 0,
            },
            failure_causes: [],
            status_breakdown: [],
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: { servers_with_samples: 2, samples: 2 },
            servers: [
                {
                    name: '=HYPERLINK("https://example.invalid")',
                    samples: 1,
                    first: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    latest: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    points: [
                        {
                            captured_at: "2026-07-29T11:00:00Z",
                            disk_free_kb: 1024,
                        },
                    ],
                },
                {
                    name: "safe\r=2+2",
                    samples: 1,
                    first: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    latest: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    points: [
                        {
                            captured_at: "2026-07-29T11:00:00Z",
                            disk_free_kb: 1024,
                        },
                    ],
                },
            ],
        }),
    );

    await page.goto("/observability?window=24h");
    const downloadPromise = page.waitForEvent("download");
    await page.locator("#export-health-csv").click();
    const download = await downloadPromise;
    const csv = fs.readFileSync(await download.path(), "utf8");
    expect(csv).toContain(`"'=HYPERLINK(""https://example.invalid"")"`);
    expect(csv).not.toContain(`"=HYPERLINK(""https://example.invalid"")"`);
    expect(csv).toContain('"safe\r=2+2"');
});

test("observability renders an unchanged disk delta without an increase label", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            totals: {
                updates_total: 0,
                updates_success: 0,
                updates_failure: 0,
                success_rate_pct: 0,
            },
            duration: {
                avg_ms: 0,
                samples_with_duration: 0,
                samples_without_duration: 0,
            },
            failure_causes: [],
            status_breakdown: [],
        }),
    );
    await page.route("**/api/observability/health-trends*", (route) =>
        fulfillJson(route, {
            window: "24h",
            from: "2026-07-28T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: { servers_with_samples: 1, samples: 2 },
            servers: [
                {
                    name: "steady-disk",
                    samples: 2,
                    first: {
                        captured_at: "2026-07-28T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    latest: {
                        captured_at: "2026-07-29T11:00:00Z",
                        disk_free_kb: 1024,
                    },
                    disk_free_delta_kb: 0,
                    points: [],
                },
            ],
        }),
    );

    await page.goto("/observability?window=24h");
    const diskCell = page
        .locator("#health-trends-body tr")
        .filter({ hasText: "steady-disk" })
        .locator("td")
        .nth(4);
    await expect(diskCell).toHaveText("1 MB (unchanged)");
    await expect(diskCell).not.toContainText("increase");
});

test("observability charts, filters, shareable URL, pagination, and CSV scale an investigation", async ({
    baseURL,
    page,
}) => {
    await page.route("**/api/app-settings/timezone", (route) =>
        fulfillJson(route, {
            timezone: "America/Toronto",
            resolved_timezone: "America/Toronto",
            editable_timezone: "America/Toronto",
        }),
    );
    await ensureAuthenticatedSession(page);
    await page.route("**/api/observability/summary*", (route) =>
        fulfillJson(route, {
            window: "7d",
            from: "2026-07-22T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            totals: {
                updates_total: 10,
                updates_success: 9,
                updates_failure: 1,
                success_rate_pct: 90,
            },
            duration: {
                avg_ms: 800,
                samples_with_duration: 10,
                samples_without_duration: 0,
            },
            failure_causes: [
                {
                    cause: "retry_exhausted",
                    count: 1,
                    servers: ["prod-failing"],
                },
            ],
            status_breakdown: [
                { status: "success", count: 9 },
                { status: "failure", count: 1 },
            ],
        }),
    );
    const servers = Array.from({ length: 28 }, (_, index) => ({
        name:
            index === 0
                ? "prod-failing"
                : `host-${String(index).padStart(2, "0")}`,
        samples: 2,
        latest: {
            captured_at: "2026-07-29T11:00:00Z",
            package_count: index,
            security_count: index % 3,
            disk_free_kb: 10485760 - index * 1024,
            apt_status: "ok",
            disk_status: "ok",
        },
        first: {
            captured_at: "2026-07-28T11:00:00Z",
            package_count: Math.max(0, index - 1),
            security_count: 0,
            disk_free_kb: 11534336,
        },
        package_delta: 2,
        security_delta: index % 3,
        disk_free_delta_kb: -1048576,
        update_failures: index === 0 ? 2 : 0,
        scan_failures: 0,
        apt_problem_samples: 0,
        disk_problem_samples: 0,
        reboot_seen: false,
        points: [
            {
                captured_at: "2026-07-28T11:00:00Z",
                package_count: 1,
                security_count: 0,
                disk_free_kb: 11534336,
                last_update_status: "success",
            },
            {
                captured_at: "2026-07-29T11:00:00Z",
                package_count: 2,
                security_count: 1,
                disk_free_kb: 10485760,
                last_update_status: index === 0 ? "failure" : "success",
            },
        ],
    }));
    let healthTrendRequestCount = 0;
    await page.route("**/api/observability/health-trends*", (route) => {
        healthTrendRequestCount += 1;
        const responseServers =
            healthTrendRequestCount === 2
                ? servers.map((server) => ({
                      ...server,
                      points: server.points.slice(1),
                  }))
                : servers;
        return fulfillJson(route, {
            window: "7d",
            from: "2026-07-22T12:00:00Z",
            to: "2026-07-29T12:00:00Z",
            generated_at: "2026-07-29T12:00:00Z",
            retention_days: 90,
            fleet: {
                servers_with_samples: 28,
                samples: 56,
                update_failures: 2,
                scan_failures: 0,
            },
            servers: responseServers,
        });
    });

    await page.goto("/observability?window=7d");
    await expect(page.locator("#observability-last-refresh")).toContainText(
        "2026",
    );
    await expect(page.locator("#observability-last-refresh")).toContainText(
        "ago",
    );
    await expect(
        page.getByRole("group", { name: /Package count trend/ }),
    ).toBeVisible();
    await expect(
        page.getByRole("group", { name: /Disk free trend/ }),
    ).toBeVisible();
    await expect(
        page.locator("#package-trend-chart .trend-axis-unit"),
    ).toHaveText("packages");
    await expect(page.locator("#disk-trend-chart .trend-axis-unit")).toHaveText(
        "disk free",
    );
    await expect(
        page.locator("#package-trend-chart .trend-y-axis-label"),
    ).toHaveCount(3);
    await expect(
        page.locator("#package-trend-chart .trend-x-axis-label"),
    ).toHaveCount(3);
    await expect(
        page.locator("#package-trend-chart .trend-x-axis-label"),
    ).toHaveText(["Jul 28", "Jul 28", "Jul 29"]);
    await expect(page.locator("#package-trend-chart .trend-point")).toHaveCount(
        2,
    );
    await expect(
        page.getByRole("group", { name: /Package count trend/ }),
    ).toHaveAttribute("aria-label", /2 time points/);
    const packageLinePoints = await page
        .locator("#package-trend-chart .trend-line")
        .getAttribute("points");
    expect(packageLinePoints.trim().split(/\s+/)).toHaveLength(3);
    await expect(page.locator(".trend-chart-scope")).toHaveText([
        "Fleet total",
        "Fleet total",
        "Fleet total",
        "Fleet total",
    ]);
    const packagePoint = page
        .locator("#package-trend-chart .trend-point")
        .first();
    await expect(packagePoint).toHaveAttribute(
        "aria-label",
        /exact observation Jul 28, 2026, 07:00 EDT/,
    );
    await packagePoint.hover();
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toBeVisible();
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).not.toHaveAttribute("style", /.+/);
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("Fleet total");
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("28 packages");
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("Jul 28, 2026, 07:00 EDT");
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("Daily bucket");
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("28 hosts represented");
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toContainText("28 observations");
    const lastPackagePoint = page
        .locator("#package-trend-chart .trend-point")
        .last();
    await lastPackagePoint.hover();
    const tooltipBox = await page
        .locator("#package-trend-chart .trend-tooltip")
        .boundingBox();
    const chartBox = await page.locator("#package-trend-chart").boundingBox();
    expect(tooltipBox.x).toBeGreaterThanOrEqual(chartBox.x);
    expect(tooltipBox.x + tooltipBox.width).toBeLessThanOrEqual(
        chartBox.x + chartBox.width,
    );
    await page.locator("#health-search").focus();
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toBeHidden();
    await packagePoint.focus();
    await expect(
        page.locator("#package-trend-chart .trend-tooltip"),
    ).toBeVisible();
    const focusedPointKey = await packagePoint.getAttribute(
        "data-observability-focus-key",
    );
    expect(focusedPointKey).toBeTruthy();
    await page.evaluate(() => {
        executeEffects(
            observabilityInteraction.dispatch({ type: "manualRefresh" }),
        );
    });
    await expect(
        page.locator(`[data-observability-focus-key="${focusedPointKey}"]`),
    ).toHaveCount(0);
    await expect(
        page.locator("#package-trend-chart .trend-point"),
    ).toBeFocused();
    await page.locator("#health-trend-server").selectOption("prod-failing");
    await expect(page.locator(".trend-chart-scope")).toHaveText([
        "prod-failing",
        "prod-failing",
        "prod-failing",
        "prod-failing",
    ]);
    await page.locator("#health-trend-server").selectOption("");
    await expect(page.locator(".trend-chart-scope")).toHaveText([
        "Fleet total",
        "Fleet total",
        "Fleet total",
        "Fleet total",
    ]);
    await expect(page.locator("#failure-breakdown-bars progress")).toHaveCount(
        1,
    );
    await expect(
        page.locator(
            '#failure-causes-body a[href*="audit_target=prod-failing"]',
        ),
    ).toHaveAttribute("href", /audit_status=failure/);
    await expect(page.locator("#status-breakdown-bars progress")).toHaveCount(
        2,
    );
    await expect(page.locator("#health-result-count")).toContainText(
        "25 of 28",
    );
    await expect(page.locator("#health-trends-body tr")).toHaveCount(25);
    const failingHostRow = page
        .locator("#health-trends-body tr")
        .filter({ hasText: "prod-failing" });
    await expect(failingHostRow).toContainText("increase of 2 packages");
    await expect(failingHostRow).toContainText("10.0 GB");
    await page.locator("#health-next-page").click();
    await expect(page).toHaveURL(/page=2/);
    await expect(page.locator("#health-result-count")).toContainText("3 of 28");

    await page.locator("#health-search").fill("prod");
    await page.locator("#health-attention-filter").selectOption("failures");
    await expect(page).toHaveURL(/search=prod/);
    await expect(page).toHaveURL(/attention=failures/);
    await expect(page.locator("#health-trends-body")).toContainText(
        "prod-failing",
    );
    await expect(page.locator("#health-trends-body tr")).toHaveCount(1);
    await expect(page.locator("#health-trends-body a")).toHaveAttribute(
        "href",
        /\/manage\?server=prod-failing#server-directory$/,
    );

    const downloadPromise = page.waitForEvent("download");
    await page.locator("#export-health-csv").click();
    const download = await downloadPromise;
    expect(download.suggestedFilename()).toMatch(
        /^observability-health-7d\.csv$/,
    );
    const healthCSV = fs.readFileSync(await download.path(), "utf8");
    const healthCSVLines = healthCSV.trim().split("\n");
    expect(healthCSVLines).toHaveLength(3);
    expect(healthCSVLines[0]).toContain("Captured at app time,Captured at UTC");
    expect(healthCSVLines[1]).toContain("prod-failing");
    expect(healthCSVLines[1]).toContain("2026-07-28T11:00:00Z");
    expect(healthCSVLines[2]).toContain("prod-failing");
    expect(healthCSVLines[2]).toContain("2026-07-29T11:00:00Z");
    const failuresDownloadPromise = page.waitForEvent("download");
    await page.locator("#export-failures-csv").click();
    const failuresDownload = await failuresDownloadPromise;
    expect(failuresDownload.suggestedFilename()).toBe(
        "observability-failures-7d.csv",
    );

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
        bodyWidth: document.body.scrollWidth,
        viewportWidth: document.documentElement.clientWidth,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(
        mobileLayout.viewportWidth,
    );
    await expect(page.locator("#health-scroll-hint")).toBeVisible();

    await page.goto("/manage?server=prod-failing#server-directory");
    await expect(page.locator("#search")).toHaveValue("prod-failing");
    await expect(page).toHaveURL(/server=prod-failing#server-directory$/);
});
