const { readFile } = require("node:fs/promises");
const { test, expect, password, ensureAuthenticatedSession, navigateSection } = require("./support/fixtures");

test.use({ approvalLinkFixture: true });

const linkedJob = "6383ba6587c34f1190c2d906396a1d3e";

function approvalLink(overrides = {}) {
    return "/?" + new URLSearchParams({
        approval_generation: "4",
        job_id: linkedJob,
        server_identity: "stable-notification-host",
        ...overrides,
    });
}

test("delivery history opens the report and exact pending approval by keyboard on mobile", async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/admin#admin-section-notifications");
    await navigateSection(page, "admin", "notifications");
    const history = page.locator("#notification-history");
    await history.getByText("Delivery history", { exact: true }).click();
    const missing = history.getByRole("listitem").filter({ hasText: "Job report unavailable" });
    await expect(missing).toContainText("Delivered");
    await expect(missing.getByRole("link")).toHaveCount(0);
    expect(await history.evaluate(panel => panel.scrollWidth <= panel.clientWidth)).toBe(true);

    const report = history.getByRole("link", { name: "Job report", exact: true });
    const plan = history.getByRole("link", { name: "Review approval plan", exact: true });
    await expect(report).toHaveAttribute("href", "/api/reports/jobs/" + linkedJob);
    await expect(plan).toHaveAttribute("href", approvalLink());
    await report.focus();
    const [download] = await Promise.all([page.waitForEvent("download"), page.keyboard.press("Enter")]);
    expect(download.suggestedFilename()).toBe("job-" + linkedJob + ".md");
    expect(await readFile(await download.path(), "utf8")).toContain(linkedJob);

    await expect(report).toBeFocused();
    await page.keyboard.press("Tab");
    await expect(plan).toBeFocused();
    await page.keyboard.press("Enter");
    await expect(page).toHaveURL(url => url.pathname + url.search === approvalLink());
    await expect(page.locator("#status-drawer-title")).toHaveText("renamed-edge");
    await expect(page.locator("#status-drawer")).toContainText("Linked plan · Generation 4");
});

async function signInThroughLink(page, baseURL, role) {
    await page.context().clearCookies();
    await page.goto(approvalLink());
    await completeApprovalSignIn(page, baseURL, role);
}

async function completeApprovalSignIn(page, baseURL, role) {
    const target = approvalLink();
    await expect(page).toHaveURL(url => url.pathname === "/login" && url.searchParams.get("return_to") === target);
    await page.locator("#username").fill(role);
    await page.locator("#password").fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(baseURL + target);
    await expect(page.locator("#status-drawer-title")).toHaveText("renamed-edge");
    await expect(page.locator("#status-drawer")).toContainText("Linked plan · Generation 4");
    await expect(page.locator("#drawer-panel-pending")).toContainText("openssl");
}

test("approval notification opens the exact renamed host after sign-in and preserves read-only access", async ({ page, baseURL }) => {
    await ensureAuthenticatedSession(page);
    for (const role of ["operator", "reader"]) {
        const status = await page.evaluate(async ({ role, password }) => (await fetch("/api/auth/accounts", {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify({ username: role, role, password }),
        })).status, { role, password });
        expect(status).toBe(201);
    }

    const maintenanceRequests = [];
    page.on("request", request => {
        if (request.method() === "POST" && /^\/api\/(approve|cancel|update|status\/batches)/.test(new URL(request.url()).pathname)) {
            maintenanceRequests.push(request.url());
        }
    });
    await signInThroughLink(page, baseURL, "operator");
    await expect(page.locator("#drawer-approve-all")).toBeEnabled();
    await page.context().clearCookies();
    await page.evaluate(() => { void window.fetchServers(); });
    await completeApprovalSignIn(page, baseURL, "operator");
    await signInThroughLink(page, baseURL, "reader");
    await expect(page.locator("#status-drawer-approval-actions")).toBeHidden();
    expect(maintenanceRequests).toEqual([]);
    const status = await page.evaluate(async () => (await fetch("/api/approve/renamed-edge", { method: "POST" })).status);
    expect(status).toBe(403);
});

test("approval link distinguishes stale plans and closes the drawer when the linked generation changes", async ({ page }) => {
    await ensureAuthenticatedSession(page);
    const notice = page.locator("#linked-server-message");
    const drawer = page.locator("#status-drawer");
    for (const [changes, hint] of [
        [{ approval_generation: "3" }, "This approval plan was replaced."],
        [{ job_id: "expired-job" }, "This approval has expired or is no longer pending."],
        [{ server_identity: "identity-before-recreation" }, "The linked host was removed or replaced in the inventory."],
    ]) {
        await page.goto(approvalLink(changes));
        await expect(notice).toBeVisible();
        await expect(notice).toContainText(hint);
        await expect(drawer).toHaveAttribute("aria-hidden", "true");
    }
    await page.goto(approvalLink());
    await expect(page.locator("#drawer-panel-pending")).toContainText("openssl");
    let detailConflict = false;
    await page.route("**/api/servers/status-detail?**", async route => {
        const request = new URL(route.request().url());
        if (request.searchParams.get("view") !== "packages" || request.searchParams.get("require_pending_approval") !== "true") {
            return route.continue();
        }
        detailConflict = true;
        await route.fulfill({ status: 409, json: { error: "Host status changed." } });
    });
    await page.route("**/api/servers", async route => {
        const response = await route.fetch();
        const rows = await response.json();
        if (detailConflict) {
            rows.find(row => row.server_identity === "stable-notification-host").approval_generation = 5;
        }
        await route.fulfill({ response, json: rows });
    });
    await page.evaluate(() => window.fetchServers());
    await expect(notice).toContainText("This approval plan was replaced.", { timeout: 4000 });
    await expect(drawer).toHaveAttribute("aria-hidden", "true");
    await expect(notice).toBeFocused();
});

test("a targeted host refresh retires a replaced notification plan", async ({ page }) => {
    await page.addInitScript(() => {
        window.EventSource = class extends EventTarget {
            constructor() {
                super();
                window.approvalFixtureEvents = this;
                setTimeout(() => this.dispatchEvent(new Event("open")), 0);
            }
            close() {}
        };
    });
    await ensureAuthenticatedSession(page);
    const read = pathname => page.waitForResponse(response => new URL(response.url()).pathname === pathname);
    const initial = Promise.all([read("/api/servers"), read("/api/dashboard/summary")]);
    await page.goto(approvalLink());
    const [inventory, dashboard] = await Promise.all((await initial).map(response => response.json()));
    await expect(page.locator("#drawer-panel-pending")).toContainText("openssl");
    await page.route("**/api/status/changed-hosts?*", route => {
        const params = new URL(route.request().url()).searchParams;
        expect(params.getAll("server")).toEqual(["renamed-edge"]);
        return route.fulfill({
            json: {
                servers: [{ ...inventory.find(row => row.name === "renamed-edge"), approval_generation: 5 }],
                dashboard: { ...dashboard, servers: dashboard.servers.filter(row => row.name === "renamed-edge") },
            },
        });
    });
    await page.evaluate(() => window.approvalFixtureEvents.dispatchEvent(new MessageEvent("dashboard", {
        data: JSON.stringify({ reason: "job.update", server_name: "renamed-edge", revision: 1 }),
    })));
    const notice = page.locator("#linked-server-message");
    await expect(notice).toContainText("This approval plan was replaced.");
    await expect(notice).toBeFocused();
    await expect(page.locator("#status-drawer")).toHaveAttribute("aria-hidden", "true");
});
