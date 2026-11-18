const { test, expect, password, ensureAuthenticatedSession } = require("./support/fixtures");

test.use({ approvalLinkFixture: true });

function approvalLink(overrides = {}) {
    return "/?" + new URLSearchParams({
        approval_generation: "4",
        job_id: "notification-job",
        server_identity: "stable-notification-host",
        ...overrides,
    });
}

async function signInThroughLink(page, baseURL, role) {
    const target = approvalLink();
    await page.context().clearCookies();
    await page.goto(target);
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
    await page.route("**/api/servers", async route => {
        const response = await route.fetch();
        const rows = await response.json();
        rows.find(row => row.server_identity === "stable-notification-host").approval_generation = 5;
        await route.fulfill({ response, json: rows });
    });
    await page.evaluate(() => window.fetchServers());
    await expect(drawer).toHaveAttribute("aria-hidden", "true");
    await expect(notice).toContainText("This approval plan was replaced.");
    await expect(notice).toBeFocused();
});
