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

test("manage server availability persists and blocks real maintenance requests", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const serverName = "availability + / test";
    const encodedName = encodeURIComponent(serverName);
    const create = await page.evaluate(async (name) => {
        const response = await fetch("/api/servers", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({
                name,
                host: "availability.example.test",
                user: "root",
                pass: "test-credential",
            }),
        });
        return response.status;
    }, serverName);
    expect(create).toBe(201);
    try {
        await page.goto("/manage");
        const row = page.locator("#manage-servers-table tbody tr").filter({
            has: page.locator('button[data-name="' + serverName + '"]'),
        });
        await row.getByRole("button", { name: "Disable", exact: true }).click();
        await expect(row.locator(".server-availability")).toHaveText(
            "Disabled",
        );
        await expect(
            row.getByRole("button", { name: "Enable", exact: true }),
        ).toBeEnabled();
        await page.reload();
        await expect(row.locator(".server-availability")).toHaveText(
            "Disabled",
        );
        const blocked = await page.evaluate(async (name) => {
            const response = await fetch("/api/update/" + name, {
                method: "POST",
            });
            return { status: response.status, body: await response.json() };
        }, encodedName);
        expect(blocked.status).toBe(409);
        expect(blocked.body.error).toContain("disabled");
        await page.setViewportSize({ width: 390, height: 844 });
        await row.getByRole("button", { name: "Enable", exact: true }).click();
        await expect(row.locator(".server-availability")).toHaveText("Enabled");
        await page.reload();
        await expect(
            row.getByRole("button", { name: "Disable", exact: true }),
        ).toBeEnabled();
    } finally {
        await page.evaluate(
            (name) => fetch("/api/servers/" + name, { method: "DELETE" }),
            encodedName,
        );
    }
});

test("manage typed confirmations gate destructive host and audit actions", async ({
    baseURL,
    page,
    context,
}) => {
    const state = {};
    await context.grantPermissions(["clipboard-read", "clipboard-write"], {
        origin: baseURL,
    });
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await expect(page.locator("#manage-servers-table tbody")).toContainText(
        "demo-host",
    );
    await expect(page.locator("#global-key-status")).toHaveText("Configured");
    await expect(page.locator("#global-key-status")).toHaveClass(
        /is-configured/,
    );
    await expect(page.locator("#upload-global-key-btn")).toHaveText(
        "Replace Global SSH Credential",
    );
    await expect(page.locator("#clear-global-key-btn")).toBeEnabled();
    await expect(page.locator("body")).not.toContainText(
        "DO-NOT-RENDER-PRIVATE-KEY",
    );
    await page.getByRole("link", { name: /Audit trail/ }).click();
    await expect(page.locator("#manage-section-audit-content")).toBeVisible();
    await expect(
        page.locator('#audit-table a[href="/api/reports/audit/55"]'),
    ).toBeVisible();
    await page.locator('#audit-table button[data-audit-detail="55"]').click();
    await expect(page.locator("#audit-detail-modal")).toContainText(
        "Audit #55",
    );
    await expect(page.locator("#audit-detail-modal")).toContainText(
        '"scope": "security"',
    );
    await expect(page.locator("#audit-detail-modal")).toContainText("req-55");
    await expect(page.locator("#audit-detail-report")).toHaveAttribute(
        "href",
        "/api/reports/audit/55",
    );
    await page.locator("#audit-detail-copy").click();
    await expect
        .poll(() => page.evaluate(() => navigator.clipboard.readText()))
        .toBe(
            [
                "Audit #55",
                "Actor: admin",
                "Status: success",
                "Action: server.delete",
                "Target: server: demo-host",
                "Time: 2026-05-17 08:00:00 America/Toronto",
                "Client IP: 127.0.0.1",
                "Request ID: req-55",
                "Message: Deleted server",
                "Metadata:",
                "{",
                '  "scope": "security",',
                '  "count": 2',
                "}",
            ].join("\n"),
        );
    await page.locator("#audit-detail-close").click();

    await page.locator("#audit-action-preset").selectOption("server.delete");
    await page.locator("#audit-from-filter").fill("2026-05-17T08:00");
    await page.locator("#audit-to-filter").fill("2026-05-17T13:00");
    await expect
        .poll(() =>
            (state.auditListUrls || []).some(
                (url) =>
                    url.includes("action=server.delete") &&
                    url.includes("from=") &&
                    url.includes("to="),
            ),
        )
        .toBe(true);

    await page.evaluate(() => {
        window.alert = () => {};
    });
    const deleteServerButton = page.locator(
        '#manage-servers-table button[data-action="delete-server"][data-name="demo-host"]',
    );
    const deleteServerMenu = deleteServerButton.locator(
        "xpath=ancestor::details",
    );
    const openDeleteServerMenu = async () => {
        if (!(await deleteServerMenu.evaluate((menu) => menu.open))) {
            await deleteServerMenu.locator("summary").click();
        }
    };
    const auditPruneButton = page.locator("#audit-prune");
    const clearGlobalKeyButton = page.locator("#clear-global-key-btn");
    await openDeleteServerMenu();
    await dismissTypedConfirm(page, deleteServerButton);
    await dismissTypedConfirm(page, auditPruneButton);
    await page.getByRole("link", { name: /Global SSH Credential/ }).click();
    await expect(
        page.locator("#manage-section-global-key-content"),
    ).toBeVisible();
    await dismissTypedConfirm(page, clearGlobalKeyButton);
    await expect.poll(() => state.deleteServerCount || 0).toBe(0);
    await expect.poll(() => state.auditPruneCount || 0).toBe(0);
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(0);

    await openDeleteServerMenu();
    await acceptTypedConfirm(page, deleteServerButton, "demo-host");
    await acceptTypedConfirm(page, auditPruneButton, "PRUNE");
    await expect.poll(() => state.deleteServerCount || 0).toBe(1);
    await expect.poll(() => state.auditPruneCount || 0).toBe(1);

    await acceptTypedConfirm(
        page,
        clearGlobalKeyButton,
        "CLEAR GLOBAL SSH CREDENTIAL",
    );
    await expect.poll(() => state.clearGlobalKeyCount || 0).toBe(1);
    await expect(page.locator("#global-key-status")).toHaveText(
        "Not configured",
    );
    await expect(page.locator("#global-key-status")).toHaveClass(/is-missing/);
    await expect(page.locator("#upload-global-key-btn")).toHaveText(
        "Add Global SSH Credential",
    );
    await expect(clearGlobalKeyButton).toBeDisabled();
});

test("manage server editor locks background scrolling while its content remains scrollable", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page.evaluate(() => window.scrollTo(0, 300));
    const editServerButton = page.locator(
        '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
    );
    await editServerButton.evaluate((element) => {
        element.addEventListener(
            "click",
            () => {
                document.documentElement.dataset.testScrollBeforeModal = String(
                    window.scrollY,
                );
            },
            { capture: true, once: true },
        );
    });

    await editServerButton.click();
    await expect(page.locator("#edit-modal")).toHaveClass(/active/);
    await expect(page.locator("html")).toHaveClass(/manage-modal-open/);
    await expect(page.locator("body")).toHaveClass(/manage-modal-open/);
    await expect(page.locator("body")).toHaveCSS("position", "fixed");
    const backgroundScrollTopBeforeOpen = Number.parseFloat(
        await page
            .locator("html")
            .getAttribute("data-test-scroll-before-modal"),
    );
    const lockedBackgroundScrollTop = await page.evaluate(() => window.scrollY);

    const modalGrid = page.locator("#edit-modal .modal-grid");
    await expect(modalGrid).toHaveCSS("overflow-y", "auto");
    const modalScroll = await modalGrid.evaluate((element) => {
        const before = element.scrollTop;
        element.scrollTop = before + 200;
        return { before, after: element.scrollTop };
    });
    expect(modalScroll.after).toBeGreaterThan(modalScroll.before);

    await page.mouse.move(5, 5);
    await page.mouse.wheel(0, 500);
    await page.waitForTimeout(100);
    await expect
        .poll(() => page.evaluate(() => window.scrollY))
        .toBe(lockedBackgroundScrollTop);

    await page.locator("#edit-cancel").click();
    await expect(page.locator("html")).not.toHaveClass(/manage-modal-open/);
    await expect(page.locator("body")).not.toHaveClass(/manage-modal-open/);
    await expect
        .poll(() => page.evaluate(() => window.scrollY))
        .toBe(backgroundScrollTopBeforeOpen);
});

test("successful immediate server-key upload accepts the editor credential intent", async ({
    baseURL,
    page,
}) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
        )
        .click();
    await page.locator("#edit-key").setInputFiles({
        name: "id_ed25519",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("test-private-key"),
    });
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "Unsaved changes",
    );
    await expect(page.locator("#edit-save")).toBeEnabled();

    await page.locator("#edit-upload-key").click();

    await expect.poll(() => state.uploadServerKeyCount || 0).toBe(1);
    await expect(page.locator("#edit-key-file-selection")).toHaveText(
        "No file selected",
    );
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "No unsaved changes",
    );
    await expect(page.locator("#edit-save")).toBeDisabled();
});

test("failed replacement-key upload retains the accepted server edit and refreshes inventory", async ({
    baseURL,
    page,
}) => {
    const state = {
        failServerKeyUpload: true,
        servers: [makeServer("demo-host")],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
        )
        .click();
    await page.locator("#edit-name").fill("renamed-host");
    await page.locator("#edit-host").fill("renamed-host.example.test");
    await page.locator("#edit-pass").fill("replacement-password");
    await page.locator("#edit-key").setInputFiles({
        name: "invalid_ed25519",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("invalid-private-key"),
    });

    await page.locator("#edit-save").click();

    await expect.poll(() => state.serverUpdateCount || 0).toBe(1);
    await expect.poll(() => state.uploadServerKeyCount || 0).toBe(1);
    await expect.poll(() => state.inventoryLoadCount || 0).toBeGreaterThan(1);
    await expect(page.locator("#manage-servers-table tbody")).toContainText(
        "renamed-host",
    );
    await expect(page.locator("#edit-modal")).toHaveClass(/active/);
    await expect(page.locator("#edit-name")).toHaveValue("renamed-host");
    await expect(page.locator("#edit-pass")).toHaveValue("");
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "Unsaved changes",
    );
    await expect(page.locator("#edit-save")).toBeEnabled();

    await page.locator("#edit-discard").click();
    await expect(page.locator("#edit-name")).toHaveValue("renamed-host");
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "No unsaved changes",
    );
});

test("successful per-server-key creation clears the displayed filename", async ({
    baseURL,
    page,
}, testInfo) => {
    const state = {};
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .getByRole("link", { name: /Add Server Create an SSH target/ })
        .click();
    await page
        .locator('input[name="add-auth-method"][value="per-server-key"]')
        .check();
    await page.locator("#name").fill("new-host");
    await page.locator("#host").fill("new-host.example.test");
    await page.locator("#user").fill("root");
    await page.locator("#key_file").setInputFiles({
        name: "id_ed25519",
        mimeType: "application/octet-stream",
        buffer: Buffer.from("test-private-key"),
    });
    await expect(page.locator("#server-key-file-selection")).toHaveText(
        "id_ed25519",
    );

    await page.locator("#onboarding-next").click();
    await expect(page.locator("#onboarding-fingerprint")).toHaveText(
        "SHA256:trusted",
    );
    await expect(page.locator("#onboarding-next")).toBeDisabled();
    await page.locator("#trust-host-key").check();
    await page.locator("#onboarding-next").click();
    await expect(page.locator("#onboarding-check-result")).toContainText(
        "Prerequisites verified",
    );
    await page.setViewportSize({ width: 1440, height: 1800 });
    await page.evaluate(() => window.scrollTo({ top: 0, behavior: "instant" }));
    const screenshotPath = testInfo.outputPath("server-onboarding-checks.png");
    await page
        .locator("#manage-section-add-server")
        .screenshot({ path: screenshotPath });
    await testInfo.attach("server-onboarding-checks", {
        path: screenshotPath,
        contentType: "image/png",
    });
    await expect.poll(() => state.onboardingCreateCount || 0).toBe(0);
    await page.locator("#onboarding-next").click();
    await page.locator("#onboarding-next").click();
    await expect.poll(() => state.onboardingCreateCount || 0).toBe(1);
    expect(state.onboardingDraft.key).toBe("test-private-key");
    expect(state.uploadServerKeyCount || 0).toBe(0);
    await expect(page.locator("#server-key-file-selection")).toHaveText(
        "No file selected",
    );
});

test("add-server validation failures render inline and focus the first invalid field", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page);

    await page.goto("/manage");
    await page
        .getByRole("link", { name: /Add Server Create an SSH target/ })
        .click();
    const submit = page.locator("#onboarding-next");
    await submit.click();

    await expect(page.locator("#add-server-error")).toContainText(
        "name, host, user required",
    );
    await expect(page.locator("#name")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#host")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#user")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#name")).toBeFocused();

    await page.locator("#name").fill("new-host");
    await page.locator("#host").fill("new-host.example.test");
    await page.locator("#user").fill("root");
    await page.locator("#port").fill("70000");
    await submit.click();

    await expect(page.locator("#add-server-error")).toContainText(
        "SSH port must be a whole number",
    );
    await expect(page.locator("#port")).toHaveAttribute("aria-invalid", "true");
    await expect(page.locator("#port")).toBeFocused();
});

test("onboarding failed checks block saving and allow correction and retry", async ({
    baseURL,
    page,
}) => {
    const state = { onboardingReady: false };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);
    await page.goto("/manage");
    await page.locator("#add-server-action").click();
    await page.locator("#name").fill("new-host");
    await page.locator("#host").fill("new-host.example.test");
    await page.locator("#user").fill("deployer");
    await page.locator("#pass").fill("test-password");
    await page.locator("#onboarding-next").click();
    await page.locator("#trust-host-key").check();
    await page.locator("#onboarding-next").click();
    await expect(page.locator("#onboarding-checks")).toContainText(
        "Install the managed root helper",
    );
    await expect(page.locator("#onboarding-next")).toBeDisabled();
    expect(state.onboardingCreateCount || 0).toBe(0);
    state.onboardingReady = true;
    await page.locator("#onboarding-retry").click();
    await expect(page.locator("#onboarding-check-result")).toContainText(
        "Prerequisites verified",
    );
    await page.locator("#onboarding-next").click();
    await expect(page.locator("#onboarding-summary")).toContainText("new-host");
    state.onboardingReady = false;
    await page.locator("#onboarding-next").click();
    await expect(page.locator('[data-onboarding-panel="3"]')).toBeVisible();
    await expect(page.locator("#onboarding-next")).toBeDisabled();
    await page.locator("#onboarding-back").click();
    await expect(page.locator("#name")).toHaveValue("new-host");
    await page.locator("#onboarding-next").click();
    await expect(page.locator("#trust-host-key")).not.toBeChecked();
});

test("manage known host controls expose trust, replace, and remove states", async ({
    baseURL,
    page,
}) => {
    const state = { hostKeyState: "missing" };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
        )
        .click();
    await expect(page.locator("#edit-known-host-state")).toHaveText(
        "Not trusted",
    );
    await expect(page.locator("#edit-known-host-fingerprint")).toHaveText(
        "SHA256:trusted",
    );
    await expect(page.locator("#edit-trust-known-host")).toHaveText(
        "Trust Host Key",
    );
    await expect(page.locator("#edit-trust-known-host")).toBeVisible();
    await expect(page.locator("#edit-clear-known-host")).toBeDisabled();

    await page.locator("#edit-trust-known-host").click();
    await expect(page.locator("#hostkey-title")).toHaveText(
        "Trust SSH Host Key",
    );
    await page.locator("#hostkey-modal-trust").click();
    await expect.poll(() => state.trustHostKeyCount || 0).toBe(1);
    await expect(page.locator("#edit-known-host-state")).toHaveText("Trusted");
    await expect(page.locator("#edit-trust-known-host")).toBeHidden();
    await expect(page.locator("#edit-clear-known-host")).toBeEnabled();

    state.hostKeyState = "changed";
    await page.locator("#edit-check-known-host").click();
    await expect(page.locator("#edit-known-host-state")).toHaveText(
        "Key changed",
    );
    await expect(page.locator("#edit-trust-known-host")).toHaveText(
        "Replace Host Key",
    );
    await page.locator("#edit-trust-known-host").click();
    await expect(page.locator("#hostkey-title")).toHaveText(
        "Replace SSH Host Key",
    );
    await page.locator("#hostkey-modal-trust").click();
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(0);
    await expect.poll(() => state.trustHostKeyCount || 0).toBe(2);
    await expect(page.locator("#edit-known-host-state")).toHaveText("Trusted");

    await acceptTypedConfirm(
        page,
        page.locator("#edit-clear-known-host"),
        "demo-host.example.test:22",
    );
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(1);
    await expect(page.locator("#edit-known-host-state")).toHaveText(
        "Not trusted",
    );
    await expect(page.locator("#edit-clear-known-host")).toBeDisabled();

    state.hostKeyState = "error";
    await page.locator("#edit-check-known-host").click();
    await expect(page.locator("#edit-known-host-state")).toHaveText(
        "Check failed",
    );
    await expect(page.locator("#edit-clear-known-host")).toBeEnabled();
    await acceptTypedConfirm(
        page,
        page.locator("#edit-clear-known-host"),
        "demo-host.example.test:22",
    );
    await expect.poll(() => state.clearKnownHostCount || 0).toBe(2);
    await expect(page.locator("#edit-known-host-state")).toHaveText(
        "Not trusted",
    );
});

test("Manage Servers confines responsive overflow to data tables", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);

    for (const viewport of [
        { width: 390, height: 844 },
        { width: 768, height: 1024 },
        { width: 1440, height: 900 },
    ]) {
        await page.setViewportSize(viewport);
        await page.goto("/manage");
        await expect(page.locator("#manage-section-nav")).toBeVisible();
        await expect(
            page.locator("#manage-section-directory-content"),
        ).toBeVisible();

        const layout = await page.evaluate(() => ({
            documentClientWidth: document.documentElement.clientWidth,
            documentScrollWidth: document.documentElement.scrollWidth,
            bodyScrollWidth: document.body.scrollWidth,
            sectionHeadDirections: Array.from(
                document.querySelectorAll(
                    ".manage-workspace-section .workspace-head",
                ),
            ).map((element) => getComputedStyle(element).flexDirection),
            metrics: Array.from(
                document.querySelectorAll(".manage-summary .metric-item"),
            ).map((element) => {
                const value = element.querySelector("strong");
                return {
                    rowTop: Math.round(element.getBoundingClientRect().top),
                    numberTop: Math.round(value.getBoundingClientRect().top),
                    numberBackground: getComputedStyle(value).backgroundColor,
                };
            }),
            tables: Array.from(
                document.querySelectorAll(
                    ".manage-workspace-section .table-wrap",
                ),
            ).map((element) => ({
                clientWidth: element.clientWidth,
                scrollWidth: element.scrollWidth,
                overflowX: getComputedStyle(element).overflowX,
            })),
        }));

        expect(
            layout.documentScrollWidth,
            `${viewport.width}px document must not overflow`,
        ).toBeLessThanOrEqual(layout.documentClientWidth + 1);
        expect(
            layout.bodyScrollWidth,
            `${viewport.width}px body must not overflow`,
        ).toBeLessThanOrEqual(layout.documentClientWidth + 1);
        for (const table of layout.tables) {
            expect(["auto", "scroll"]).toContain(table.overflowX);
        }
        if (viewport.width <= 640) {
            expect(
                layout.sectionHeadDirections.every(
                    (direction) => direction === "column",
                ),
            ).toBe(true);
        }
        const metricRows = new Map();
        for (const metric of layout.metrics) {
            if (!metricRows.has(metric.rowTop))
                metricRows.set(metric.rowTop, new Set());
            metricRows.get(metric.rowTop).add(metric.numberTop);
        }
        expect(
            [...metricRows.values()].every(
                (numberTops) => numberTops.size === 1,
            ),
        ).toBe(true);
        expect(
            new Set(layout.metrics.map((metric) => metric.numberBackground))
                .size,
        ).toBe(1);

        await expect(
            page.getByRole("link", { name: /Add Server Create an SSH target/ }),
        ).toBeVisible();
        await expect(
            page.getByRole("button", { name: "Prev", exact: true }),
        ).toBeVisible();
        await expect(
            page.getByRole("button", { name: "Next", exact: true }),
        ).toBeVisible();
    }
});

test("Manage Servers section navigation focuses the destination heading without an outline", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.goto("/manage");

    await page
        .getByRole("link", { name: /Add Server Create an SSH target/ })
        .click();

    const heading = page.locator("#manage-section-add-server-heading");
    await expect(heading).toBeFocused();
    await expect(heading).toHaveCSS("outline-style", "none");
});

test("Manage Servers section navigation follows the visible workspace while scrolling", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, {
        servers: Array.from({ length: 12 }, (_, index) =>
            makeServer(`scroll-host-${index + 1}`),
        ),
    });
    await page.goto("/manage");

    const addServerLink = page.getByRole("link", {
        name: /Add Server Create an SSH target/,
    });
    const directoryLink = page.getByRole("link", {
        name: /Server directory \d+ servers/,
    });
    const auditLink = page.locator('[data-manage-section-link="audit"]');
    await addServerLink.click();
    await expect(addServerLink).toHaveAttribute("aria-current", "location");

    await page.locator("#manage-servers-table").evaluate((element) => {
        element.scrollIntoView({ block: "start" });
    });

    await expect(directoryLink).toHaveAttribute("aria-current", "location");
    await expect(addServerLink).not.toHaveAttribute("aria-current", "location");
    await expect(page).toHaveURL(/#manage-section-directory$/);

    await page
        .locator('[data-manage-section-toggle="directory"]')
        .evaluate((button) => button.click());

    await expect(auditLink).toHaveAttribute("aria-current", "location");
    await expect(directoryLink).not.toHaveAttribute("aria-current", "location");
    await expect(page).toHaveURL(/#manage-section-audit$/);

    await page.evaluate(() =>
        window.scrollTo(0, document.documentElement.scrollHeight),
    );

    await expect(auditLink).toHaveAttribute("aria-current", "location");
    await expect(directoryLink).not.toHaveAttribute("aria-current", "location");
    await expect(page).toHaveURL(/#manage-section-audit$/);
});

test("Manage Servers keeps its initial section on a non-scrollable tall viewport", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [] });
    await page.setViewportSize({ width: 1440, height: 2000 });
    await page.goto("/manage");
    await page.evaluate(
        () =>
            new Promise((resolve) => {
                requestAnimationFrame(() => requestAnimationFrame(resolve));
            }),
    );

    await expect(
        page.locator('[data-manage-section-link="directory"]'),
    ).toHaveAttribute("aria-current", "location");
    await expect(
        page.locator('[data-manage-section-link="audit"]'),
    ).not.toHaveAttribute("aria-current", "location");
});

test("Manage Servers dedicated Add Server action opens the creation workspace", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.goto("/manage");

    const toggle = page.locator('[data-manage-section-toggle="add-server"]');
    const chevron = toggle.locator(".manage-section-toggle-icon");
    const chevronDirection = () =>
        chevron.evaluate((element) => {
            const matrix = new DOMMatrixReadOnly(
                getComputedStyle(element).transform,
            );
            return Math.sign(matrix.b);
        });
    await expect(toggle).toHaveAttribute("aria-expanded", "false");
    await expect.poll(chevronDirection).toBe(1);

    await page.getByRole("button", { name: "Add Server", exact: true }).click();

    await expect(
        page.locator("#manage-section-add-server-content"),
    ).toBeVisible();
    await expect(
        page.locator("#manage-section-add-server-heading"),
    ).toBeFocused();
    await expect(toggle).toHaveAttribute("aria-expanded", "true");
    await expect.poll(chevronDirection).toBe(-1);
});

test("Manage Servers summary distinguishes missing authentication from host trust issues", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, {
        hasGlobalKey: false,
        servers: [
            makeServer("missing-auth", "idle", [], {
                host_key_status: "missing",
            }),
            makeServer("trusted-password", "idle", [], {
                has_password: true,
                host_key_status: "trusted",
            }),
            makeServer("untrusted-key", "idle", [], {
                has_key: true,
                host_key_status: "missing",
            }),
        ],
    });
    await page.goto("/manage");

    await expect(page.locator("#manage-summary-missing-auth")).toHaveText("1");
    await expect(page.locator("#manage-summary-host-trust")).toHaveText("2");
});

test("inventory import previews real CSV, selects rows, configures credentials and enables explicitly", async ({
    baseURL,
    page,
}, testInfo) => {
    await ensureAuthenticatedSession(page);
    const state = { servers: [], hasGlobalKey: false, hostKeyState: "missing" };
    await stubManageApi(page, state);
    let selectedRows = [];
    let sshCalls = 0;
    page.on("request", (request) => {
        if (request.url().includes("/api/hostkeys/")) sshCalls++;
    });
    await page.route("**/api/servers/import", async (route) => {
        selectedRows = route.request().postDataJSON().rows;
        const response = await route.fetch();
        const result = await response.json();
        if (response.ok())
            state.servers = selectedRows.map((row) =>
                makeServer(row.name, "idle", [], {
                    ...row,
                    disabled: true,
                    has_password: false,
                    has_key: false,
                    host_key_status: "missing",
                }),
            );
        await route.fulfill({ response });
    });
    await page.route("**/api/servers/*/availability", async (route) => {
        state.servers[0].disabled = route.request().postDataJSON().disabled;
        await fulfillJson(route, state.servers[0]);
    });
    await page.goto("/manage#manage-section-import");
    const download = page.waitForEvent("download");
    await page.getByRole("link", { name: "Download CSV example" }).click();
    expect((await download).suggestedFilename()).toBe("inventory-example.csv");
    await page.locator("#inventory-import-file").setInputFiles({
        name: "fleet.csv",
        mimeType: "text/csv",
        buffer: Buffer.from(
            'name,host,port,user,tags\nweb-import-471,192.0.2.71,,root,"[""production"",""web""]"\nWEB-import-471,192.0.2.72,22,root,[]\ndb-import-471,2001:db8::71,2222,updater,"[""staging""]"\n<img src=x onerror=window.importXSS=1>,https://invalid,22,root,[]\n',
        ),
    });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-preview-summary")).toHaveText(
        "4 rows · 2 valid · 2 rejected · 2 selected",
    );
    await expect(page.locator("#import-preview-heading")).toBeFocused();
    await expect(page.locator("#import-rows tr").nth(0)).toContainText(
        "192.0.2.71:22",
    );
    await expect(page.locator("#import-rows input").nth(1)).toBeDisabled();
    await expect(page.locator("#import-rows tr").nth(3)).toContainText(
        "<img src=x onerror=window.importXSS=1>",
    );
    expect(await page.evaluate(() => window.importXSS)).toBeUndefined();
    await expect(page.locator("#import-rows img")).toHaveCount(0);
    await page.locator("#import-rows input").nth(2).uncheck();
    const originalSelection = await page
        .locator("#import-rows input")
        .nth(0)
        .isChecked();
    await page.locator('[data-manage-section-link="directory"]').click();
    await page.locator('[data-manage-section-link="import"]').click();
    expect(await page.locator("#import-rows input").nth(0).isChecked()).toBe(
        originalSelection,
    );
    expect(sshCalls).toBe(0);
    await page.setViewportSize({ width: 1440, height: 1200 });
    await page.locator("#manage-section-import").screenshot({
        path: testInfo.outputPath("inventory-import-preview-desktop.png"),
    });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.locator("#manage-section-import").screenshot({
        path: testInfo.outputPath("inventory-import-preview-mobile.png"),
    });
    expect(
        await page.evaluate(
            () => document.documentElement.scrollWidth <= window.innerWidth,
        ),
    ).toBe(true);
    await page.setViewportSize({ width: 1440, height: 1200 });
    await page.locator("#import-review-button").click();
    await expect(page.locator("#import-confirm-heading")).toBeFocused();
    await expect(page.locator("#import-confirm-count")).toHaveText("1");
    await expect(page.locator("#import-confirmation")).toContainText(
        "No credentials or SSH trust are imported",
    );
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#import-result")).toBeVisible();
    await expect(page.locator("#import-result-heading")).toBeFocused();
    expect(selectedRows.map((row) => row.name)).toEqual(["web-import-471"]);
    expect(selectedRows[0]).not.toHaveProperty("disabled");
    expect(selectedRows[0]).not.toHaveProperty("pass");
    expect(sshCalls).toBe(0);
    await page.locator("#manage-section-import").screenshot({
        path: testInfo.outputPath("inventory-import-result-desktop.png"),
    });
    await page
        .getByRole("button", { name: "Configure web-import-471" })
        .click();
    await expect(page.locator("#edit-modal")).toHaveClass(/active/);
    await expect(page.locator("#edit-name")).toHaveValue("web-import-471");
    await page.locator("#edit-pass").fill("fixture-password");
    await page.locator("#edit-trust-known-host").click();
    await expect(page.locator("#hostkey-modal")).toHaveClass(/active/);
    await page.locator("#hostkey-modal-trust").click();
    await expect.poll(() => state.trustHostKeyCount || 0).toBe(1);
    await page.locator("#edit-save").click();
    await expect.poll(() => state.serverUpdateCount || 0).toBe(1);
    await expect(page.locator("#edit-modal")).not.toHaveClass(/active/);
    await page.locator('[data-manage-section-link="directory"]').click();
    const server = page
        .locator("#manage-servers-table tbody tr")
        .filter({ hasText: "web-import-471" });
    await expect(server).toContainText("Disabled");
    await server.getByRole("button", { name: "Enable", exact: true }).click();
    await expect(server).toContainText("Enabled");
    // Delete only this test's actual API fixture; editor operations above are stubbed.
    await page.request.delete("/api/servers/web-import-471", {
        headers: { Origin: baseURL },
    });
});

test("inventory import rejects secrets and retries validation and persistence without losing the preview", async ({
    baseURL,
    page,
}, testInfo) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [] });
    let validationAttempts = 0;
    let commits = 0;
    await page.route(
        "**/api/servers/import/preview?format=json",
        async (route) => {
            validationAttempts++;
            if (validationAttempts === 2)
                return fulfillJson(
                    route,
                    { error: "Validation temporarily unavailable. Retry." },
                    503,
                );
            const response = await route.fetch();
            await route.fulfill({ response });
        },
    );
    await page.route("**/api/servers/import", async (route) => {
        commits++;
        const selection = route.request().postDataJSON();
        if (commits === 1)
            return fulfillJson(
                route,
                {
                    outcome: "failed",
                    error: "Inventory could not be saved. No servers were imported; review and retry",
                    created: [],
                    preview: {
                        batch_id: selection.batch_id,
                        rows: selection.rows.map((row) => ({
                            ...row,
                            errors: [],
                        })),
                    },
                },
                500,
            );
        const response = await route.fetch();
        await route.fulfill({ response });
    });
    await page.goto("/manage#manage-section-import");
    await page.locator("#inventory-import-file").setInputFiles({
        name: "credentials.json",
        mimeType: "application/json",
        buffer: Buffer.from(
            '[{"name":"unsafe","host":"192.0.2.80","user":"root","pass":"DO-NOT-DISPLAY-THIS-SECRET"}]',
        ),
    });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-feedback")).toContainText(
        "credentials and other fields are not supported",
    );
    await expect(page.locator("#inventory-import")).not.toContainText(
        "DO-NOT-DISPLAY-THIS-SECRET",
    );
    await page.locator("#inventory-import-file").setInputFiles({
        name: "retry.json",
        mimeType: "application/json",
        buffer: Buffer.from(
            '[{"name":"retry-import-471","host":"192.0.2.81","user":"root"}]',
        ),
    });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-feedback")).toContainText(
        "temporarily unavailable",
    );
    await expect(page.locator("#import-preview-button")).toBeEnabled();
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-preview")).toBeVisible();
    await page.locator("#import-review-button").click();
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#import-feedback")).toContainText(
        "No servers were imported",
    );
    await expect(page.locator("#import-rows input")).toBeChecked();
    await expect(page.locator("#import-review-button")).toBeEnabled();
    await page.locator("#manage-section-import").screenshot({
        path: testInfo.outputPath("inventory-import-retry-desktop.png"),
    });
    await page.locator("#import-review-button").click();
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#import-result")).toBeVisible();
    expect(commits).toBe(2);
    await page.request.delete("/api/servers/retry-import-471", {
        headers: { Origin: baseURL },
    });
});

test("inventory import revalidates a stale preview and creates only the refreshed selection", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [] });
    await page.goto("/manage#manage-section-import");
    await page.locator("#inventory-import-file").setInputFiles({
        name: "conflict.json",
        mimeType: "application/json",
        buffer: Buffer.from(
            '[{"name":"conflict-import-471","host":"192.0.2.91","user":"root"},{"name":"remaining-import-471","host":"192.0.2.92","user":"root"}]',
        ),
    });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-preview-summary")).toContainText(
        "2 selected",
    );
    const created = await page.request.post("/api/servers", {
        headers: { Origin: baseURL },
        data: {
            name: "claimed-import-471",
            host: "192.0.2.91",
            user: "root",
            disabled: true,
        },
    });
    expect(created.status()).toBe(201);
    await page.locator("#import-review-button").click();
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#import-feedback")).toContainText(
        "Inventory changed",
    );
    await expect(page.locator("#import-rows input").nth(0)).toBeDisabled();
    await expect(page.locator("#import-rows input").nth(1)).toBeChecked();
    await expect(page.locator("#import-preview-summary")).toContainText(
        "1 selected",
    );
    await page.locator("#import-review-button").click();
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#import-created")).toContainText(
        "remaining-import-471",
    );
    await expect(page.locator("#import-created")).not.toContainText(
        "conflict-import-471",
    );
    for (const name of ["claimed-import-471", "remaining-import-471"])
        await page.request.delete(`/api/servers/${name}`, {
            headers: { Origin: baseURL },
        });
});

test("inventory import preserves drafts and ignores a preview arriving after replacement or reset", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [makeServer("editor-draft")] });
    let release;
    const gate = new Promise((resolve) => {
        release = resolve;
    });
    await page.route(
        "**/api/servers/import/preview?format=json",
        async (route) => {
            await gate;
            await fulfillJson(route, {
                batch_id: "a".repeat(32),
                rows: [
                    {
                        row: 1,
                        name: "stale-row",
                        host: "192.0.2.100",
                        port: 22,
                        user: "root",
                        tags: [],
                        errors: [],
                    },
                ],
            });
        },
    );
    await page.goto("/manage#manage-section-import");
    await page.locator("#name").evaluate((input) => {
        input.value = "creation-draft";
    });
    await page.locator("#inventory-import-file").setInputFiles({
        name: "old.json",
        mimeType: "application/json",
        buffer: Buffer.from("[]"),
    });
    const started = page.waitForRequest(
        "**/api/servers/import/preview?format=json",
    );
    await page.locator("#import-preview-button").click();
    await started;
    await page.locator("#inventory-import-file").setInputFiles({
        name: "replacement.csv",
        mimeType: "text/csv",
        buffer: Buffer.from("name,host,port,user,tags\n"),
    });
    const done = page.waitForResponse(
        "**/api/servers/import/preview?format=json",
    );
    release();
    await done;
    await expect(page.locator("#import-file-name")).toHaveText(
        "replacement.csv",
    );
    await expect(page.locator("#import-preview")).toBeHidden();
    await page.locator('[data-manage-section-link="add-server"]').click();
    await expect(page.locator("#name")).toHaveValue("creation-draft");
    await page.locator('[data-manage-section-link="import"]').click();
    await expect(page.locator("#import-file-name")).toHaveText(
        "replacement.csv",
    );
    await page.locator("#import-reset").click();
    await expect(page.locator("#inventory-import-file")).toBeFocused();
    await expect(page.locator("#import-file-name")).toHaveText(
        "No file selected",
    );
});

test("inventory import preserves navigation and editor focus when validation finishes in the background", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, { servers: [makeServer("focus-fixture")] });
    let pendingResponse;
    await page.route(
        "**/api/servers/import/preview?format=json",
        async (route) => {
            await pendingResponse;
            await fulfillJson(route, {
                batch_id: "a".repeat(32),
                rows: [
                    {
                        row: 1,
                        name: "background-row",
                        host: "192.0.2.110",
                        port: 22,
                        user: "root",
                        tags: [],
                        errors: [],
                    },
                ],
            });
        },
    );
    await page.goto("/manage#manage-section-import");
    for (const openEditor of [false, true]) {
        let release;
        pendingResponse = new Promise((resolve) => {
            release = resolve;
        });
        await page.locator('[data-manage-section-link="import"]').click();
        await page.locator("#inventory-import-file").setInputFiles({
            name: `${openEditor ? "editor" : "directory"}.json`,
            mimeType: "application/json",
            buffer: Buffer.from("[]"),
        });
        const started = page.waitForRequest(
            "**/api/servers/import/preview?format=json",
        );
        await page.locator("#import-preview-button").click();
        await started;
        await page.locator('[data-manage-section-link="directory"]').click();
        let focused = page.locator("#manage-section-directory-heading");
        if (openEditor) {
            await page
                .locator(
                    '[data-action="edit-server"][data-name="focus-fixture"]',
                )
                .click();
            focused = page.locator("#edit-host");
            await focused.fill("operator-draft.example");
        }
        await expect(focused).toBeFocused();
        const done = page.waitForResponse(
            "**/api/servers/import/preview?format=json",
        );
        release();
        await done;
        await expect(page.locator("#import-preview-summary")).toContainText(
            "1 selected",
        );
        await expect(focused).toBeFocused();
        if (openEditor)
            await expect(focused).toHaveValue("operator-draft.example");
    }
});

test("inventory import Configure waits for the automatic inventory refresh", async ({
    baseURL,
    page,
}) => {
    const state = await prepareImportConfigureResult(page);
    let release;
    const gate = new Promise((resolve) => {
        release = resolve;
    });
    await page.route("**/api/servers", async (route) => {
        await gate;
        await fulfillJson(route, state.servers);
    });
    const refreshing = page.waitForRequest("**/api/servers");
    await page.locator("#import-confirm-button").click();
    await refreshing;
    await expect(page.locator("#import-result")).toBeVisible();
    await page.getByRole("button", { name: "Configure review-import" }).click();
    release();
    await expect(page.locator("#edit-modal")).toHaveClass(/active/);
    await expect(page.locator("#edit-name")).toHaveValue("review-import");
});

test("inventory import delayed Configure preserves a newer editor draft", async ({
    baseURL,
    page,
}) => {
    const state = await prepareImportConfigureResult(page);
    await page.locator("#import-confirm-button").click();
    await expect(page.locator("#manage-servers-table tbody")).toContainText(
        "review-import",
    );
    let release;
    const gate = new Promise((resolve) => {
        release = resolve;
    });
    await page.route("**/api/servers", async (route) => {
        await gate;
        await fulfillJson(route, state.servers);
    });
    const refreshing = page.waitForRequest("**/api/servers");
    await page.getByRole("button", { name: "Configure review-import" }).click();
    await refreshing;
    await page.locator('[data-manage-section-link="directory"]').click();
    await page
        .locator('[data-action="edit-server"][data-name="other-editor"]')
        .click();
    await page.locator("#edit-host").fill("unsaved-editor.example");
    const finished = page.waitForResponse("**/api/servers");
    release();
    await finished;
    await page.waitForFunction(
        () => window.ManagePage.getView().streams.inventory.inFlight === null,
    );
    await expect(page.locator("#edit-name")).toHaveValue("other-editor");
    await expect(page.locator("#edit-host")).toHaveValue(
        "unsaved-editor.example",
    );
});

test("package deferrals 473 Manage and policy editor preserve origin and independent drafts", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const direct = {
        id: "server-rule",
        server_name: "deferral-host",
        selector: "openssl:amd64",
        reason: "Compatibility investigation — ticket OPS-104",
        author: "fixture-admin",
        created_at: "2026-10-26T12:00:00Z",
        expires_at: "2030-11-10T17:00:00Z",
        expires_at_display: "2030-11-10 12:00:00 EST",
        origin: "Server: deferral-host",
    };
    const inherited = {
        ...direct,
        id: "policy-rule",
        server_name: "",
        policy_id: 12,
        selector: "libssl3:amd64",
        origin: "Policy: Production (revision 3)",
        reason: "Validate application compatibility",
    };
    let rules = [direct, inherited],
        created;
    await stubManageApi(page, { servers: [makeServer("deferral-host")] });
    await page.route(
        "**/api/servers/deferral-host/package-deferrals",
        (route) => fulfillJson(route, { revision: "fixture-revision", rules }),
    );
    await page.route("**/api/servers/deferral-host/postchecks", (route) =>
        fulfillJson(route, { profile: null }),
    );
    await page.route("**/api/package-deferrals", (route) => {
        if (route.request().method() === "POST") {
            created = route.request().postDataJSON();
            rules.push({ ...direct, ...created, id: "new-rule" });
            return fulfillJson(route, rules.at(-1), 201);
        }
        return fulfillJson(route, { rules });
    });
    await page.route("**/api/package-deferrals/*", (route) => {
        rules = rules.filter(
            (r) => r.id !== route.request().url().split("/").pop(),
        );
        return fulfillJson(route, { message: "ended" });
    });
    await page.goto("/manage");
    await page
        .locator('[data-action="edit-server"][data-name="deferral-host"]')
        .click();
    const editor = page.locator("#server-package-deferrals");
    await expect(editor).toContainText("Production (revision 3)");
    await expect(editor).toContainText("fixture-admin");
    await expect(editor).toContainText("12:00:00 EST");
    await editor
        .locator('[data-deferral-field="selector"]')
        .fill("linux-image-amd64");
    await editor
        .locator('[data-deferral-field="reason"]')
        .fill("Testing reboot compatibility");
    await editor
        .locator('[data-deferral-field="expires_at"]')
        .fill("2030-11-10T17:00:00Z");
    await captureDeferral473(page, editor, "desktop-manage-rule-editing.png");
    await captureDeferral473(
        page,
        editor,
        "mobile-manage-rule-editing.png",
        true,
    );
    expect(
        await page.evaluate(
            () => document.documentElement.scrollWidth <= innerWidth,
        ),
    ).toBeTruthy();
    await editor.locator("[data-add-deferral]").click();
    await expect(editor.locator("[data-deferral-feedback]")).toContainText(
        "Saved.",
    );
    expect(created).toEqual({
        server_name: "deferral-host",
        selector: "linux-image-amd64",
        reason: "Testing reboot compatibility",
        expires_at: "2030-11-10T17:00:00Z",
    });
    await editor.locator('[data-end-deferral="new-rule"]').click();
    await expect(editor).not.toContainText("Testing reboot compatibility");
    await page.setViewportSize({ width: 1440, height: 1000 });
    await stubAdminApi(page, {
        policies: [
            {
                id: 12,
                name: "Production",
                enabled: true,
                target_tag: "prod",
                time_local: "02:00",
                cadence_kind: "daily",
                execution_mode: "approval_required",
                package_scope: "security",
                revision: 3,
            },
        ],
    });
    await page.goto("/admin");
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await page.locator('[data-action="edit-policy"][data-id="12"]').click();
    const policyEditor = page.locator("#policy-package-deferrals");
    await expect(policyEditor).toContainText("libssl3:amd64");
    await expect(policyEditor).not.toContainText("openssl:amd64");
    await policyEditor
        .locator('[data-deferral-field="selector"]')
        .fill("postgresql-17");
    await policyEditor
        .locator('[data-deferral-field="reason"]')
        .fill("Staging validation");
    await policyEditor
        .locator('[data-deferral-field="expires_at"]')
        .fill("2030-11-10T17:00:00Z");
    await page.locator('[data-admin-section-link="scheduled-runs"]').click();
    await page
        .locator('[data-admin-section-link="scheduled-policies"]')
        .click();
    await expect(
        policyEditor.locator('[data-deferral-field="reason"]'),
    ).toHaveValue("Staging validation");
    await captureDeferral473(
        page,
        policyEditor,
        "desktop-policy-rule-editing.png",
    );
    await captureDeferral473(
        page,
        policyEditor,
        "mobile-policy-rule-editing.png",
        true,
    );
    await policyEditor.locator("[data-add-deferral]").click();
    await expect(
        policyEditor.locator("[data-deferral-feedback]"),
    ).toContainText("Saved.");
    expect(created.policy_id).toBe(12);
});

test("manage policy override list follows live tag edits", async ({
    baseURL,
    page,
}) => {
    const state = {
        servers: [{ ...makeServer("demo-host"), tags: ["prod"] }],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
        )
        .click();
    await expect(page.locator("#edit-trust-host-key")).toHaveCount(0);
    await expect(page.locator("#edit-known-host-state")).toHaveText("Trusted");
    await expect(page.locator("#edit-trust-known-host")).toBeHidden();
    await expect(page.locator("#edit-clear-known-host")).toBeEnabled();
    const overrides = page.locator("#edit-policy-overrides");
    await expect(overrides).toContainText('Disable "Prod security"');

    await page.locator("#edit-tags").fill("hold");
    await expect(overrides).toContainText(
        "No tag-based scheduled policies currently match this server.",
    );

    await page.locator("#edit-tags").fill("web");
    await expect(overrides).toContainText('Disable "Prod security"');

    state.policies = [
        {
            id: 10,
            name: "Explicit server policy",
            target_tag: "",
            include_tags: [],
            exclude_tags: [],
            target_servers: ["demo-host"],
            matched_servers: ["Demo-Host"],
        },
    ];
    state.servers = [{ ...makeServer("Demo-Host"), tags: ["misc"] }];

    await page.locator("#edit-cancel").click();
    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="Demo-Host"]',
        )
        .click();
    await expect(page.locator("#edit-policy-overrides")).toContainText(
        'Disable "Explicit server policy"',
    );
});
test("failed policy override save keeps the accepted server editor open and dirty", async ({
    baseURL,
    page,
}) => {
    const state = {
        failPolicyOverrideSave: true,
        servers: [{ ...makeServer("demo-host"), tags: ["prod"] }],
    };
    await ensureAuthenticatedSession(page);
    await stubManageApi(page, state);

    await page.goto("/manage");
    await page
        .locator(
            '#manage-servers-table button[data-action="edit-server"][data-name="demo-host"]',
        )
        .click();
    const override = page.locator(
        '#edit-policy-overrides input[data-policy-id="9"]',
    );
    await expect(override).toBeVisible();
    await override.check();
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "Unsaved changes",
    );

    await page.locator("#edit-save").click();

    await expect.poll(() => state.serverUpdateCount || 0).toBe(1);
    await expect.poll(() => state.policyOverrideSaveCount || 0).toBe(1);
    await expect(page.locator("#edit-modal")).toHaveClass(/active/);
    await expect(page.locator("#edit-draft-state")).toHaveText(
        "Unsaved changes",
    );
    await expect(page.locator("#edit-save")).toBeEnabled();
});

test("inventory import keeps accepted result focus when viewport tracking changes the section", async ({
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const state = { servers: [] };
    await stubManageApi(page, state);
    const entry = {
        row: 1,
        name: "focus-import",
        host: "192.0.2.141",
        port: 22,
        user: "root",
        tags: [],
        errors: [],
    };
    await page.route("**/api/servers/import/preview?format=json", (route) =>
        fulfillJson(route, { batch_id: "b".repeat(32), rows: [entry] }),
    );
    let release;
    const gate = new Promise((resolve) => (release = resolve));
    await page.route("**/api/servers/import", async (route) => {
        await gate;
        state.servers = [
            makeServer(entry.name, "idle", [], { disabled: true }),
        ];
        return fulfillJson(
            route,
            {
                outcome: "success",
                preview: { batch_id: "b".repeat(32), rows: [entry] },
                created: [entry.name],
            },
            201,
        );
    });
    await page.goto("/manage#manage-section-import");
    await page
        .locator("#inventory-import-file")
        .setInputFiles({
            name: "focus.json",
            mimeType: "application/json",
            buffer: Buffer.from("[]"),
        });
    await page.locator("#import-preview-button").click();
    await expect(page.locator("#import-preview-heading")).toBeFocused();
    await page.locator("#import-review-button").click();
    const started = page.waitForRequest("**/api/servers/import");
    await page.locator("#import-confirm-button").click();
    await started;
    // Viewport observation can change the navigation highlight while the disabled
    // initiating button releases keyboard focus. It is not a newer focus target.
    await page.evaluate(() =>
        window.scrollTo(0, document.documentElement.scrollHeight),
    );
    await expect
        .poll(() =>
            page.evaluate(
                () => window.ManagePage.getView().workspace.activeSection,
            ),
        )
        .toBe("audit");
    release();
    await expect(page.locator("#import-result-heading")).toBeFocused();
});
