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

test("setup form shows mismatch error", async ({ baseURL, page }) => {
    await page.goto("/setup");
    await page.locator("#username").fill(username);
    await page.locator("#password").fill(password);
    await page.locator("#password-confirm").fill("DifferentPass1234");
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page.locator("#error-banner")).toBeVisible();
    await expect(page.locator("#error-banner")).toContainText(
        "Passwords do not match.",
    );
    await expect(page).toHaveURL(/\/setup$/);
});

test("setup creates account and redirects to status page", async ({
    baseURL,
    page,
}) => {
    await page.goto("/setup");
    if (/\/login$/.test(page.url())) {
        await signIn(page);
        return;
    }
    if (page.url() === new URL("/", page.url()).href) {
        await expect(page.locator("#logout-btn")).toBeVisible();
        return;
    }
    await page.locator("#username").fill(username);
    await page.locator("#password").fill(password);
    await page.locator("#password-confirm").fill(password);
    await page.getByRole("button", { name: "Create account" }).click();
    await expect(page).toHaveURL(new URL("/", page.url()).href);
    await expect(page.locator("#logout-btn")).toBeVisible();
});

test("invalid login shows error, valid login succeeds", async ({
    baseURL,
    page,
}) => {
    await ensureAuthenticatedSession(page);
    await page.locator("#logout-btn").click();
    await expect(page).toHaveURL(/\/login$/);

    await page.locator("#username").fill(username);
    await page.locator("#password").fill("WrongPassword123");
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page.locator("#error-banner")).toBeVisible();
    await expect(page.locator("#error-banner")).toContainText(
        /invalid credentials|login failed/i,
    );
    await expect(page).toHaveURL(/\/login$/);

    await page.locator("#password").fill(password);
    await signIn(page);
});
