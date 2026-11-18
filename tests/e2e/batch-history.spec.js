const { test, expect, username, password, ensureAuthenticatedSession } = require("./support/fixtures");

test.use({ batchHistoryFixture: true });

test("Status opens an older batch after paging and restores its link after sign-in", async ({ page, baseURL }) => {
    await ensureAuthenticatedSession(page);
    const batch = page.locator("#status-batch-panel");
    await expect(batch.locator("#status-batch-link")).toHaveAttribute("href", "?maintenance_batch=" + (21).toString(16).padStart(32, "0"));
    await page.reload();
    const history = page.locator("#status-batch-history");
    await history.locator("summary").click();
    const entries = history.locator("li");
    await expect(entries).toHaveCount(20);
    await history.getByRole("button", { name: "Older" }).click();
    await expect(entries).toHaveCount(1);
    await expect(entries.first()).toContainText("1 pending · 1 finished · 1 need verification");
    const link = entries.first().getByRole("link", { name: "Update checks" });
    await expect(link).toBeFocused();
    await history.getByRole("button", { name: "Newer" }).click();
    await expect(entries).toHaveCount(20);
    await history.getByRole("button", { name: "Older" }).click();
    await expect(entries).toHaveCount(1);
    await link.click();

    const batchID = (1).toString(16).padStart(32, "0");
    const target = `/?maintenance_batch=${batchID}`;
    await expect(page).toHaveURL(baseURL + target);
    await expect(batch.getByRole("link", { name: "Job report" })).toHaveAttribute("href", "/api/reports/jobs/history-waiting-job");

    await page.context().clearCookies();
    await page.goto(target);
    await expect(page).toHaveURL(url => url.pathname === "/login" && url.searchParams.get("return_to") === target);
    await page.locator("#username").fill(username);
    await page.locator("#password").fill(password);
    await page.getByRole("button", { name: "Sign in", exact: true }).click();
    await expect(page).toHaveURL(baseURL + target);
    await expect(batch).toContainText("batch-host-01");

    for (const invalid of ["f".repeat(32), "latest", "", `${batchID}&maintenance_batch=${batchID}`]) {
        await page.goto(`/?maintenance_batch=${invalid}`);
        await expect(batch.locator("#status-batch-unavailable")).toBeVisible();
        await expect(batch.getByRole("link", { name: "Link to this batch" })).toBeHidden();
    }
});

test("Status links an interrupted APT job to the same host after rename", async ({ page }) => {
    await ensureAuthenticatedSession(page);
    await page.goto("/?maintenance_batch=" + (2).toString(16).padStart(32, "0"));
    const batch = page.locator("#status-batch-panel");
    const renamed = batch.locator("li").filter({ hasText: "before-rename" });
    await expect(renamed.getByText("APT reconciliation required", { exact: true })).toBeVisible();
    await expect(renamed.getByRole("link", { name: "Job report" })).toHaveAttribute("href", "/api/reports/jobs/recovery-job-before-rename");
    await expect(renamed.getByRole("button", { name: /Retry/ })).toHaveCount(0);

    const replaced = batch.locator("li").filter({ hasText: "replaced-host" });
    await expect(replaced.getByText("APT reconciliation required", { exact: true })).toBeVisible();
    await expect(replaced.getByText("This host is no longer in the inventory.", { exact: false })).toBeVisible();
    await expect(replaced.getByRole("link", { name: "Open host diagnostics" })).toHaveCount(0);
    await expect(replaced.getByRole("link", { name: "Job report" })).toBeVisible();

    await renamed.getByRole("link", { name: "Open host diagnostics" }).click();
    await expect(page).toHaveURL(/\/?\?server_identity=previous-before-rename$/);
    await expect(page.locator("#selected-host-title")).toHaveText("renamed-host");
    await expect(page.locator("#selected-host-panel").getByRole("button", { name: "Repair APT" })).toBeEnabled();
});
