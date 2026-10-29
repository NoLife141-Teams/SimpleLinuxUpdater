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

test("policy schedules 472 monthly explicit canaries preserve drafts and server projections", async ({
    baseURL,
    page,
}) => {
    const state = {
        policyPreviewResponse: {
            matched_servers: [
                { name: "srv-web-01", rollout_stage: "wave", wave: 1 },
                { name: "srv-web-02", rollout_stage: "canary", wave: 0 },
            ],
            excluded_servers: [],
            disabled_by_override: [],
            validation_errors: [],
            operational_warnings: [],
            informational_facts: [
                {
                    code: "month_day_missing",
                    message:
                        "2028-02 has no day 31; this month is skipped without moving the occurrence.",
                },
            ],
            upcoming_occurrences: [
                {
                    local_civil_time: "2028-03-31 03:45",
                    timezone: "America/Toronto",
                    offset: "-04:00",
                    abbreviation: "EDT",
                    scheduled_for_utc: "2028-03-31T07:45:00.000000000Z",
                    dst_status: "unambiguous",
                    canonical_choice: "exact",
                    matched_server_count: 2,
                    applicable_no_run_windows: [],
                    admission_outcome: "admitted",
                },
            ],
            schedule_conflicts: [],
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.route("**/api/update-policies/calendar?*", (route) =>
        fulfillJson(route, {
            days: 1,
            start_date: "2028-02-29",
            end_date: "2028-02-29",
            timezone: "America/Toronto",
            accepted_rollouts: [
                {
                    policy: {
                        id: 42,
                        name: "Accepted security rollout",
                        revision: 3,
                    },
                    scheduled_for_utc: "2028-01-31T08:45:00Z",
                    batches: [
                        { index: 0, stage: "canary", servers: ["srv-web-02"] },
                        {
                            index: 1,
                            stage: "wave",
                            servers: ["srv-web-01"],
                            release_delay_minutes: 15,
                        },
                    ],
                },
            ],
            policies: [
                {
                    id: 42,
                    name: "Month-end security waves",
                    enabled: true,
                    cadence_kind: "monthly",
                    time_local: "03:45",
                    month_day: 31,
                    matched_servers: ["srv-web-01", "srv-web-02"],
                    days: [
                        {
                            date: "2028-02-29",
                            weekday: "tue",
                            timezone_offset: "-05:00",
                            allowed_slots: [],
                            blocked_windows: [],
                            blocked_reasons: ["month_day_missing"],
                        },
                    ],
                },
            ],
        }),
    );
    await page.route("**/api/update-policies/runs?*", (route) =>
        fulfillJson(route, {
            items: [
                {
                    id: 472,
                    policy_name: "Accepted security rollout",
                    policy_revision: 3,
                    rollout_stage: "wave",
                    wave: 1,
                    server_name: "srv-web-01",
                    scheduled_for_utc: "2028-01-31T08:45:00Z",
                    status: "skipped",
                    exact_skip_reason: "rollout_gate",
                },
            ],
            total: 1,
            page: 1,
            total_pages: 1,
            page_size: 25,
        }),
    );
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "This month has no selected day; occurrence skipped",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "accepted revision 3",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "Canary: srv-web-02",
    );
    await page.locator("#policy-name").fill("Month-end security waves");
    await page.locator("#policy-target-tag").fill("prod");
    await page.locator("#policy-cadence-kind").selectOption("monthly");
    await page.locator("#policy-month-day").fill("31");
    await page.locator("#policy-time-local").fill("03:45");
    await page.locator("#policy-rollout-mode").selectOption("canary_waves");
    await page.locator("#policy-canary-mode").selectOption("explicit");
    await page.locator("#policy-canary-servers").fill("srv-web-02");
    await page.locator("#policy-wave-size").fill("1");
    await expect(page.locator("#policy-preview-matched")).toContainText(
        "srv-web-02 · canary",
    );
    await expect(page.locator("#policy-preview-facts")).toContainText(
        "2028-02 has no day 31",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "UTC 2028-03-31T07:45:00.000000000Z",
    );
    await expect(page.locator("#policy-canary-count")).toBeHidden();
    await expect(page.locator("#policy-date-local")).toBeHidden();
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await expect(page.locator("#scheduled-runs-table")).toContainText(
        "Revision 3 · wave 1",
    );
    await expect(page.locator("#scheduled-runs-table")).toContainText(
        "Reason: rollout_gate",
    );
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(page.locator("#policy-month-day")).toHaveValue("31");
    await expect(page.locator("#policy-canary-servers")).toHaveValue(
        "srv-web-02",
    );
    await page.locator("#policy-month-last-day").check();
    await expect(page.locator("#policy-month-day")).toBeDisabled();
    await expect(page.locator("#policy-summary")).toContainText(
        "Monthly on the last day",
    );
    await page.locator("#policy-month-last-day").uncheck();
    if (process.env.CAPTURE_POLICY_SCHEDULES_472) {
        await capturePolicy472Panel(
            page,
            page.locator("#policy-preview"),
            "desktop-monthly-preview.png",
        );
        await capturePolicy472Panel(
            page,
            page.locator("#update-policy-form .form-section").nth(1),
            "desktop-monthly-schedule.png",
        );
        await capturePolicy472Panel(
            page,
            page.locator("#update-policy-form .form-section").nth(2),
            "desktop-explicit-canary-controls.png",
        );
        await capturePolicy472Panel(
            page,
            page.locator("#update-policy-form .form-section").nth(1),
            "mobile-monthly-schedule.png",
            true,
        );
        const layout = await page.evaluate(() => ({
            viewport: innerWidth,
            width: document.body.scrollWidth,
        }));
        expect(layout.width).toBeLessThanOrEqual(layout.viewport);
    }
    await page.locator("#policy-save-btn").click();
    await expect
        .poll(() => state.policyPayload)
        .toMatchObject({
            cadence_kind: "monthly",
            month_day: 31,
            month_last_day: false,
            canary_mode: "explicit",
            canary_servers: ["srv-web-02"],
            canary_count: 1,
        });
});

test("policy schedules 472 one-time draft submits civil date and shows canary blocking", async ({
    baseURL,
    page,
}) => {
    const state = {
        policyPreviewResponse: {
            matched_servers: [
                { name: "srv-web-01", rollout_stage: "wave", wave: 1 },
            ],
            excluded_servers: [],
            disabled_by_override: [],
            validation_errors: [],
            operational_warnings: [
                {
                    code: "canary_unavailable",
                    message: "canary_unavailable: srv-missing (missing)",
                },
            ],
            informational_facts: [],
            schedule_conflicts: [],
            upcoming_occurrences: [
                {
                    local_civil_time: "2028-11-05 01:30",
                    timezone: "America/Toronto",
                    offset: "-04:00",
                    abbreviation: "EDT",
                    scheduled_for_utc: "2028-11-05T05:30:00.000000000Z",
                    dst_status: "ambiguous",
                    canonical_choice: "earlier_fallback_occurrence",
                    matched_server_count: 1,
                    applicable_no_run_windows: [],
                    admission_outcome: "canary_unavailable",
                },
            ],
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await page.locator("#policy-name").fill("One-time fallback maintenance");
    await page.locator("#policy-target-tag").fill("prod");
    await page.locator("#policy-cadence-kind").selectOption("once");
    await page.locator("#policy-date-local").fill("2028-11-05");
    await page.locator("#policy-time-local").fill("01:30");
    await page.locator("#policy-rollout-mode").selectOption("canary_waves");
    await page.locator("#policy-canary-mode").selectOption("explicit");
    await page.locator("#policy-canary-servers").fill("srv-missing");
    await expect(page.locator("#policy-once-help")).toContainText(
        "never rearms",
    );
    await expect(page.locator("#policy-preview-warnings")).toContainText(
        "srv-missing (missing)",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "Blocked: explicit canary unavailable",
    );
    await expect(page.locator("#policy-summary")).toContainText(
        "Once on 2028-11-05 at 01:30",
    );
    if (process.env.CAPTURE_POLICY_SCHEDULES_472) {
        await capturePolicy472Panel(
            page,
            page.locator("#policy-preview"),
            "desktop-once-blocked-preview.png",
        );
        await capturePolicy472Panel(
            page,
            page.locator("#update-policy-form .form-section").nth(1),
            "mobile-once-schedule.png",
            true,
        );
    }
    await page.locator("#policy-save-btn").click();
    await expect
        .poll(() => state.policyPayload)
        .toMatchObject({
            cadence_kind: "once",
            date_local: "2028-11-05",
            time_local: "01:30",
            canary_servers: ["srv-missing"],
        });
});

test("policy schedules 472 consumed one-time status stays visible through cadence edits", async ({
    baseURL,
    page,
}) => {
    const state = {
        policies: [
            {
                id: 12,
                name: "Consumed one-time maintenance",
                enabled: true,
                target_tag: "prod",
                cadence_kind: "once",
                date_local: "2026-10-25",
                time_local: "03:00",
                execution_mode: "scan_only",
                package_scope: "security",
                once_consumed_at: "2026-10-25T07:00:00Z",
                matched_servers: ["srv-web-01"],
            },
        ],
        policyPreviewResponse: {
            matched_servers: [{ name: "srv-web-01" }],
            excluded_servers: [],
            disabled_by_override: [],
            validation_errors: [],
            operational_warnings: [],
            informational_facts: [
                {
                    code: "once_consumed",
                    message:
                        "One-time occurrence consumed at 2026-10-25T07:00:00Z. Editing date/time or timezone does not rearm this policy; create a new policy for another operation.",
                },
            ],
            upcoming_occurrences: [],
            schedule_conflicts: [],
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(page.locator("#scheduled-policy-table")).toContainText(
        "One-time occurrence consumed",
    );
    await page.locator('[data-action="edit-policy"][data-id="12"]').click();
    await expect(page.locator("#policy-date-local")).toHaveValue("2026-10-25");
    await expect(page.locator("#policy-summary")).toContainText(
        "One-time occurrence consumed",
    );
    await page.locator("#policy-cadence-kind").selectOption("daily");
    await expect(page.locator("#policy-summary")).toContainText(
        "create a new policy",
    );
    await expect(page.locator("#policy-preview-facts")).toContainText(
        "does not rearm",
    );
});

test("policy schedules 472 calendar retains accepted rollouts after the last policy is deleted", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, { policies: [] });
    await page.route("**/api/update-policies/calendar?*", (route) =>
        fulfillJson(route, {
            days: 14,
            start_date: "2026-10-25",
            end_date: "2026-11-07",
            timezone: "UTC",
            policies: [],
            accepted_rollouts: [
                {
                    policy: {
                        id: 42,
                        name: "Deleted policy with pending wave",
                        revision: 3,
                    },
                    scheduled_for_utc: "2026-10-25T03:00:00Z",
                    batches: [
                        { index: 0, stage: "canary", servers: ["srv-web-02"] },
                        {
                            index: 1,
                            stage: "wave",
                            servers: ["srv-web-01"],
                            release_delay_minutes: 15,
                        },
                    ],
                },
            ],
        }),
    );
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "Deleted policy with pending wave",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "accepted revision 3",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "Wave 1: srv-web-01 (+15 min)",
    );
    await expect(page.locator("#maintenance-calendar-list")).not.toContainText(
        "No scheduled policies to show.",
    );
});

test("admin scheduled policy editor submits rich targeting fields and renders report links", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    await expect(page.locator("#scheduled-policy-table tbody")).toContainText(
        "Nightly security",
    );
    await expect(page.locator("#scheduled-policy-table tbody")).toContainText(
        "include web",
    );
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "Nightly security",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "Allowed 02:00",
    );
    await expect(page.locator("#maintenance-calendar-list")).toContainText(
        "global 23:00-03:00 overnight",
    );
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await expect(
        page.locator(
            '#scheduled-runs-table a[href="/api/reports/jobs/job-report-1"]',
        ),
    ).toBeVisible();
    await page
        .locator(
            '#scheduled-runs-table button[data-action="job-detail"][data-job-id="job-report-1"]',
        )
        .click();
    await expect(page.locator("#job-detail-modal")).toContainText(
        "Job job-report-1",
    );
    await expect(page.locator("#job-detail-modal")).toContainText("Complete");
    await expect(page.locator("#job-detail-modal")).toContainText(
        '"max_attempts": 3',
    );
    await expect(page.locator("#job-detail-modal")).toContainText(
        "upgrade completed",
    );
    await expect(page.locator("#job-detail-copy-logs")).toBeVisible();
    await expect(page.locator("#job-detail-report")).toHaveAttribute(
        "href",
        "/api/reports/jobs/job-report-1",
    );
    await page.locator("#job-detail-close").click();

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "current");
    await page.locator("#notification-enabled").check();
    await page.locator("#notification-webhook-replace").click();
    await page
        .locator("#notification-webhook-url")
        .fill("https://hooks.example.test/simplelinuxupdater");
    await page.locator("#notification-event-schedule-skipped").uncheck();
    await page.locator("#notification-save").click();
    await expect
        .poll(() => state.notificationPayload)
        .toMatchObject({
            enabled: true,
            webhook_url: "https://hooks.example.test/simplelinuxupdater",
            webhook_url_intent: "replace",
            event_types: [
                "update.complete",
                "schedule.run.failed",
                "backup.restore",
            ],
        });
    await expect(page.locator("#notification-status")).toContainText(
        "Webhook URL replaced",
    );
    await page.locator("#notification-test").click();
    await expect.poll(() => state.notificationTestCount || 0).toBe(1);
    await expect(page.locator("#notification-diagnostics-event")).toHaveText(
        "notification.test",
    );
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Succeeded",
    );

    await page.locator("#policy-name").fill("Weekend prod");
    await page.locator("#policy-target-tag").fill("");
    await page.locator("#policy-include-tags").fill("prod, web, prod");
    await page.locator("#policy-exclude-tags").fill("hold, db");
    await page.locator("#policy-target-servers").fill("srv-web-01, srv-web-02");
    await page.locator("#policy-time-local").fill("03:45");
    await page
        .locator("#policy-execution-mode")
        .selectOption("approval_required");
    await page.locator("#policy-approval-timeout").fill("90");
    await page.locator("#policy-package-scope").selectOption("security");
    await expect(page.locator("#policy-preview")).toContainText("2 matched");
    await expect(page.locator("#policy-preview")).toContainText("srv-web-02");
    await expect(page.locator("#policy-preview")).toContainText("srv-db-01");
    await expect(page.locator("#policy-preview")).toContainText("srv-paused");
    await expect(page.locator("#policy-preview")).toContainText(
        "server disabled",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "2026-05-17 03:45",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "America/Toronto",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "UTC 2026-05-17T07:45:00.000000000Z",
    );
    await expect(page.locator("#policy-preview-facts")).toContainText(
        "canonical application timezone America/Toronto",
    );

    const previewCountBeforeTimezoneChange = state.policyPreviewCount;
    await page.locator("#app-timezone-input").click();
    await page.locator("#app-timezone-search").fill("+05:30");
    await page
        .locator('#app-timezone-options [role="option"][data-value="+05:30"]')
        .click();
    await page.locator("#app-timezone-save").click();
    await expect
        .poll(() => state.policyPreviewCount)
        .toBeGreaterThan(previewCountBeforeTimezoneChange);
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "+05:30",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "UTC 2026-05-17T22:15:00.000000000Z",
    );
    await page.locator("#policy-save-btn").click();

    await expect
        .poll(() => state.policyPreviewPayload)
        .toMatchObject({
            name: "Weekend prod",
            target_tag: "",
            include_tags: ["prod", "web"],
            exclude_tags: ["hold", "db"],
            target_servers: ["srv-web-01", "srv-web-02"],
        });
    await expect
        .poll(() => state.policyPayload)
        .toMatchObject({
            name: "Weekend prod",
            target_tag: "",
            include_tags: ["prod", "web"],
            exclude_tags: ["hold", "db"],
            target_servers: ["srv-web-01", "srv-web-02"],
            execution_mode: "approval_required",
            approval_timeout_minutes: 90,
        });
});

test("admin schedules independent autoremove without upgrade controls", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await page.locator("#policy-name").fill("Weekly cleanup");
    await page.locator("#policy-target-tag").fill("prod");
    await page.locator("#policy-upgrade-mode").selectOption("full");
    await page.locator("#policy-execution-mode").selectOption("autoremove");
    await expect(page.locator("#policy-package-scope")).toBeHidden();
    await expect(page.locator("#policy-upgrade-mode")).toBeDisabled();
    await expect(page.locator("#policy-approval-timeout")).toBeHidden();
    await expect(page.locator("#policy-autoremove-help")).toBeVisible();
    await page.locator("#policy-cadence-kind").selectOption("weekly");
    await page
        .locator('#policy-weekday-section button[data-weekday="sun"]')
        .click();
    await page.locator("#policy-time-local").fill("04:30");
    await expect(page.locator("#policy-summary")).toContainText(
        "Every Sun at 04:30",
    );
    await expect(page.locator("#policy-summary")).toContainText(
        "Autoremove: automatically remove unused packages",
    );
    await expect(page.locator("#policy-summary")).not.toContainText(
        "Full upgrade",
    );
    await page.locator("#policy-save-btn").click();
    await expect
        .poll(() => state.policyPayload)
        .toMatchObject({
            name: "Weekly cleanup",
            execution_mode: "autoremove",
            cadence_kind: "weekly",
            weekdays: ["sun"],
            time_local: "04:30",
            package_scope: "full",
            upgrade_mode: "standard",
            approval_timeout_minutes: 0,
        });
    // The received request precedes the response and editor reset.
    await expect(page.locator("#update-policy-status")).toHaveText(
        "Policy created.",
    );
    await page
        .locator("#policy-execution-mode")
        .selectOption("approval_required");
    await expect(page.locator("#policy-package-scope")).toBeVisible();
    await expect(page.locator("#policy-upgrade-mode")).toBeEnabled();
    await expect(page.locator("#policy-approval-timeout")).toBeVisible();
});

test("admin scheduled run history filters, paginates, and exposes existing investigation details", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await expect(page.locator("#scheduled-runs-result-summary")).toHaveText(
        "26 matching runs.",
    );
    await expect(page.locator("#scheduled-runs-table")).toContainText("1m 30s");
    await expect(page.locator("#scheduled-runs-table")).toContainText(
        "Job details",
    );
    await expect(
        page.locator("#scheduled-runs-table a", { hasText: "Audit trail" }),
    ).toHaveAttribute("href", /audit_action=schedule\.run\.completed/);

    await page.locator("#scheduled-runs-policy-filter").fill("Nightly");
    await page.locator("#scheduled-runs-server-filter").fill("web");
    await page
        .locator("#scheduled-runs-outcome-filter")
        .selectOption("skipped");
    await page.locator("#scheduled-runs-from-filter").fill("2026-05-01");
    await page.locator("#scheduled-runs-to-filter").fill("2026-05-31");
    await page
        .locator("#scheduled-runs-filter")
        .getByRole("button", { name: "Apply filters" })
        .click();

    await expect(page.locator("#scheduled-runs-result-summary")).toHaveText(
        "1 matching run.",
    );
    await expect(page.locator("#scheduled-runs-table")).toContainText(
        "Reason: busy",
    );
    await expect
        .poll(() => state.scheduledRunsQueries.at(-1))
        .toMatchObject({
            policy: "Nightly",
            server: "web",
            outcome: "skipped",
            from: "2026-05-01",
            to: "2026-05-31",
            page: "1",
            page_size: "25",
        });

    await page.locator("#scheduled-runs-reset").click();
    await expect(page.locator("#scheduled-runs-result-summary")).toHaveText(
        "26 matching runs.",
    );
    await page.locator("#scheduled-runs-next").click();
    await expect(page.locator("#scheduled-runs-page-info")).toHaveText(
        "Page 2 of 2",
    );
    await expect(page.locator("#scheduled-runs-table")).toContainText(
        "Older policy",
    );
    await expect.poll(() => state.scheduledRunsQueries.at(-1)?.page).toBe("2");

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
        viewportWidth: window.innerWidth,
        bodyWidth: document.body.scrollWidth,
        cellDisplay: getComputedStyle(
            document.querySelector("#scheduled-runs-table tbody td"),
        ).display,
        filterColumns: getComputedStyle(
            document.querySelector(".scheduled-runs-filter"),
        ).gridTemplateColumns,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(
        mobileLayout.viewportWidth,
    );
    expect(mobileLayout.cellDisplay).toBe("grid");
    expect(mobileLayout.filterColumns.trim().split(/\s+/)).toHaveLength(1);

    await page
        .locator("#scheduled-runs-table a", { hasText: "Audit trail" })
        .click();
    await expect(page).toHaveURL(
        /\/manage\?audit_action=schedule\.run\.failed&audit_target=srv-web-09#audit-trail$/,
    );
    await expect(page.locator("#audit-target-filter")).toHaveValue(
        "srv-web-09",
    );
    await expect(page.locator("#audit-action-filter")).toHaveValue(
        "schedule.run.failed",
    );
});

test("admin policy preview explains canonical DST, no-run, and empty-target outcomes", async ({
    baseURL,
    page,
}) => {
    const state = {
        policyPreviewResponse: {
            matched_servers: [{ name: "srv-web-01", tags: ["prod", "web"] }],
            excluded_servers: [],
            disabled_by_override: [],
            validation_errors: [],
            operational_warnings: [
                {
                    code: "no_run_window",
                    message:
                        "One or more upcoming occurrences are blocked by an applicable no-run window.",
                },
                {
                    code: "policy_schedule_overlap",
                    message:
                        "One or more enabled policies target shared servers during the same projected occurrence.",
                },
            ],
            informational_facts: [
                {
                    code: "dst_nonexistent_skipped",
                    message:
                        "2026-03-08 02:30 does not exist in America/Toronto; the scheduler skips that local occurrence.",
                },
                {
                    code: "dst_fallback_canonical_choice",
                    message:
                        "2026-11-01 01:30 is repeated; the scheduler uses the earlier occurrence.",
                },
            ],
            upcoming_occurrences: [
                {
                    local_civil_time: "2026-11-01 01:30",
                    timezone: "America/Toronto",
                    offset: "-04:00",
                    abbreviation: "EDT",
                    scheduled_for_utc: "2026-11-01T05:30:00.000000000Z",
                    dst_status: "ambiguous",
                    canonical_choice: "earlier_fallback_occurrence",
                    matched_server_count: 1,
                    applicable_no_run_windows: [
                        {
                            source: "global",
                            weekdays: ["sun"],
                            start_time: "01:00",
                            end_time: "02:00",
                            overnight: false,
                            applies_to_slot: true,
                        },
                    ],
                    admission_outcome: "blocked_no_run",
                },
            ],
            schedule_conflicts: [
                {
                    policy_id: 7,
                    policy_name: "Nightly production",
                    overlap_kind: "partial",
                    shared_servers: ["srv-web-01"],
                    occurrence_windows: [
                        {
                            local_civil_time: "2026-11-01 01:30",
                            timezone: "America/Toronto",
                            window_start_utc: "2026-11-01T05:30:00.000000000Z",
                            window_end_utc: "2026-11-01T05:31:00.000000000Z",
                            draft_admission_outcome: "blocked_no_run",
                            competing_admission_outcome: "admitted",
                            effective: false,
                        },
                        {
                            local_civil_time: "2026-11-02 01:30",
                            timezone: "America/Toronto",
                            window_start_utc: "2026-11-02T06:30:00.000000000Z",
                            window_end_utc: "2026-11-02T06:31:00.000000000Z",
                            draft_admission_outcome: "admitted",
                            competing_admission_outcome: "admitted",
                            effective: true,
                        },
                    ],
                },
            ],
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    await page.locator("#policy-name").fill("DST policy");
    await page.locator("#policy-target-tag").fill("prod");
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "2026-11-01 01:30",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "Repeated local time",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "Expected: blocked by no-run window",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "global 01:00-02:00",
    );
    await expect(page.locator("#policy-preview-facts")).toContainText(
        "does not exist",
    );
    await expect(page.locator("#policy-preview-facts")).toContainText(
        "earlier occurrence",
    );
    await expect(page.locator("#policy-preview-warnings")).toContainText(
        "blocked by an applicable no-run window",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "Nightly production",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "Partial target overlap",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "srv-web-01",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "Suppressed by no-run",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "Effective overlap",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "2026-11-02T06:30:00.000000000Z",
    );
    await expect(page.locator("#policy-save-btn")).toBeEnabled();
    await expect(page.locator("#policy-preview-validation-errors")).toBeEmpty();
    await page.setViewportSize({ width: 390, height: 844 });
    const mobileConflictLayout = await page
        .locator("#policy-preview-conflicts .preview-conflict")
        .evaluate((element) => ({
            bodyWidth: document.body.scrollWidth,
            viewportWidth: document.documentElement.clientWidth,
            conflictScrollWidth: element.scrollWidth,
            conflictClientWidth: element.clientWidth,
        }));
    expect(mobileConflictLayout.bodyWidth).toBeLessThanOrEqual(
        mobileConflictLayout.viewportWidth,
    );
    expect(mobileConflictLayout.conflictScrollWidth).toBeLessThanOrEqual(
        mobileConflictLayout.conflictClientWidth + 1,
    );
    await page.setViewportSize({ width: 1280, height: 720 });

    state.policyPreviewResponse = {
        matched_servers: [],
        excluded_servers: [
            { name: "srv-web-01", tags: ["prod"], reason: "no_target_match" },
        ],
        disabled_by_override: [],
        validation_errors: [],
        operational_warnings: [
            {
                code: "no_matching_servers",
                message: "No current server would be targeted by this policy.",
            },
        ],
        informational_facts: [
            {
                code: "application_timezone",
                message:
                    "Occurrences use the canonical application timezone America/Toronto.",
            },
        ],
        schedule_conflicts: [],
        upcoming_occurrences: [
            {
                local_civil_time: "2026-11-02 01:30",
                timezone: "America/Toronto",
                offset: "-05:00",
                abbreviation: "EST",
                scheduled_for_utc: "2026-11-02T06:30:00.000000000Z",
                dst_status: "standard",
                canonical_choice: "exact",
                matched_server_count: 0,
                applicable_no_run_windows: [],
                admission_outcome: "no_matching_servers",
            },
        ],
    };
    const previousPreviewCount = state.policyPreviewCount;
    await page.locator("#policy-target-tag").fill("");
    await page.locator("#policy-target-servers").fill("srv-missing");
    await expect
        .poll(() => state.policyPreviewCount)
        .toBeGreaterThan(previousPreviewCount);
    await expect(page.locator("#policy-preview-summary")).toContainText(
        "No current server",
    );
    await expect(page.locator("#policy-preview-occurrences")).toContainText(
        "Expected: no matching servers",
    );
    await expect(page.locator("#policy-preview-warnings")).toContainText(
        "No current server would be targeted",
    );
    await expect(page.locator("#policy-preview-conflicts")).toContainText(
        "No enabled policy overlap",
    );

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileLayout = await page.evaluate(() => ({
        bodyWidth: document.body.scrollWidth,
        viewportWidth: document.documentElement.clientWidth,
    }));
    expect(mobileLayout.bodyWidth).toBeLessThanOrEqual(
        mobileLayout.viewportWidth,
    );
    await expect(page.locator("#policy-preview-occurrences")).toBeVisible();
});

test("admin timezone picker searches by city and saves the canonical timezone", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    const picker = page.locator("#app-timezone-input");
    const saveTimezone = page.locator("#app-timezone-save");
    await expect(picker).toContainText("Toronto");
    await expect(picker).toContainText("America/Toronto");
    await expect(saveTimezone).toBeDisabled();
    await expect(page.locator("#app-timezone-preview")).toContainText(
        "Current app time:",
    );
    await expect(page.locator("#app-timezone-preview")).toContainText(
        "America/Toronto",
    );
    await expect(page.locator(".app-time-context")).toBeVisible();
    await expect(page.locator("#app-timezone-source")).toHaveText(
        "Explicit timezone",
    );

    const desktopTimeLayout = await page.evaluate(() => {
        const context = document
            .querySelector(".app-time-context")
            .getBoundingClientRect();
        const pickerPanel = document
            .querySelector(".app-time-picker-panel")
            .getBoundingClientRect();
        return {
            contextRight: context.right,
            pickerLeft: pickerPanel.left,
            pickerWidth: pickerPanel.width,
        };
    });
    expect(
        desktopTimeLayout.pickerLeft - desktopTimeLayout.contextRight,
    ).toBeGreaterThanOrEqual(16);
    expect(desktopTimeLayout.pickerWidth).toBeLessThanOrEqual(580);

    await page.setViewportSize({ width: 390, height: 844 });
    const mobileTimeLayout = await page.evaluate(() => {
        const context = document
            .querySelector(".app-time-context")
            .getBoundingClientRect();
        const pickerPanel = document
            .querySelector(".app-time-picker-panel")
            .getBoundingClientRect();
        return {
            bodyWidth: document.body.scrollWidth,
            viewportWidth: document.documentElement.clientWidth,
            contextBottom: context.bottom,
            pickerTop: pickerPanel.top,
        };
    });
    expect(mobileTimeLayout.pickerTop).toBeGreaterThanOrEqual(
        mobileTimeLayout.contextBottom + 12,
    );
    expect(mobileTimeLayout.bodyWidth).toBeLessThanOrEqual(
        mobileTimeLayout.viewportWidth,
    );
    await page.setViewportSize({ width: 1280, height: 720 });

    await picker.click();
    await expect(page.locator("#app-timezone-popover")).toBeVisible();
    const systemDefault = page.locator(
        '#app-timezone-options [role="option"][data-value=""]',
    );
    await expect(systemDefault).toContainText("System default timezone");
    await expect(systemDefault).toContainText(
        "Uses the server timezone detected when saved",
    );
    await expect(systemDefault).not.toContainText("Detected at startup");
    await expect(
        page.locator(
            '#app-timezone-options [role="option"][data-value="Local"]',
        ),
    ).toHaveCount(0);
    const overlap = await page.evaluate(() => {
        const popover = document.querySelector("#app-timezone-popover");
        const followingSection = document.querySelector(
            ".app-time-card + .workspace",
        );
        const popoverRect = popover.getBoundingClientRect();
        const followingRect = followingSection.getBoundingClientRect();
        const x = Math.max(popoverRect.left, followingRect.left) + 24;
        const y = Math.max(popoverRect.top, followingRect.top) + 24;
        const hasOverlap =
            x < Math.min(popoverRect.right, followingRect.right) &&
            y < Math.min(popoverRect.bottom, followingRect.bottom);
        const topElement = hasOverlap ? document.elementFromPoint(x, y) : null;
        return {
            hasOverlap,
            pickerOwnsOverlap: Boolean(
                topElement && popover.contains(topElement),
            ),
        };
    });
    expect(overlap.hasOverlap).toBe(true);
    expect(overlap.pickerOwnsOverlap).toBe(true);
    await page.locator("#app-timezone-search").fill("paris");
    const paris = page.locator(
        '#app-timezone-options [role="option"][data-value="Europe/Paris"]',
    );
    await expect(paris).toContainText("Paris");
    await expect(paris).toContainText("Europe/Paris");
    await paris.click();

    await expect(picker).toContainText("Paris");
    await expect(page.locator("#app-timezone-popover")).toBeHidden();
    await expect(saveTimezone).toBeEnabled();
    await saveTimezone.click();
    await expect
        .poll(() => state.timezoneSave)
        .toEqual({ timezone: "Europe/Paris" });
    await expect(page.locator("#app-timezone-status")).toContainText(
        "App timezone saved",
    );
    await expect(saveTimezone).toBeDisabled();
});

for (const [label, policyName] of [
    ["a saved policy", "Disabled dev validation"],
    [
        "a long policy name",
        "Nightly security maintenance for production application servers in the primary datacenter",
    ],
]) {
    test(`admin maintenance calendar fits mobile widths with ${label}`, async ({
        baseURL,
        page,
    }) => {
        const state = {
            policies: [
                {
                    id: 12,
                    name: policyName,
                    enabled: false,
                    target_tag: "prod",
                    execution_mode: "scan_only",
                    package_scope: "full",
                    cadence_kind: "daily",
                    time_local: "02:00",
                    weekdays: [],
                    matched_servers: [],
                },
            ],
        };
        await ensureAuthenticatedSession(page);
        await stubAdminApi(page, state);
        const calendarRequests = [];
        page.on("request", (request) => {
            const url = new URL(request.url());
            if (url.pathname === "/api/update-policies/calendar")
                calendarRequests.push(url);
        });
        await page.goto("/admin#admin-section-scheduled-policies");
        const filter = page.getByRole("combobox", {
            name: "Calendar policy filter",
        });
        const refresh = page.locator("#maintenance-calendar-refresh");
        await expect(filter.locator('option[value="12"]')).toHaveText(
            policyName,
        );

        for (const width of [390, 360, 430, 1280]) {
            await page.setViewportSize({ width, height: 844 });
            await filter.scrollIntoViewIfNeeded();
            const layout = await filter.evaluate(async (element) => {
                await document.fonts.ready;
                const head = element.closest(".table-shell-head");
                const controls = [element, head.querySelector("button")];
                return {
                    viewport: document.documentElement.clientWidth,
                    documentWidth: document.documentElement.scrollWidth,
                    bodyWidth: document.body.scrollWidth,
                    head: {
                        left: head.getBoundingClientRect().left,
                        right: head.getBoundingClientRect().right,
                    },
                    controls: controls.map((control) => ({
                        left: control.getBoundingClientRect().left,
                        right: control.getBoundingClientRect().right,
                    })),
                };
            });
            expect(
                layout.documentWidth,
                `document at ${width}px`,
            ).toBeLessThanOrEqual(layout.viewport);
            expect(layout.bodyWidth, `body at ${width}px`).toBeLessThanOrEqual(
                layout.viewport,
            );
            for (const control of layout.controls) {
                expect(control.left).toBeGreaterThanOrEqual(
                    layout.head.left - 1,
                );
                expect(control.right).toBeLessThanOrEqual(
                    layout.head.right + 1,
                );
            }
            await expect(filter).toBeInViewport({ ratio: 1 });
            await Promise.all([
                page.waitForResponse((response) => {
                    const url = new URL(response.url());
                    return (
                        url.pathname === "/api/update-policies/calendar" &&
                        url.searchParams.get("policy_id") === "12"
                    );
                }),
                filter.selectOption("12"),
            ]);
            await expect(filter).toHaveValue("12");
            const requestCount = calendarRequests.length;
            await refresh.click();
            await expect
                .poll(() => calendarRequests.length)
                .toBeGreaterThan(requestCount);
            expect(calendarRequests.at(-1).searchParams.get("policy_id")).toBe(
                "12",
            );
        }
    });
}

test("admin workspace navigation restores disclosure and focuses deep links", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    const sectionLinks = page.locator(
        "#admin-section-nav [data-admin-section-link]",
    );
    await expect
        .poll(() =>
            sectionLinks.evaluateAll((links) =>
                links.map((link) => link.dataset.adminSectionLink).sort(),
            ),
        )
        .toEqual([
            "account-security",
            "app-time",
            "application-checks",
            "backup",
            "installation",
            "metrics",
            "notifications",
            "recent-activity",
            "scheduled-policies",
            "scheduled-runs",
        ]);
    await expect(sectionLinks.first()).toHaveAttribute(
        "aria-current",
        "location",
    );

    await page
        .locator("#admin-section-account-security")
        .scrollIntoViewIfNeeded();
    await expect(
        page.locator('[data-admin-section-link="account-security"]'),
    ).toHaveAttribute("aria-current", "location");

    const applicationChecksLink = page.locator(
        '[data-admin-section-link="application-checks"]',
    );
    await expect(applicationChecksLink).toHaveAttribute(
        "href",
        "#admin-section-application-checks",
    );
    await applicationChecksLink.click();
    await expect(page).toHaveURL(/#admin-section-application-checks$/);
    await expect(
        page.locator("#admin-section-application-checks-heading"),
    ).toBeFocused();
    await expect(
        page.locator('[data-admin-section-content="application-checks"]'),
    ).toBeVisible();
    await expect(applicationChecksLink).toHaveAttribute(
        "aria-current",
        "location",
    );

    await page.locator('[data-admin-section-link="backup"]').click();
    await expect(page).toHaveURL(/#admin-section-backup$/);
    await expect(page.locator("#admin-section-backup-heading")).toBeFocused();
    await expect(page.locator("#admin-section-backup-heading")).toHaveCSS(
        "outline-style",
        "none",
    );
    await expect(
        page.locator('[data-admin-section-link="backup"]'),
    ).toHaveAttribute("aria-current", "location");

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(
        page.locator("#admin-section-notifications-heading"),
    ).toBeFocused();
    await expect(
        page.locator("#admin-section-notifications-heading"),
    ).toHaveCSS("outline-style", "none");
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "current");
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator('[data-admin-section-toggle="notifications"]').click();
    await expect(
        page.locator('[data-admin-section-content="notifications"]'),
    ).toBeHidden();
    await expect(
        page.locator('[data-admin-section-summary="notifications"]'),
    ).toContainText("Notifications disabled");
    await expect
        .poll(() =>
            page.evaluate(() =>
                JSON.parse(
                    localStorage.getItem(
                        "simplelinuxupdater.admin.collapsed-sections.v1",
                    ) || "[]",
                ),
            ),
        )
        .toEqual(["notifications"]);

    await page.reload();
    await expect(
        page.locator('[data-admin-section-content="notifications"]'),
    ).toBeHidden();
    await expect(
        page.locator('[data-admin-section-toggle="notifications"]'),
    ).toHaveAttribute("aria-expanded", "false");

    await page.goto("/admin#admin-section-notifications");
    await expect(
        page.locator('[data-admin-section-content="notifications"]'),
    ).toBeVisible();
    await expect(
        page.locator("#admin-section-notifications-heading"),
    ).toBeFocused();
    await expect(
        page.locator('[data-admin-section-link="notifications"]'),
    ).toHaveAttribute("aria-current", "location");

    await page.setViewportSize({ width: 390, height: 844 });
    const layout = await page.evaluate(() => ({
        bodyWidth: document.body.scrollWidth,
        viewportWidth: document.documentElement.clientWidth,
        navVisible: Boolean(
            document.querySelector("#admin-section-nav")?.getClientRects()
                .length,
        ),
    }));
    expect(layout.navVisible).toBe(true);
    expect(layout.bodyWidth).toBeLessThanOrEqual(layout.viewportWidth);
});

test("admin policy fields align and adjacent action groups keep visible spacing", async ({
    baseURL,
    page,
}) => {
    // Capture one stable layout snapshot. Sequential boundingBox calls can
    // otherwise observe different animation frames while sticky navigation
    // settles. The epsilon only absorbs floating-point representation noise.
    const maximumAlignmentDrift = 2;
    const minimumVisibleGap = 7;
    const layoutEpsilon = 0.01;
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.setViewportSize({ width: 1280, height: 720 });
    await page.goto("/admin#admin-section-scheduled-policies");
    const boxes = await page.evaluate(async () => {
        await document.fonts.ready;
        await new Promise((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(resolve)),
        );
        const selectors = {
            policyNameLabel: 'label[for="policy-name"]',
            policyName: "#policy-name",
            targetLabel: 'label[for="policy-target-tag"]',
            target: "#policy-target-tag",
            executionLabel: 'label[for="policy-execution-mode"]',
            execution: "#policy-execution-mode",
            packageLabel: 'label[for="policy-package-scope"]',
            package: "#policy-package-scope",
            blackoutAdd: "#policy-blackout-add",
            blackoutFallback: "#policy-blackout-rows ~ .json-fallback",
        };
        return Object.fromEntries(
            Object.entries(selectors).map(([name, selector]) => {
                const element = document.querySelector(selector);
                if (!element) return [name, null];
                const { x, y, width, height } = element.getBoundingClientRect();
                return [name, { x, y, width, height }];
            }),
        );
    });

    for (const [name, box] of Object.entries(boxes)) {
        expect(box, `${name} must have a layout box`).not.toBeNull();
    }

    expect(
        Math.abs(boxes.targetLabel.y - boxes.policyNameLabel.y),
    ).toBeLessThanOrEqual(maximumAlignmentDrift + layoutEpsilon);
    expect(Math.abs(boxes.target.y - boxes.policyName.y)).toBeLessThanOrEqual(
        maximumAlignmentDrift + layoutEpsilon,
    );
    expect(
        Math.abs(boxes.packageLabel.y - boxes.executionLabel.y),
    ).toBeLessThanOrEqual(maximumAlignmentDrift + layoutEpsilon);
    expect(Math.abs(boxes.package.y - boxes.execution.y)).toBeLessThanOrEqual(
        maximumAlignmentDrift + layoutEpsilon,
    );
    expect(
        boxes.blackoutFallback.y -
            (boxes.blackoutAdd.y + boxes.blackoutAdd.height),
    ).toBeGreaterThanOrEqual(minimumVisibleGap - layoutEpsilon);

    await page.goto("/admin#admin-section-metrics");
    const metricsBoxes = await page.evaluate(async () => {
        await document.fonts.ready;
        await new Promise((resolve) =>
            requestAnimationFrame(() => requestAnimationFrame(resolve)),
        );
        return Object.fromEntries(
            Object.entries({
                metricsOverview: ".metrics-credential-overview",
                metricsActions: "#metrics-token-generate",
                metricsDanger: "#metrics-token-danger-zone",
            }).map(([name, selector]) => {
                let element = document.querySelector(selector);
                if (name === "metricsActions") element = element?.parentElement;
                if (!element) return [name, null];
                const { x, y, width, height } = element.getBoundingClientRect();
                return [name, { x, y, width, height }];
            }),
        );
    });
    const { metricsOverview, metricsActions, metricsDanger } = metricsBoxes;
    expect(metricsOverview).not.toBeNull();
    expect(metricsActions).not.toBeNull();
    expect(metricsDanger).not.toBeNull();
    expect(
        metricsActions.y - (metricsOverview.y + metricsOverview.height),
    ).toBeGreaterThanOrEqual(minimumVisibleGap - layoutEpsilon);
    expect(
        metricsDanger.y - (metricsActions.y + metricsActions.height),
    ).toBeGreaterThanOrEqual(minimumVisibleGap - layoutEpsilon);
});

test("admin sections load heavy data lazily and recover failed sections with Retry", async ({
    baseURL,
    page,
}) => {
    const state = { backupFailuresRemaining: 1 };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.evaluate(() => {
        localStorage.setItem(
            "simplelinuxupdater.admin.collapsed-sections.v1",
            JSON.stringify([
                "notifications",
                "recent-activity",
                "scheduled-policies",
                "scheduled-runs",
                "backup",
                "metrics",
            ]),
        );
    });

    await page.goto("/admin");
    await expect
        .poll(() => ({
            notifications: state.notificationLoadCount || 0,
            activity: state.adminActivityLoadCount || 0,
            runs: state.scheduledRunsLoadCount || 0,
            calendar: state.calendarLoadCount || 0,
            backup: state.backupLoadCount || 0,
            metrics: state.metricsLoadCount || 0,
        }))
        .toEqual({
            notifications: 0,
            activity: 0,
            runs: 0,
            calendar: 0,
            backup: 0,
            metrics: 0,
        });

    await page.locator('[data-admin-section-toggle="backup"]').click();
    const backupLifecycle = page.locator(
        '[data-admin-section-lifecycle="backup"]',
    );
    await expect.poll(() => state.backupLoadCount || 0).toBe(1);
    await expect(backupLifecycle).toHaveAttribute("data-status", "failed");
    await expect(
        backupLifecycle.locator("[data-admin-section-status]"),
    ).toHaveText("Failed");
    await expect(
        backupLifecycle.locator("[data-admin-section-retry]"),
    ).toBeVisible();

    await backupLifecycle.locator("[data-admin-section-retry]").click();
    await expect.poll(() => state.backupLoadCount || 0).toBe(2);
    await expect(backupLifecycle).toHaveAttribute("data-status", "current");
    await expect(
        backupLifecycle.locator("[data-admin-section-refreshed]"),
    ).toContainText("Updated");

    await page.locator('[data-admin-section-toggle="backup"]').click();
    await page.locator('[data-admin-section-toggle="backup"]').click();
    await expect.poll(() => state.backupLoadCount || 0).toBe(2);

    await page.goto("/admin#admin-section-metrics");
    await expect.poll(() => state.metricsLoadCount || 0).toBe(1);
    await expect(
        page.locator('[data-admin-section-lifecycle="metrics"]'),
    ).toHaveAttribute("data-status", "current");

    await page.goto("/admin#admin-section-scheduled-policies");
    await expect.poll(() => state.calendarLoadCount || 0).toBe(1);
    await expect(
        page.locator('[data-admin-section-lifecycle="scheduled-policies"]'),
    ).toHaveAttribute("data-status", "current");
});

test("metrics credential lifecycle covers generate, use, stale, rotate, disable, restore, and safe copy feedback", async ({
    baseURL,
    page,
    context,
}) => {
    const state = {
        metricsResponse: {
            enabled: false,
            lifecycle_state: "disabled",
            created_at: "",
            rotated_at: "",
            last_used_at: "",
            last_used_origin_masked: "",
            stale_after_days: 30,
        },
    };
    await context.grantPermissions(["clipboard-read", "clipboard-write"], {
        origin: baseURL,
    });
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin#admin-section-metrics");

    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Disabled",
    );
    await expect(page.locator("#metrics-token-status")).toHaveText(
        "Metrics access is disabled.",
    );
    await expect(page.locator("#metrics-token-generate")).toBeEnabled();
    await expect(page.locator("#metrics-token-rotate")).toBeDisabled();
    await expect(page.locator("#metrics-token-disable")).toBeDisabled();

    await page.locator("#metrics-token-generate").click();
    await expect.poll(() => state.metricsRotateCount || 0).toBe(1);
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Never used",
    );
    await expect(page.locator("#metrics-token-status")).toContainText(
        "never completed a successful metrics request",
    );
    await expect(page.locator("#metrics-token-once")).toBeVisible();
    await expect(page.locator("#metrics-token-value")).toHaveText(
        "one-time-metrics-token",
    );

    await page.locator("#metrics-token-copy").click();
    await expect
        .poll(() => page.evaluate(() => navigator.clipboard.readText()))
        .toBe("one-time-metrics-token");
    await expect(page.locator("#app-feedback-region")).toHaveText(
        "Metrics token copied.",
    );
    await expect(page.locator("#app-feedback-region")).not.toContainText(
        "one-time-metrics-token",
    );

    state.metricsResponse = {
        enabled: true,
        lifecycle_state: "current",
        created_at: "2026-05-01T12:00:00Z",
        rotated_at: "",
        last_used_at: "2026-07-27T12:00:00Z",
        last_used_origin_masked: "198.51.100.x",
        stale_after_days: 30,
    };
    await page.reload();
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Current",
    );
    await expect(page.locator("#metrics-token-last-origin")).toHaveText(
        "198.51.100.x",
    );
    await expect(page.locator("#metrics-token-once")).toBeHidden();
    await expect(page.locator("body")).not.toContainText(
        "one-time-metrics-token",
    );

    state.metricsResponse = {
        enabled: true,
        lifecycle_state: "stale",
        created_at: "2026-05-01T12:00:00Z",
        rotated_at: "2026-05-15T12:00:00Z",
        last_used_at: "2026-05-16T12:00:00Z",
        last_used_origin_masked: "203.0.113.x",
        stale_after_days: 30,
        token: "DO-NOT-REVEAL-STATUS-TOKEN",
        last_used_origin: "203.0.113.44",
    };
    await page.reload();
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Stale",
    );
    await expect(page.locator("#metrics-token-status")).toContainText(
        "access remains enabled",
    );
    await expect(page.locator("#metrics-token-last-origin")).toHaveText(
        "203.0.113.x",
    );
    await expect(page.locator("#metrics-token-stale-policy")).toContainText(
        "never disabled automatically",
    );
    await expect(page.locator("body")).not.toContainText(
        "DO-NOT-REVEAL-STATUS-TOKEN",
    );
    await expect(page.locator("body")).not.toContainText("203.0.113.44");

    state.metricsRotateResponse = {
        enabled: true,
        lifecycle_state: "never_used",
        created_at: "2026-05-01T12:00:00Z",
        rotated_at: "2026-07-28T12:00:00Z",
        last_used_at: "",
        last_used_origin_masked: "",
        stale_after_days: 30,
        token: "rotated-one-time-token",
    };
    await dismissTypedConfirm(page, page.locator("#metrics-token-rotate"));
    expect(state.metricsRotateCount).toBe(1);
    await acceptTypedConfirm(
        page,
        page.locator("#metrics-token-rotate"),
        "ROTATE TOKEN",
    );
    await expect.poll(() => state.metricsRotateCount || 0).toBe(2);
    await expect(page.locator("#metrics-token-value")).toHaveText(
        "rotated-one-time-token",
    );
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Never used",
    );
    await expect(page.locator("#metrics-token-last-used")).toHaveText("Never");

    await acceptTypedConfirm(
        page,
        page.locator("#metrics-token-disable"),
        "DISABLE METRICS",
    );
    await expect.poll(() => state.metricsDisableCount || 0).toBe(1);
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Disabled",
    );
    await expect(page.locator("#metrics-token-once")).toBeHidden();
    await expect(page.locator("#metrics-token-danger-zone")).toContainText(
        "immediately stops every scraper",
    );

    state.metricsResponse = {
        enabled: true,
        lifecycle_state: "never_used",
        created_at: "2026-04-01T12:00:00Z",
        rotated_at: "",
        last_used_at: "",
        last_used_origin_masked: "",
        stale_after_days: 30,
    };
    await page.reload();
    await expect(page.locator("#metrics-token-lifecycle-state")).toHaveText(
        "Never used",
    );
    await expect(page.locator("#metrics-token-created")).not.toHaveText(
        "Unavailable",
    );
    await expect(page.locator("#metrics-token-last-used")).toHaveText("Never");
    await page.setViewportSize({ width: 390, height: 844 });
    const mobileMetricsLayout = await page.evaluate(() => ({
        bodyWidth: document.body.scrollWidth,
        viewportWidth: document.documentElement.clientWidth,
        factColumns: getComputedStyle(
            document.querySelector("#metrics-token-lifecycle-facts"),
        ).gridTemplateColumns.split(" ").length,
    }));
    expect(mobileMetricsLayout.bodyWidth).toBeLessThanOrEqual(
        mobileMetricsLayout.viewportWidth,
    );
    expect(mobileMetricsLayout.factColumns).toBe(1);
});

test("recent Admin activity loads on demand with safe ordered facts and retains stale results", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.evaluate(() => {
        localStorage.setItem(
            "simplelinuxupdater.admin.collapsed-sections.v1",
            JSON.stringify(["recent-activity"]),
        );
    });

    await page.goto("/admin");
    await expect.poll(() => state.adminActivityLoadCount || 0).toBe(0);
    await page.locator('[data-admin-section-link="recent-activity"]').click();
    const lifecycle = page.locator(
        '[data-admin-section-lifecycle="recent-activity"]',
    );
    const list = page.locator("#admin-activity-list");
    await expect(lifecycle).toHaveAttribute("data-status", "current");
    await expect
        .poll(() =>
            new URL(state.adminActivityURLs[0]).searchParams.get("category"),
        )
        .toBe("admin_activity");
    expect(
        new URL(state.adminActivityURLs[0]).searchParams.get("page_size"),
    ).toBe("8");
    await expect(list.locator(".admin-activity-item")).toHaveCount(2);
    await expect(list.locator(".admin-activity-action")).toHaveText([
        "Session IP revealed",
        "Policy updated",
    ]);
    await expect(list).toContainText("2026-05-17 08:05 EDT · America/Toronto");
    await expect(list).toContainText("Actor: admin");
    await expect(list).toContainText("success");
    await expect(list.locator("a").first()).toHaveAttribute(
        "href",
        "/api/reports/audit/82",
    );
    await expect(
        page.locator(
            '#admin-section-recent-activity a[href="/manage#audit-trail"]',
        ),
    ).toBeVisible();
    const pageText = await page
        .locator("#admin-section-recent-activity")
        .innerText();
    expect(pageText).not.toContain("DO-NOT-RENDER-MESSAGE");
    expect(pageText).not.toContain("DO-NOT-RENDER-META");
    expect(pageText).not.toContain("DO-NOT-RENDER-REQUEST");
    expect(pageText).not.toContain("203.0.113.44");
    expect(pageText).not.toContain("DO-NOT-RENDER-TARGET");

    state.adminActivityFailuresRemaining = 1;
    await page.evaluate(() => window.fetchRecentAdminActivity());
    await expect(lifecycle).toHaveAttribute("data-status", "stale");
    await expect(page.locator("#admin-activity-message")).toContainText(
        "Showing the last successful results",
    );
    await expect(list.locator(".admin-activity-item")).toHaveCount(2);
    await lifecycle.locator("[data-admin-section-retry]").click();
    await expect(lifecycle).toHaveAttribute("data-status", "current");

    state.adminActivityResponse = {
        items: [],
        total: 0,
        page: 1,
        page_size: 8,
    };
    await page.reload();
    await expect(page.locator("#admin-activity-message")).toHaveText(
        "No recent administrative activity.",
    );
    await expect(
        page.locator("#admin-activity-list .admin-activity-item"),
    ).toHaveCount(0);
});

test("application post-check profiles edit, persist and test with bounded expectations", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const original = await page.request
        .get("/api/postcheck-profiles")
        .then((r) => r.json());
    await page.goto("/admin#admin-section-application-checks");
    await expect(page.locator("#postcheck-new")).toBeEnabled();
    await expect(page.locator("#postcheck-feedback")).toHaveText(
        "No unsaved changes.",
    );
    try {
        await page.locator("#postcheck-new").click();
        await page.locator("#postcheck-name").fill("Website recovery");
        await page.locator("#postcheck-servers").fill("e2e-health");
        await page.locator("#postcheck-tags").fill("web");
        await page.locator("#postcheck-add").click();
        await page.locator("#postcheck-0-name").fill("HTTP readiness");
        await page.locator("#postcheck-0-url").fill("https://localhost/health");
        await page.locator("#postcheck-0-timeout_seconds").fill("20");
        await expect(page.locator("#postcheck-test")).toBeDisabled();
        await page.locator("#postcheck-save").click();
        await expect(page.locator("#postcheck-feedback")).toHaveText(
            "Application post-check profiles saved.",
        );
        await page.reload();
        await expect(page.locator("#postcheck-editor")).toBeHidden();
        await page.locator("#postcheck-profile-select").selectOption({ label: "Website recovery (revision 1)" });
        await expect(page.locator("#postcheck-name")).toHaveValue(
            "Website recovery",
        );
        await expect(page.locator("#postcheck-0-url")).toHaveValue(
            "https://localhost/health",
        );
        const saved = await page.request
            .get("/api/postcheck-profiles")
            .then((r) => r.json());
        const profile = saved.profiles.find(
            (p) => p.name === "Website recovery",
        );
        await page.route("**/api/servers/e2e-health/postchecks/test", (route) =>
            route.fulfill({
                json: {
                    server_name: "e2e-health",
                    profile,
                    results: [
                        {
                            id: "check-1",
                            name: "HTTP readiness",
                            kind: "http_status",
                            mode: "blocking",
                            passed: false,
                            attempts: 10,
                            duration_ms: 20000,
                            details:
                                "HTTP status 500; expected 200; Recovery timeout reached",
                        },
                    ],
                },
            }),
        );
        await page.locator("#postcheck-test-server").fill("e2e-health");
        await page.locator("#postcheck-test").click();
        await expect(page.locator("#postcheck-test-results")).toContainText(
            "FAIL HTTP readiness",
        );
        await expect(page.locator("#postcheck-test-results")).toContainText(
            "HTTP status 500",
        );
        await page.setViewportSize({ width: 1440, height: 1500 });
        await page.evaluate(() => window.scrollTo(0, 0));
        await page
            .locator("#admin-section-application-checks")
            .screenshot({ path: "/tmp/468-application-checks-desktop.png" });
        await page.setViewportSize({ width: 390, height: 2400 });
        await page.evaluate(() => window.scrollTo(0, 0));
        await page
            .locator("#admin-section-application-checks")
            .screenshot({ path: "/tmp/468-application-checks-mobile.png" });
        await page.setViewportSize({ width: 390, height: 844 });
        expect(
            await page.evaluate(
                () => document.documentElement.scrollWidth <= window.innerWidth,
            ),
        ).toBeTruthy();
        await page.locator("#postcheck-0-kind").selectOption("systemd_active");
        await expect(page.locator("#postcheck-0-unit")).toHaveValue(
            "nginx.service",
        );
        await expect(page.locator("#postcheck-test-results")).toHaveText("");
        await page.locator("#postcheck-discard").click();
        await expect(page.locator("#postcheck-0-url")).toHaveValue(
            "https://localhost/health",
        );
        const created = await page.request.post("/api/servers", {
            headers: { Origin: baseURL },
            data: {
                name: "e2e-health",
                host: "e2e-health.invalid",
                port: 22,
                user: "operator",
                pass: "test-only",
                tags: ["web"],
            },
        });
        expect(created.ok(), await created.text()).toBeTruthy();
        await page.setViewportSize({ width: 1440, height: 1000 });
        await page.goto("/manage");
        await page
            .locator('[data-action="edit-server"][data-name="e2e-health"]')
            .click();
        await expect(
            page.locator("#edit-application-postchecks"),
        ).toContainText("Website recovery · revision 1");
        await expect(
            page.getByRole("link", {
                name: "Configure profiles and run a read-only test in Admin",
            }),
        ).toHaveAttribute("href", "/admin#admin-section-application-checks");
        await page
            .locator("#edit-application-postchecks")
            .locator("..")
            .screenshot({ path: "/tmp/468-manage-postcheck-preview.png" });
        const pending = makeServer(
            "e2e-health",
            "pending_approval",
            makePendingUpdates(1),
            {
                job_id: "postcheck-job",
                approval_generation: 1,
                application_postcheck_profile: profile,
            },
        );
        await stubDashboardApi(page, () => [pending]);
        await page.goto("/");
        await expect(page.locator("#servers-table")).toContainText(
            "Application checks: Website recovery · revision 1",
        );
        await expect(page.locator("#servers-table")).toContainText(
            "https://localhost/health → HTTP 200",
        );

        await page.goto("/admin#admin-section-application-checks");
        await expect(page.locator("#postcheck-editor")).toBeHidden();
        await page.locator("#postcheck-profile-select").selectOption({ label: "Website recovery (revision 1)" });
        await expect(page.locator("#postcheck-name")).toHaveValue(
            "Website recovery",
        );
        await page.locator("#postcheck-delete").click();
        await expect(page.locator("#postcheck-editor")).toBeHidden();
        const removed = page.waitForResponse(
            (response) =>
                response.url().endsWith("/api/postcheck-profiles") &&
                response.request().method() === "PUT",
        );
        await page.locator("#postcheck-save").click();
        expect((await removed).ok()).toBeTruthy();
        await page.reload();
        await expect(
            page.locator("#postcheck-profile-select option"),
        ).toHaveCount(original.profiles.length);
        const effective = await page.request
            .get("/api/servers/e2e-health/postchecks")
            .then((r) => r.json());
        expect(effective.profile).toBeNull();
    } finally {
        await page.request.delete("/api/servers/e2e-health", {
            headers: { Origin: baseURL },
        });
        const current = await page.request
            .get("/api/postcheck-profiles")
            .then((r) => r.json());
        const restored = await page.request.put("/api/postcheck-profiles", {
            headers: { Origin: baseURL },
            data: { revision: current.revision, profiles: original.profiles },
        });
        expect(restored.ok(), await restored.text()).toBeTruthy();
    }
});

test("notification conditions stay opt-in and grouped reminders validate and save", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const state = {};
    await stubAdminApi(page, state);
    await page.goto("/admin#admin-section-notifications");
    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "current");
    await expect(
        page.locator("#notification-event-approval-published"),
    ).not.toBeChecked();
    await expect(
        page.locator("#notification-reminders-enabled"),
    ).not.toBeChecked();
    await page.locator("#notification-event-approval-published").check();
    await page.locator("#notification-event-reboot-required").check();
    await page.locator("#notification-reminders-enabled").check();
    await page.locator("#notification-reminder-interval").fill("4");
    await expect(page.locator("#notification-save")).toBeDisabled();
    await expect(page.locator("#notification-draft-state")).toContainText(
        "between 5 and 43200",
    );
    await page.locator("#notification-reminder-interval").fill("60");
    await page.locator("#notification-save").click();
    await expect
        .poll(() => state.notificationPayload)
        .toMatchObject({
            reminders_enabled: true,
            reminder_interval_minutes: 60,
        });
    expect(state.notificationPayload.event_types).toContain(
        "approval.published",
    );
    expect(state.notificationPayload.event_types).toContain("reboot.required");
    await expect(page.locator("#notification-reminders-enabled")).toBeChecked();
    await expect(page.locator("#notification-reminder-interval")).toHaveValue(
        "60",
    );
    await expect(page.locator("#notification-save")).toBeDisabled();
});

test("notification webhook settings preserve, replace, clear, and mask secret-bearing URLs", async ({
    baseURL,
    page,
}) => {
    const state = {
        notificationConfiguredURL:
            "https://hooks.example.test/path?token=stored-secret",
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin#admin-section-notifications");
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "current");

    await expect(page.locator("#notification-webhook-masked")).toHaveText(
        "https://hooks.example.test/••••",
    );
    await expect(page.locator("#notification-webhook-editor")).toBeHidden();
    await expect(page.locator("#notification-webhook-url")).toHaveValue("");
    expect(
        await page.locator("#admin-section-notifications").innerText(),
    ).not.toContain("stored-secret");
    await expect(page.locator("#notification-webhook-policy")).toContainText(
        "Use HTTPS for public endpoints",
    );
    await expect(page.locator("#notification-webhook-policy")).toContainText(
        "Embedded URL credentials are not supported",
    );

    await page.locator("#notification-event-schedule-skipped").uncheck();
    await page.locator("#notification-save").click();
    await expect
        .poll(() => state.notificationPayload)
        .toMatchObject({
            webhook_url_intent: "preserve",
        });
    expect(state.notificationPayload).not.toHaveProperty("webhook_url");
    await expect(page.locator("#notification-status")).toContainText(
        "configured URL was preserved",
    );
    await expect(page.locator("#notification-webhook-masked")).toHaveText(
        "https://hooks.example.test/••••",
    );

    await page.locator("#notification-webhook-replace").click();
    await expect(page.locator("#notification-webhook-editor")).toBeVisible();
    await page
        .locator("#notification-webhook-url")
        .fill("http://public.example.test/hook");
    await expect(page.locator("#notification-save")).toBeDisabled();
    await expect(page.locator("#notification-draft-state")).toContainText(
        "Use HTTPS for public endpoints",
    );
    await page
        .locator("#notification-webhook-url")
        .fill("https://replacement.example.test/hook?token=replacement-secret");
    await expect(page.locator("#notification-save")).toBeEnabled();
    await page.locator("#notification-save").click();
    await expect
        .poll(() => state.notificationPayload)
        .toMatchObject({
            webhook_url_intent: "replace",
            webhook_url:
                "https://replacement.example.test/hook?token=replacement-secret",
        });
    await expect(page.locator("#notification-status")).toContainText(
        "Webhook URL replaced",
    );
    await expect(page.locator("#notification-webhook-editor")).toBeHidden();
    expect(
        await page.locator("#admin-section-notifications").innerText(),
    ).not.toContain("replacement-secret");

    await page.locator("#notification-webhook-clear").click();
    await expect(page.locator("#notification-webhook-masked")).toHaveText(
        "Will be cleared when saved",
    );
    await page.locator("#notification-save").click();
    await expect
        .poll(() => state.notificationPayload)
        .toMatchObject({
            enabled: false,
            webhook_url_intent: "clear",
        });
    expect(state.notificationPayload).not.toHaveProperty("webhook_url");
    await expect(page.locator("#notification-status")).toContainText(
        "Webhook URL cleared",
    );
    await expect(page.locator("#notification-webhook-masked")).toHaveText(
        "Not configured",
    );
    await expect(page.locator("#notification-webhook-clear")).toBeDisabled();
});

test("notification delivery health exposes safe retry diagnostics and independent test outcomes", async ({
    baseURL,
    page,
}) => {
    const state = {
        notificationConfiguredURL:
            "https://hooks.example.test/path?token=stored-secret",
        notificationDiagnosticsResponse: {
            event_type: "update.complete",
            action: "update.complete",
            target_name: "srv-a",
            outcome: "retrying",
            success: false,
            attempts: 2,
            status_code: 503,
            error: "webhook returned HTTP 503",
            attempted_at: "2026-05-17T12:00:00Z",
            completed_at: "2026-05-17T12:00:00Z",
            duration_ms: 820,
            consecutive_failures: 2,
            next_retry_at: "2026-05-17T12:00:05Z",
            response_body: "DO-NOT-RENDER-REMOTE-BODY",
            headers: { authorization: "DO-NOT-RENDER-AUTHORIZATION" },
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin#admin-section-notifications");

    const facts = page.locator("#notification-diagnostics-facts");
    await expect(facts).toBeVisible();
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Retry scheduled",
    );
    await expect(page.locator("#notification-diagnostics-http")).toHaveText(
        "HTTP 503",
    );
    await expect(page.locator("#notification-diagnostics-duration")).toHaveText(
        "820 ms",
    );
    await expect(page.locator("#notification-diagnostics-failures")).toHaveText(
        "2",
    );
    await expect(
        page.locator("#notification-diagnostics-retry-row"),
    ).toBeVisible();
    await expect(
        page.locator("#notification-diagnostics-message"),
    ).toContainText("automatic retry is scheduled");
    expect(
        await page.locator("#admin-section-notifications").innerText(),
    ).not.toContain("DO-NOT-RENDER");
    expect(
        await page.locator("#admin-section-notifications").innerText(),
    ).not.toContain("stored-secret");

    state.notificationFailuresRemaining = 1;
    await page.evaluate(() => window.fetchNotificationSettings());
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "stale");
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Retry scheduled",
    );

    state.notificationDiagnosticsFailuresRemaining = 1;
    await page.locator("#notification-diagnostics-retry").click();
    await expect(
        page.locator("#notification-diagnostics-message"),
    ).toContainText("Showing the last successful diagnostics");
    await expect(page.locator("#notification-diagnostics-http")).toHaveText(
        "HTTP 503",
    );

    state.notificationDiagnosticsResponse = {
        event_type: "update.complete",
        outcome: "failed",
        attempts: 3,
        status_code: 502,
        error: "webhook returned HTTP 502",
        attempted_at: "2026-05-17T12:01:00Z",
        completed_at: "2026-05-17T12:01:01Z",
        duration_ms: 1040,
        consecutive_failures: 3,
        next_retry_at: "2026-05-17T12:01:05Z",
    };
    await page.locator("#notification-diagnostics-retry").click();
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Failed",
    );
    await expect(
        page.locator("#notification-diagnostics-retry-row"),
    ).toBeHidden();
    await expect(
        page.locator("#notification-diagnostics-message"),
    ).toContainText("no retry is scheduled");

    state.notificationTestFailure = true;
    state.notificationTestResponse = {
        event_type: "notification.test",
        action: "notification.test",
        target_name: "webhook",
        outcome: "failed",
        success: false,
        attempts: 3,
        status_code: 504,
        error: "Webhook delivery timed out.",
        attempted_at: "2026-05-17T12:02:00Z",
        completed_at: "2026-05-17T12:02:02Z",
        duration_ms: 2000,
        consecutive_failures: 6,
        request_headers: "DO-NOT-RENDER-TEST-HEADER",
        payload: "DO-NOT-RENDER-TEST-PAYLOAD",
    };
    await page.locator("#notification-test").click();
    await expect(page.locator("#notification-error")).toContainText(
        "Notification test failed",
    );
    await expect(page.locator("#notification-diagnostics-event")).toHaveText(
        "notification.test",
    );
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Failed",
    );
    await expect(page.locator("#notification-diagnostics-failures")).toHaveText(
        "6",
    );
    expect(
        await page.locator("#admin-section-notifications").innerText(),
    ).not.toContain("DO-NOT-RENDER");

    state.notificationTestFailure = false;
    state.notificationTestResponse = {
        event_type: "notification.test",
        action: "notification.test",
        target_name: "webhook",
        outcome: "succeeded",
        success: true,
        attempts: 1,
        status_code: 204,
        attempted_at: "2026-05-17T12:03:00Z",
        completed_at: "2026-05-17T12:03:00Z",
        duration_ms: 95,
        consecutive_failures: 0,
    };
    await page.locator("#notification-test").click();
    await expect(page.locator("#notification-status")).toContainText(
        "Webhook test delivered",
    );
    await expect(page.locator("#notification-diagnostics-outcome")).toHaveText(
        "Succeeded",
    );
    await expect(page.locator("#notification-diagnostics-failures")).toHaveText(
        "0",
    );
    await expect(
        page.locator("#notification-diagnostics-error-row"),
    ).toBeHidden();
});

test("automated backups validate configuration and display suspended, deferred and failed results", async ({
    baseURL,
    page,
}) => {
    const config = {
        enabled: true,
        cadence: "weekly",
        time: "03:15",
        weekday: 1,
        retain: 3,
        include_known_hosts: true,
    };
    const state = {
        backupStatusResponse: {
            db_path: "/tmp/test.db",
            recovery_health: backupRecoveryHealthFixture("healthy"),
            automation: {
                config,
                configured: true,
                state: "configured",
                message: "Local destination validated.",
                next_at: "2026-10-26T03:15:00Z",
                last_attempt_at: "2026-10-25T03:15:00Z",
                last_success_at: "2026-10-25T03:15:00Z",
            },
        },
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    let saved = null;
    await page.route("**/api/backup/automation", async (route) => {
        saved = route.request().postDataJSON();
        state.backupStatusResponse.automation = {
            ...state.backupStatusResponse.automation,
            config: saved,
            state: "configured",
            message: "Local destination validated.",
        };
        return route.fulfill({
            contentType: "application/json",
            body: JSON.stringify(state.backupStatusResponse.automation),
        });
    });
    await page.route("**/api/backup/automation/run", (route) => {
        state.backupStatusResponse.automation = {
            ...state.backupStatusResponse.automation,
            state: "deferred",
            message: "Backup deferred for active work.",
            next_at: "2026-10-25T03:20:00Z",
            retry_at: "2026-10-25T03:20:00Z",
        };
        return route.fulfill({
            contentType: "application/json",
            body: JSON.stringify(state.backupStatusResponse.automation),
        });
    });
    await page.goto("/admin#admin-section-backup");
    await expect(page.locator("#backup-automation-status")).toContainText(
        "configured: Local destination validated.",
    );
    await expect(page.locator("#backup-automation-cadence")).toHaveValue(
        "weekly",
    );
    await expect(page.locator("#backup-automation-time")).toHaveValue("03:15");
    await expect(page.locator("#backup-automation-success")).not.toHaveText(
        "Never",
    );
    await page.locator("#backup-automation-retain").fill("2");
    await page.locator("#backup-automation-save").click();
    await expect.poll(() => saved?.retain).toBe(2);
    expect(saved).not.toHaveProperty("passphrase");
    expect(saved).not.toHaveProperty("directory");
    await expect(page.locator("#backup-automation-run")).toBeEnabled();
    await page.locator("#backup-automation-run").click();
    await expect(page.locator("#backup-automation-status")).toContainText(
        "deferred: Backup deferred for active work.",
    );
    for (const result of ["suspended", "failed"]) {
        state.backupStatusResponse.automation = {
            ...state.backupStatusResponse.automation,
            state: result,
            configured: result !== "suspended",
            message: `Backup ${result}.`,
        };
        await page.reload();
        await expect(page.locator("#backup-automation-status")).toContainText(
            `${result}: Backup ${result}.`,
        );
        if (result === "suspended")
            await expect(page.locator("#backup-automation-run")).toBeDisabled();
    }
    await page.setViewportSize({ width: 390, height: 844 });
    await expect(page.locator("#backup-automation-save")).toBeVisible();
    expect(
        await page.evaluate(
            () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
    ).toBe(true);
});

test("backup recovery health distinguishes healthy, stale, never, failed, and unavailable evidence", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin#admin-section-backup");

    const healthState = page.locator("#backup-recovery-health-state");
    const healthPanel = page.locator(".backup-recovery-health");
    const lifecycle = page.locator('[data-admin-section-lifecycle="backup"]');
    await expect(healthState).toHaveText("Healthy");
    await expect(healthPanel).toContainText("4.0 KB");
    await expect(healthPanel).toContainText("No backup is scheduled.");
    await expect(healthPanel).toContainText("up to 90 days");
    await expect(healthPanel).toContainText(
        "not retained or deleted by the app",
    );

    state.backupStatusResponse = {
        db_path: "/tmp/simplelinuxupdater.db",
        known_hosts_path: "/tmp/known_hosts",
        recovery_health: backupRecoveryHealthFixture("stale"),
    };
    await page.reload();
    await expect(healthState).toHaveText("Stale");
    await expect(page.locator("#backup-recovery-health-message")).toContainText(
        "older than the 168-hour threshold",
    );

    state.backupStatusResponse.recovery_health =
        backupRecoveryHealthFixture("never");
    await page.reload();
    await expect(healthState).toHaveText("Never recorded");
    await expect(page.locator("#backup-recovery-export")).toHaveText(
        "Never recorded",
    );
    await expect(page.locator("#backup-recovery-verification")).toHaveText(
        "Never recorded",
    );

    state.backupStatusResponse.recovery_health =
        backupRecoveryHealthFixture("failed");
    await page.reload();
    await expect(healthState).toHaveText("Failed");
    await expect(page.locator("#backup-recovery-export-detail")).toContainText(
        "Latest attempt failed",
    );
    await expect(page.locator("#backup-recovery-export")).toContainText(
        "4.0 KB",
    );

    state.backupStatusResponse.recovery_health =
        backupRecoveryHealthFixture("healthy");
    await page.reload();
    await expect(healthState).toHaveText("Healthy");
    state.backupFailuresRemaining = 1;
    await page.evaluate(() => window.fetchBackupStatus());
    await expect(lifecycle).toHaveAttribute("data-status", "stale");
    await expect(healthState).toHaveText("Healthy");
    await expect(lifecycle.locator("[data-admin-section-retry]")).toBeVisible();

    state.backupFailuresRemaining = 1;
    await page.reload();
    await expect(lifecycle).toHaveAttribute("data-status", "failed");
    await expect(healthState).toHaveText("Unavailable");
    await lifecycle.locator("[data-admin-section-retry]").click();
    await expect(lifecycle).toHaveAttribute("data-status", "current");
    await expect(healthState).toHaveText("Healthy");
});

test("admin section drafts support save eligibility, discard, replacement confirmation, and safe navigation warnings", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    const dispatchBeforeUnload = () =>
        page.evaluate(() => {
            const event = new Event("beforeunload", { cancelable: true });
            return !window.dispatchEvent(event);
        });

    await page
        .locator("#auth-current-password")
        .fill("secret-current-password");
    await page
        .locator("#backup-export-passphrase")
        .fill("secret-backup-passphrase");
    expect(await dispatchBeforeUnload()).toBe(false);

    await page.locator('[data-admin-section-link="notifications"]').click();
    await expect(
        page.locator('[data-admin-section-lifecycle="notifications"]'),
    ).toHaveAttribute("data-status", "current");
    await expect(page.locator("#notification-save")).toBeDisabled();
    await page.locator("#notification-webhook-replace").click();
    await page
        .locator("#notification-webhook-url")
        .fill("https://hooks.example.test/replacement");
    await page.locator("#notification-enabled").check();
    await expect(page.locator("#notification-draft-state")).toContainText(
        "Webhook URL will be replaced",
    );
    await expect(page.locator("#notification-save")).toBeEnabled();
    expect(await dispatchBeforeUnload()).toBe(true);
    await page.locator("#notification-discard").click();
    await expect(page.locator("#notification-webhook-url")).toHaveValue("");
    await expect(page.locator("#notification-enabled")).not.toBeChecked();
    await expect(page.locator("#notification-save")).toBeDisabled();
    expect(await dispatchBeforeUnload()).toBe(false);

    await expect(page.locator("#policy-draft-action-bar")).toBeHidden();
    await page.locator("#policy-name").fill("Unsaved policy");
    await expect(page.locator("#policy-draft-action-bar")).toBeVisible();
    await expect(page.locator("#policy-save-btn")).toBeDisabled();
    await page.locator("#policy-target-tag").fill("prod");
    await expect(page.locator("#policy-save-btn")).toBeEnabled();

    const editPolicy = page.locator(
        'button[data-action="edit-policy"][data-id="12"]',
    );
    await editPolicy.click();
    await expect(page.locator("#action-confirm-modal")).toBeVisible();
    await expect(page.locator("#action-confirm-title")).toHaveText(
        "Discard unsaved policy changes",
    );
    await page.locator("#action-confirm-cancel").click();
    await expect(page.locator("#policy-name")).toHaveValue("Unsaved policy");

    await editPolicy.click();
    await page.locator("#action-confirm-submit").click();
    await expect(page.locator("#action-confirm-modal")).not.toBeVisible();
    await expect(page.locator("#policy-name")).toHaveValue("Nightly security");
    await expect(page.locator("#policy-draft-action-bar")).toBeHidden();

    await page.locator("#policy-name").fill("Changed accepted policy");
    await expect(page.locator("#policy-draft-action-bar")).toBeVisible();
    await page.locator("#policy-discard-btn").click();
    await expect(page.locator("#policy-name")).toHaveValue("Nightly security");
    await expect(page.locator("#policy-draft-action-bar")).toBeHidden();
});

test("admin typed confirmations gate restore and policy deletion", async ({
    baseURL,
    page,
}) => {
    const state = { deferRestore: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    await page.locator("#backup-restore-file").setInputFiles({
        name: "backup.slubkp",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("fake-backup"),
    });
    await page.locator("#backup-restore-passphrase").fill("LongPassphrase123");
    await expect(page.locator("#backup-restore-btn")).toBeDisabled();
    await page.locator("#backup-verify-btn").click();
    await expect.poll(() => state.verifyCount || 0).toBe(1);
    await expect(page.locator("#backup-review-readiness")).toHaveText(
        "Ready for confirmation",
    );
    await expect(page.locator("#backup-restore-review")).toContainText("5");
    await expect(page.locator("#backup-restore-review")).toContainText(
        "All active Admin sessions will be invalidated",
    );
    await expect(page.locator("#backup-restore-btn")).toBeEnabled();
    await dismissTypedConfirm(page, page.locator("#backup-restore-btn"));
    await expect.poll(() => state.restoreCount || 0).toBe(0);
    await expect(page.locator("#backup-restore-btn")).toBeEnabled();

    await page.evaluate(() => {
        window.alert = () => {};
    });
    await acceptTypedConfirm(
        page,
        page.locator("#backup-restore-btn"),
        "RESTORE",
    );
    await expect.poll(() => state.restoreCount || 0).toBe(1);
    await expect(page.locator("#auth-sessions-clear")).toBeDisabled();
    await expect(
        page.locator(
            '#scheduled-policy-table button[data-action="delete-policy"][data-id="12"]',
        ),
    ).toBeDisabled();
    state.releaseRestore();
    await expect.poll(() => state.restoreCompleted || false).toBe(true);
    await expect(page.locator("#auth-sessions-clear")).toBeEnabled();

    const deletePolicyButton = page.locator(
        '#scheduled-policy-table button[data-action="delete-policy"][data-id="12"]',
    );
    await dismissTypedConfirm(page, deletePolicyButton);
    await expect.poll(() => state.policyDeleteCount || 0).toBe(0);

    await acceptTypedConfirm(page, deletePolicyButton, "Nightly security");
    await expect.poll(() => state.policyDeleteCount || 0).toBe(1);
});

test("restore readiness distinguishes blockers and expires when the archive or passphrase changes", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");
    await page.locator('[data-admin-section-link="backup"]').click();

    await page.locator("#backup-restore-file").setInputFiles({
        name: "ready.slubkp",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("ready-backup"),
    });
    await page.locator("#backup-restore-passphrase").fill("LongPassphrase123");
    await page.locator("#backup-verify-btn").click();
    await expect(page.locator("#backup-review-readiness")).toHaveText(
        "Ready for confirmation",
    );
    await expect(page.locator("#backup-review-warnings")).toContainText(
        "Metrics API credential",
    );
    await expect(page.locator("#backup-restore-btn")).toBeEnabled();

    await page.locator("#backup-restore-file").setInputFiles({
        name: "future.slubkp",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("future-backup"),
    });
    await expect(page.locator("#backup-restore-review")).toBeHidden();
    await expect(page.locator("#backup-restore-btn")).toBeDisabled();

    state.verifyResponse = {
        valid: false,
        compatible: false,
        restore_ready: false,
        archive: {
            format: "simplelinuxupdater-backup",
            version: 99,
            created_at: "2026-06-01T08:00:00Z",
            size_bytes: 8192,
        },
        resources: [],
        missing_resources: [],
        safe_counts: {},
        impact: {
            sessions_invalidated: true,
            metrics_access_replaced: true,
            maintenance_required: true,
            downtime_expected: true,
            restart_required: false,
        },
        blockers: [
            {
                code: "unsupported_version",
                message: "Backup version 99 is not supported.",
            },
        ],
        warnings: [],
    };
    await page.locator("#backup-verify-btn").click();
    await expect(page.locator("#backup-review-readiness")).toHaveText(
        "Blocked",
    );
    await expect(page.locator("#backup-review-blockers")).toContainText(
        "Backup version 99 is not supported",
    );
    await expect(page.locator("#backup-restore-btn")).toBeDisabled();

    state.verifyResponse = undefined;
    await page
        .locator("#backup-restore-passphrase")
        .fill("AnotherLongPassphrase456");
    await expect(page.locator("#backup-restore-review")).toBeHidden();
    await page.locator("#backup-verify-btn").click();
    await expect(page.locator("#backup-review-readiness")).toHaveText(
        "Ready for confirmation",
    );
    await expect(page.locator("#backup-restore-btn")).toBeEnabled();
});

test("admin danger dialogs explain impact, trap keyboard focus, isolate the page, and restore focus", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    await page.locator('[data-admin-section-link="account-security"]').click();
    const revokeSession = page.locator(
        '#auth-session-inventory button[data-session-id="other-session"]',
    );
    await revokeSession.focus();
    await page.keyboard.press("Enter");
    const actionModal = page.locator("#action-confirm-modal");
    await expect(actionModal).toBeVisible();
    await expect(actionModal.locator("#action-confirm-title")).toHaveText(
        "Revoke server-side session",
    );
    await expect(actionModal.locator("[data-confirm-operation]")).toContainText(
        "Revoke one server-side session",
    );
    await expect(actionModal.locator("[data-confirm-resources]")).toContainText(
        "Firefox · Linux",
    );
    await expect(
        actionModal.locator("[data-confirm-consequences]"),
    ).toContainText("logged out immediately");
    await expect(
        actionModal.locator("[data-confirm-reversibility]"),
    ).toContainText("Not reversible");
    await expect(
        actionModal.locator("[data-confirm-authentication]"),
    ).toContainText("signed-in Admin session");
    await expect(page.locator(".dashboard-shell")).toHaveAttribute("inert", "");
    await expect(page.locator("#action-confirm-cancel")).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(page.locator("#action-confirm-submit")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(actionModal).toBeHidden();
    await expect(revokeSession).toBeFocused();
    await expect(page.locator(".dashboard-shell")).not.toHaveAttribute(
        "inert",
        "",
    );

    await page.emulateMedia({ reducedMotion: "reduce" });
    await page.setViewportSize({ width: 390, height: 640 });
    await page.locator('[data-admin-section-link="backup"]').click();
    await page.locator("#backup-restore-file").setInputFiles({
        name: "backup.slubkp",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("fake-backup"),
    });
    await page.locator("#backup-restore-passphrase").fill("LongPassphrase123");
    await page.locator("#backup-verify-btn").click();
    await expect(page.locator("#backup-review-readiness")).toHaveText(
        "Ready for confirmation",
    );
    const restore = page.locator("#backup-restore-btn");
    await restore.focus();
    await page.keyboard.press("Enter");
    const typedModal = page.locator("#typed-confirm-modal");
    await expect(typedModal).toBeVisible();
    await expect(typedModal.locator("#typed-confirm-title")).toHaveText(
        "Restore application backup",
    );
    await expect(typedModal.locator("[data-confirm-resources]")).toContainText(
        "backup.slubkp",
    );
    await expect(
        typedModal.locator("[data-confirm-authentication]"),
    ).toContainText("backup passphrase");
    await expect(page.locator("#typed-confirm-input")).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(page.locator("#typed-confirm-cancel")).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(page.locator("#typed-confirm-input")).toBeFocused();
    await expect(typedModal.locator(".confirmation-modal")).toBeInViewport();
    await page.keyboard.press("Escape");
    await expect(typedModal).toBeHidden();
    await expect(restore).toBeFocused();
});

test("admin password change sends payload and session clear requires typed confirmation", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);

    await page.goto("/admin");
    await expect(page.locator("#auth-session-status")).toContainText(
        "2 active sessions",
    );
    await expect(page.locator("#auth-session-inventory")).toContainText(
        "Chrome · Windows",
    );
    await expect(page.locator("#auth-session-inventory")).toContainText(
        "Firefox · Linux",
    );
    await expect(page.locator("#auth-session-inventory")).toContainText(
        "192.168.1.x",
    );
    await page
        .locator(
            '#auth-session-inventory button[data-session-id="other-session"]',
        )
        .click();
    await expect(page.locator("#action-confirm-modal")).toBeVisible();
    await expect(page.locator("#action-confirm-title")).toHaveText(
        "Revoke server-side session",
    );
    await page.locator("#action-confirm-cancel").click();
    await expect.poll(() => state.sessionRevokeID || "").toBe("");

    await page.locator("#auth-sessions-clear-others").click();
    await expect(page.locator("#action-confirm-modal")).toBeVisible();
    await page.locator("#action-confirm-submit").click();
    await expect.poll(() => state.sessionClearOthersCount || 0).toBe(1);
    await expect(page.locator("#auth-session-status")).toContainText(
        "1 active session",
    );
    await expect(page.locator("#auth-sessions-clear-others")).toBeDisabled();

    await page.locator("#auth-current-password").fill(password);
    await page.locator("#auth-new-password").fill(changedPassword);
    await page.locator("#auth-confirm-password").fill(changedPassword);
    await page.locator("#auth-password-save").click();
    await expect
        .poll(() => state.passwordPayload)
        .toEqual({
            current_password: password,
            new_password: changedPassword,
            confirm_password: changedPassword,
            invalidate_other_sessions: true,
        });
    await expect(page.locator("#auth-password-status")).toContainText(
        "Password changed",
    );

    await dismissTypedConfirm(page, page.locator("#auth-sessions-clear"));
    await expect.poll(() => state.sessionClearCount || 0).toBe(0);

    await page.evaluate(() => {
        window.location.assign = () => {};
    });
    await acceptTypedConfirm(
        page,
        page.locator("#auth-sessions-clear"),
        "LOGOUT ALL",
    );
    await expect.poll(() => state.sessionClearCount || 0).toBe(1);
});

test("admin password controls expose requirements, entry aids, and both session outcomes", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    await expect(page.locator("#auth-password-requirements")).toContainText(
        "10–64 characters",
    );
    await expect(page.locator("#auth-password-requirements")).toContainText(
        "At least one letter",
    );
    await expect(page.locator("#auth-password-requirements")).toContainText(
        "At least one digit",
    );
    const current = page.locator("#auth-current-password");
    await current.fill(password);
    await page.getByRole("button", { name: "Show current password" }).click();
    await expect(current).toHaveAttribute("type", "text");
    await expect(
        page.getByRole("button", { name: "Hide current password" }),
    ).toHaveAttribute("aria-pressed", "true");
    await page.getByRole("button", { name: "Hide current password" }).click();
    await expect(current).toHaveAttribute("type", "password");

    await current.evaluate((input) => {
        const event = new KeyboardEvent("keydown", { key: "A", bubbles: true });
        Object.defineProperty(event, "getModifierState", {
            value: (key) => key === "CapsLock",
        });
        input.dispatchEvent(event);
    });
    await expect(page.locator("#auth-password-caps-warning")).toBeVisible();
    await current.evaluate((input) => {
        const event = new KeyboardEvent("keyup", { key: "a", bubbles: true });
        Object.defineProperty(event, "getModifierState", {
            value: () => false,
        });
        input.dispatchEvent(event);
    });
    await expect(page.locator("#auth-password-caps-warning")).toBeHidden();

    await page.locator("#auth-new-password").fill(changedPassword);
    await page.locator("#auth-confirm-password").fill("DifferentPass123");
    await expect(page.locator("#auth-password-save")).toBeDisabled();
    await expect(page.locator("#auth-password-save")).toHaveAttribute(
        "title",
        "New passwords do not match.",
    );

    await page.locator("#auth-confirm-password").fill(changedPassword);
    await page.locator("#auth-password-invalidate-others").uncheck();
    await page.locator("#auth-password-save").click();
    await expect.poll(() => state.passwordPayloads?.length || 0).toBe(1);
    await expect(state.passwordPayloads[0]).toMatchObject({
        invalidate_other_sessions: false,
    });
    await expect(page.locator("#auth-password-status")).toHaveText(
        "Password changed. 0 sessions invalidated; 2 active sessions preserved.",
    );
    await expect(page.locator("#auth-current-password")).toHaveValue("");
    await expect(page.locator("#auth-new-password")).toHaveValue("");
    await expect(page.locator("#auth-confirm-password")).toHaveValue("");

    await page.locator("#auth-current-password").fill(password);
    await page.locator("#auth-new-password").fill(changedPassword);
    await page.locator("#auth-confirm-password").fill(changedPassword);
    await page.locator("#auth-password-invalidate-others").check();
    await page.locator("#auth-password-save").click();
    await expect.poll(() => state.passwordPayloads?.length || 0).toBe(2);
    await expect(state.passwordPayloads[1]).toMatchObject({
        invalidate_other_sessions: true,
    });
    await expect(page.locator("#auth-password-status")).toHaveText(
        "Password changed. 1 other session invalidated; 1 active session preserved.",
    );
    await expect(page.locator("#auth-session-status")).toHaveText(
        "1 active session.",
    );
});

test("admin password change reports partial session invalidation without retaining secrets", async ({
    baseURL,
    page,
}) => {
    const state = { passwordPartialFailure: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    await page.locator("#auth-current-password").fill(password);
    await page.locator("#auth-new-password").fill(changedPassword);
    await page.locator("#auth-confirm-password").fill(changedPassword);
    await page.locator("#auth-password-save").click();
    await expect(page.locator("#auth-password-status")).toHaveText(
        "Password changed, but other sessions could not be invalidated. 2 active sessions preserved.",
    );
    await expect(page.locator("#auth-password-status")).toHaveClass(
        /form-feedback-warning/,
    );
    await expect(page.locator("#auth-session-status")).toHaveText(
        "2 active sessions.",
    );
    await expect(page.locator("#auth-current-password")).toHaveValue("");
    await expect(page.locator("#auth-new-password")).toHaveValue("");
    await expect(page.locator("#auth-confirm-password")).toHaveValue("");
    await expect
        .poll(() =>
            page.evaluate(() =>
                JSON.stringify({
                    local: { ...localStorage },
                    session: { ...sessionStorage },
                }),
            ),
        )
        .not.toContain(changedPassword);
});

test("active session inventory keeps a temporary IP reveal controllable and undisclosed", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    state.sessions.push(
        { ...state.sessions[1], id: "other-session-2" },
        { ...state.sessions[1], id: "other-session-3" },
        { ...state.sessions[1], id: "other-session-4" },
    );

    await page.goto("/admin");
    await expect(
        page.locator("#auth-current-session-list .session-item"),
    ).toHaveCount(1);
    await expect(
        page.locator("#auth-other-session-list .session-item"),
    ).toHaveCount(4);
    await expect(page.locator("#auth-other-session-list")).not.toHaveClass(
        /is-expanded/,
    );
    await expect(page.locator("#auth-sessions-show-all")).toBeVisible();
    await page.locator("#auth-sessions-show-all").click();
    await expect(page.locator("#auth-other-session-list")).toHaveClass(
        /is-expanded/,
    );
    await expect(page.locator("#auth-sessions-show-all")).toHaveText(
        "Collapse",
    );

    const reveal = page.locator(
        'button[data-reveal-session-id="current-session"]',
    );
    await reveal.focus();
    await reveal.click();
    await expect(page.locator("#session-ip-reveal-modal")).toBeVisible();
    await expect(page.locator("#session-ip-reveal-password")).toBeFocused();
    await page.keyboard.press("Shift+Tab");
    await expect(page.locator("#session-ip-reveal-submit")).toBeFocused();
    await page.keyboard.press("Escape");
    await expect(page.locator("#session-ip-reveal-modal")).toBeHidden();
    await expect(reveal).toBeFocused();

    await reveal.click();
    await page.locator("#session-ip-reveal-password").fill(password);
    await page.locator("#session-ip-reveal-submit").click();
    await expect
        .poll(() => state.sessionIPRevealPayload)
        .toEqual({ current_password: password });
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.44");
    await expect(
        page.locator('[data-session-ip-visibility="current-session"]'),
    ).toContainText("Full IP visible for 30 seconds");
    const hide = page.locator('button[data-hide-session-ip="current-session"]');
    await expect(hide).toHaveText("Hide now");
    await expect(hide).toBeFocused();
    await expect(page).not.toHaveURL(/192\.168\.1\.44/);
    await expect
        .poll(() =>
            page.evaluate(() =>
                JSON.stringify({
                    local: { ...localStorage },
                    session: { ...sessionStorage },
                }),
            ),
        )
        .not.toContain("192.168.1.44");
    await expect
        .poll(() => page.locator("[aria-live]").allTextContents())
        .not.toContain("192.168.1.44");

    await hide.click();
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x");
    await expect(
        page.locator('button[data-reveal-session-id="current-session"]'),
    ).toHaveText("Reveal IP");
});

test("temporary session IP remasks on expiry, refresh, and document hiding", async ({
    baseURL,
    page,
}) => {
    const state = { sessionIPRevealSeconds: 1 };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    const revealIP = async () => {
        await page
            .locator('button[data-reveal-session-id="current-session"]')
            .click();
        await page.locator("#session-ip-reveal-password").fill(password);
        await page.locator("#session-ip-reveal-submit").click();
        await expect(
            page.locator('[data-session-ip-id="current-session"]'),
        ).toHaveText("192.168.1.44");
    };

    await revealIP();
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x", { timeout: 2500 });

    state.sessionIPRevealSeconds = 30;
    await revealIP();
    await page.evaluate(() => window.fetchAuthSessionStatus());
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x");

    await revealIP();
    await page.evaluate(() => {
        Object.defineProperty(document, "hidden", {
            configurable: true,
            value: true,
        });
        document.dispatchEvent(new Event("visibilitychange"));
    });
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x");
});

test("session IP reveal keeps authentication failures inside the secure dialog", async ({
    baseURL,
    page,
}) => {
    const state = { sessionIPRevealFailure: true };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    await page
        .locator('button[data-reveal-session-id="current-session"]')
        .click();
    await page.locator("#session-ip-reveal-password").fill("wrong password");
    await page.locator("#session-ip-reveal-submit").click();
    await expect(page.locator("#session-ip-reveal-error")).toHaveText(
        "current password is invalid",
    );
    await expect(page.locator("#session-ip-reveal-modal")).toBeVisible();
    await expect(page.locator("#session-ip-reveal-password")).toBeFocused();
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x");
    await expect(page.locator("body")).not.toContainText("192.168.1.44");
});

test("cancelling session IP reauthentication discards a late response", async ({
    baseURL,
    page,
}) => {
    let releaseResponse;
    const state = {
        sessionIPRevealResponseGate: new Promise((resolve) => {
            releaseResponse = resolve;
        }),
    };
    await ensureAuthenticatedSession(page);
    await stubAdminApi(page, state);
    await page.goto("/admin");

    const reveal = page.locator(
        'button[data-reveal-session-id="current-session"]',
    );
    await reveal.click();
    await page.locator("#session-ip-reveal-password").fill(password);
    try {
        await page.locator("#session-ip-reveal-submit").click();
        await expect
            .poll(() => state.sessionIPRevealPayload)
            .toEqual({ current_password: password });
        await page.locator("#session-ip-reveal-cancel").click();
        await expect(page.locator("#session-ip-reveal-modal")).toBeHidden();
        await expect(reveal).toBeFocused();
    } finally {
        releaseResponse();
    }
    await expect.poll(() => state.sessionIPRevealResponseFinished).toBe(true);
    await expect(
        page.locator('[data-session-ip-id="current-session"]'),
    ).toHaveText("192.168.1.x");
    await expect(page.locator("body")).not.toContainText("192.168.1.44");
});
