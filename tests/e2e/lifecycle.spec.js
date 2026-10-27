const {
    test,
    expect,
    ensureAuthenticatedSession,
    password,
    changedPassword,
} = require("./support/fixtures");
const {
    stubAdminApi,
    stubManageApi,
    stubDashboardApi,
    makeServer,
    makePendingUpdates,
    fulfillJson,
} = require("./support/builders");
const { startInstallation } = require("./support/installation");

test("Admin remount cancels old reads and preserves one account and timezone listener", async ({
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const state = {};
    await stubAdminApi(page, state);
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.goto("/admin");
    await expect(page.locator("#auth-session-status")).toContainText(
        "2 active sessions",
    );
    for (let n = 0; n < 3; n++) {
        await page.evaluate(() => window.AdminPage.mount());
        await expect(page.locator("#auth-session-status")).toContainText(
            "2 active sessions",
        );
    }
    await page.locator("#auth-sessions-clear-others").click();
    await expect(page.locator("#action-confirm-modal")).toBeVisible();
    await page.evaluate(() => window.AdminPage.mount());
    await expect(page.locator("#action-confirm-modal")).toBeHidden();
    expect(state.sessionClearOthersCount || 0).toBe(0);
    await expect(page.locator("#auth-session-status")).toContainText(
        "2 active sessions",
    );
    await page.locator("#auth-sessions-clear-others").click();
    await expect(page.locator("#action-confirm-modal")).toBeVisible();
    await page.locator("#action-confirm-submit").click();
    await expect.poll(() => state.sessionClearOthersCount).toBe(1);
    await page.locator('[data-admin-section-link="app-time"]').click();
    await page.locator("#app-timezone-input").click();
    await expect(page.locator("#app-timezone-popover")).toBeVisible();
    await page.locator("#app-timezone-search").press("Escape");
    await expect(page.locator("#app-timezone-input")).toBeFocused();
    // An old fetch that ignores cancellation must not render failure or old facts.
    let release;
    const gate = new Promise((resolve) => (release = resolve));
    let delayed = true;
    await page.route("**/api/auth/sessions", async (route) => {
        if (delayed) {
            delayed = false;
            await gate;
            return fulfillJson(route, { session_count: 99, sessions: [] });
        }
        await route.fallback();
    });
    const requested = page.waitForRequest("**/api/auth/sessions");
    const pending = page.evaluate(() => window.fetchAuthSessionStatus());
    await requested;
    await page.evaluate(() => window.AdminPage.mount());
    release();
    await pending;
    await expect(page.locator("#auth-session-status")).toContainText(
        "1 active session",
    );
    expect(errors).toEqual([]);
});

test("Manage remount leaves one inventory action onboarding wizard and import listener", async ({
    page,
}) => {
    await ensureAuthenticatedSession(page);
    const state = { servers: [makeServer("demo-host")] };
    await stubManageApi(page, state);
    let changes = 0,
        previews = 0;
    await page.route("**/api/servers/demo-host/availability", (route) => {
        changes++;
        state.servers[0].disabled = true;
        return fulfillJson(route, { ok: true });
    });
    await page.route("**/api/servers/import/preview?format=json", (route) => {
        previews++;
        return fulfillJson(route, { batch_id: "a".repeat(32), rows: [] });
    });
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.goto("/manage");
    for (let n = 0; n < 3; n++) {
        await page.evaluate(() => window.ManagePage.mount());
        await expect(page.locator("#manage-servers-table")).toContainText(
            "demo-host",
        );
    }
    await page.locator('[data-action="toggle-server"]').click();
    await expect.poll(() => changes).toBe(1);
    await page.locator('[data-manage-section-link="import"]').click();
    await page.locator("#inventory-import-file").setInputFiles({
        name: "servers.json",
        mimeType: "application/json",
        buffer: Buffer.from("[]"),
    });
    await page.locator("#import-preview-button").click();
    await expect.poll(() => previews).toBe(1);
    await page.locator("#add-server-action").click();
    await page.locator("#name").fill("remount-host");
    await page.locator("#host").fill("synthetic.invalid");
    await page.locator("#user").fill("root");
    await page.locator("#pass").fill("SyntheticOnly123!");
    await page.locator("#onboarding-next").click();
    await expect(page.locator("#onboarding-fingerprint")).not.toBeEmpty();
    expect(errors).toEqual([]);
});

test("Status remount closes superseded streams and leaves one selection and action handler", async ({
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.addInitScript(() => {
        window.__lifecycleSources = [];
        window.__rowListeners = 0;
        const nativeAdd = EventTarget.prototype.addEventListener;
        EventTarget.prototype.addEventListener = function (type, ...args) {
            if (this.classList?.contains("row-select")) window.__rowListeners++;
            return nativeAdd.call(this, type, ...args);
        };
        window.EventSource = class {
            constructor() {
                this.listeners = {};
                this.closed = false;
                window.__lifecycleSources.push(this);
            }
            addEventListener(type, callback) {
                this.listeners[type] = callback;
            }
            close() {
                this.closed = true;
            }
        };
    });
    const servers = [
        makeServer("demo-host", "pending_approval", makePendingUpdates(2)),
    ];
    await stubDashboardApi(page, () => servers);
    let cancels = 0;
    await page.route("**/api/cancel/demo-host", (route) => {
        cancels++;
        return fulfillJson(route, { ok: true });
    });
    const errors = [];
    page.on("pageerror", (error) => errors.push(String(error)));
    await page.goto("/");
    for (let n = 0; n < 3; n++) {
        await page.evaluate(() => window.StatusPage.mount());
        await expect(page.locator("#servers-table")).toContainText("demo-host");
    }
    expect(
        await page.evaluate(
            () =>
                window.__lifecycleSources.filter((source) => !source.closed)
                    .length,
        ),
    ).toBe(1);
    for (let n = 0; n < 20; n++)
        await page.evaluate(() =>
            window.fetchServers(true, "lifecycle-render"),
        );
    expect(await page.evaluate(() => window.__rowListeners)).toBe(0);
    await page.locator("#servers-table .row-select").check();
    const sourcesBeforeCache = await page.evaluate(
        () => window.__lifecycleSources.length,
    );
    await page.evaluate(() => {
        window.dispatchEvent(
            new PageTransitionEvent("pagehide", { persisted: true }),
        );
        window.dispatchEvent(
            new PageTransitionEvent("pageshow", { persisted: true }),
        );
    });
    expect(
        await page.evaluate(
            () => window.statusPageInteraction.getView().selectedNames,
        ),
    ).toEqual(["demo-host"]);
    expect(await page.evaluate(() => window.__lifecycleSources.length)).toBe(
        sourcesBeforeCache,
    );
    await page.locator('#servers-table [data-action="cancel-upgrade"]').click();
    await expect.poll(() => cancels).toBe(1);
    await page.evaluate(() => window.StatusPage.destroy());
    expect(
        await page.evaluate(
            () =>
                window.__lifecycleSources.filter((source) => !source.closed)
                    .length,
        ),
    ).toBe(0);
    await page.evaluate(() => window.StatusPage.mount());
    await expect(page.locator("#servers-table")).toContainText("demo-host");
    expect(errors).toEqual([]);
});

test("simultaneous installations isolate database config host trust passwords and revocation", async ({
    browser,
}) => {
    const installations = await Promise.all([
        startInstallation(),
        startInstallation(),
    ]);
    const contexts = [];
    try {
        for (const installation of installations)
            contexts.push(
                await browser.newContext({ baseURL: installation.url }),
            );
        expect(new Set(installations.map((item) => item.url)).size).toBe(2);
        expect(new Set(installations.map((item) => item.pid)).size).toBe(2);
        for (let i = 0; i < 2; i++)
            expect(
                (
                    await contexts[i].request.post("/api/auth/setup", {
                        headers: { Origin: installations[i].url },
                        data: { username: "admin", password },
                    })
                ).status(),
            ).toBe(200);
        const facts = await Promise.all(
            contexts.map((context) =>
                context.request
                    .get("/api/backup/status")
                    .then((response) => response.json()),
            ),
        );
        for (const key of ["db_path", "config_path", "known_hosts_path"]) {
            expect(facts[0][key]).not.toBe(facts[1][key]);
            for (let i = 0; i < 2; i++)
                expect(facts[i][key]).toContain(installations[i].directory);
        }
        expect(
            (
                await contexts[0].request.put("/api/auth/password", {
                    headers: { Origin: installations[0].url },
                    data: {
                        current_password: password,
                        new_password: changedPassword,
                        confirm_password: changedPassword,
                        invalidate_other_sessions: true,
                    },
                })
            ).status(),
        ).toBe(200);
        expect(
            (
                await contexts[0].request.delete("/api/auth/sessions", {
                    headers: { Origin: installations[0].url },
                })
            ).status(),
        ).toBe(200);
        expect(
            (
                await contexts[1].request
                    .get("/api/auth/status")
                    .then((response) => response.json())
            ).authenticated,
        ).toBe(true);
        const login = await browser.newContext({
            baseURL: installations[1].url,
        });
        contexts.push(login);
        expect(
            (
                await login.request.post("/api/auth/login", {
                    headers: { Origin: installations[1].url },
                    data: { username: "admin", password },
                })
            ).status(),
        ).toBe(200);
    } finally {
        await Promise.all(contexts.map((context) => context.close()));
        await Promise.all(
            installations.map((installation) => installation.stop()),
        );
    }
});
