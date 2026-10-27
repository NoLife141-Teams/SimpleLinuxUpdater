const { test: base, expect } = require("@playwright/test");
const { startInstallation } = require("./installation");
const username = "admin",
    password = "StrongPass1234",
    changedPassword = "NewStrongPass123";
const test = base.extend({
    scanTransportPhase: ["", {option: true}],
    installation: async ({scanTransportPhase}, use) => {
        const installation = await startInstallation(undefined, {SLU_E2E_VULNERABILITY_PHASE: scanTransportPhase});
        try {
            await use(installation);
        } finally {
            await installation.stop();
        }
    },
    baseURL: async ({ installation }, use) => use(installation.url),
});
async function ensureAuthenticatedSession(page) {
    await page.goto("/login");
    const result = await page.evaluate(
        async ({ username, password }) => {
            const status = await (await fetch("/api/auth/status")).json();
            if (status.authenticated) return true;
            const response = await fetch(
                status.setup_required ? "/api/auth/setup" : "/api/auth/login",
                {
                    method: "POST",
                    headers: { "Content-Type": "application/json" },
                    body: JSON.stringify({ username, password }),
                },
            );
            return response.ok;
        },
        { username, password },
    );
    expect(
        result,
        "fresh installation accepts its one fixture credential",
    ).toBe(true);
    await page.goto("/");
    await expect(page.locator("#logout-btn")).toBeVisible();
}
async function signIn(page) {
    await page.locator("#username").fill(username);
    await page.locator("#password").fill(password);
    await page.getByRole("button", { name: "Sign in" }).click();
    await expect(page).toHaveURL(new URL("/", page.url()).href);
}
module.exports = {
    test,
    expect,
    username,
    password,
    changedPassword,
    ensureAuthenticatedSession,
    signIn,
};
