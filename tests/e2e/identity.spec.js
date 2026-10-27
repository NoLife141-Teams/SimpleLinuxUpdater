const { test, expect } = require("./support/fixtures");
const crypto = require("node:crypto");

// Identity workflows use the same per-scenario installation fixture.
test.describe("named identities on isolated installations", () => {
    let baseURL;
    const password = "SyntheticIdentity123!";
    test.beforeEach(async ({ baseURL, page, installation }) => {
        baseURL = installation.url;
        await page.goto(`${baseURL}/setup`);
        await page.locator("#username").fill("admin");
        await page.locator("#password").fill(password);
        await page.locator("#password-confirm").fill(password);
        await page.getByRole("button", { name: /create.*account/i }).click();
        await expect(page).toHaveURL(`${baseURL}/`);
    });
    async function api(page, route, method = "GET", body) {
        return page.evaluate(
            async ({ route, method, body }) => {
                const response = await window.__nativeFetch(route, {
                    method,
                    headers: { "Content-Type": "application/json" },
                    body: body === undefined ? undefined : JSON.stringify(body),
                });
                return {
                    status: response.status,
                    data: await response.json().catch(() => ({})),
                };
            },
            { route, method, body },
        );
    }
    function totp(secret) {
        const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
        let bits = "";
        for (const char of secret)
            bits += alphabet.indexOf(char).toString(2).padStart(5, "0");
        const key = Buffer.from(
            bits.match(/.{8}/g).map((byte) => parseInt(byte, 2)),
        );
        const counter = Buffer.alloc(8);
        counter.writeBigUInt64BE(BigInt(Math.floor(Date.now() / 30000)));
        const hash = crypto.createHmac("sha1", key).update(counter).digest();
        const offset = hash[19] & 15;
        return String(
            (hash.readUInt32BE(offset) & 0x7fffffff) % 1000000,
        ).padStart(6, "0");
    }
    test("accounts, scoped personal tokens, single-display secrets and responsive views", async ({
        baseURL,
        page,
    }, info) => {
        await page.goto(`${baseURL}/account`);
        await expect(page.locator("#identity-summary")).toContainText(
            "admin · admin",
        );
        await page.locator("#new-account-name").fill("reader");
        await page.locator("#new-account-password").fill(password);
        await page
            .getByRole("button", { name: "Create account", exact: true })
            .click();
        await expect(page.locator("#account-list")).toContainText("reader");
        await page.locator("#token-name").fill("monitoring");
        await page
            .getByRole("button", { name: "Create personal token" })
            .click();
        await expect(page.locator("#token-secret")).toContainText("slu_");
        const secret = await page.locator("#token-secret").textContent();
        const result = await page.evaluate(async (secret) => {
            const read = await fetch("/api/servers", {
                headers: { Authorization: `Bearer ${secret}` },
            });
            const write = await fetch("/api/update/missing", {
                method: "POST",
                headers: { Authorization: `Bearer ${secret}` },
            });
            return { read: read.status, write: write.status };
        }, secret);
        expect(result).toEqual({ read: 200, write: 403 });
        await page
            .getByRole("button", { name: "I have saved the secret" })
            .click();
        await expect(page.locator("#token-secret")).toBeEmpty();
        await expect(page.locator("#token-list")).toContainText("monitoring");
        // Screenshots intentionally contain neither passwords, token nor MFA secrets.
        await page.evaluate(() => window.scrollTo(0, 0));
        await page.screenshot({
            path: info.outputPath("account-desktop.png"),
            fullPage: true,
        });
        await page.setViewportSize({ width: 390, height: 844 });
        await page.screenshot({
            path: info.outputPath("account-mobile.png"),
            fullPage: true,
        });
        expect(
            await page.evaluate(
                () => document.documentElement.scrollWidth <= window.innerWidth,
            ),
        ).toBeTruthy();
        await page.getByRole("button", { name: "Revoke", exact: true }).click();
        await expect(page.locator("#token-list")).toContainText("revoked");
        expect((await api(page, "/api/servers", "GET")).status).toBe(200);
        await page.locator("#logout-btn").click();
        await expect(page).toHaveURL(`${baseURL}/login`);
        await page.locator("#username").fill("reader");
        await page.locator("#password").fill(password);
        await page
            .getByRole("button", { name: "Sign in", exact: true })
            .click();
        await expect(page).toHaveURL(`${baseURL}/`);
        await expect(
            page.getByRole("link", { name: "Admin", exact: true }),
        ).toHaveCount(0);
        await expect(
            page.getByRole("link", { name: "Manage Servers", exact: true }),
        ).toHaveCount(0);
        expect((await api(page, "/api/update/missing", "POST")).status).toBe(
            403,
        );
        await page.goto(`${baseURL}/account`);
        await expect(page.locator("#identity-summary")).toContainText(
            "reader · reader",
        );
        await expect(page.locator("#accounts-section")).toBeHidden();
    });
    test("status controls and background reads follow reader and operator roles", async ({
        baseURL,
        page,
    }) => {
        for (const role of ["reader", "operator"]) {
            expect(
                (
                    await api(page, "/api/auth/accounts", "POST", {
                        username: role,
                        password,
                        role,
                    })
                ).status,
            ).toBe(201);
        }
        expect(
            (
                await api(page, "/api/servers", "POST", {
                    name: "presentation-host",
                    host: "synthetic.invalid",
                    user: "root",
                    pass: "SyntheticSSH123!",
                })
            ).status,
        ).toBe(201);
        const forbiddenReads = [];
        page.on("response", (response) => {
            if (
                response.request().method() === "GET" &&
                response.status() === 403
            )
                forbiddenReads.push(new URL(response.url()).pathname);
        });
        for (const role of ["reader", "operator"]) {
            await page.locator("#logout-btn").click();
            await expect(page).toHaveURL(`${baseURL}/login`);
            await page.locator("#username").fill(role);
            await page.locator("#password").fill(password);
            await page
                .getByRole("button", { name: "Sign in", exact: true })
                .click();
            await expect(page).toHaveURL(`${baseURL}/`);
            await expect(page.locator("body")).toHaveAttribute(
                "data-account-role",
                role,
            );
            await expect(page.locator("#servers-table")).toContainText(
                "presentation-host",
            );
            expect(
                await page.evaluate(() =>
                    window.canUseScope("maintenance:write"),
                ),
            ).toBe(role === "operator");
            expect(
                await page.evaluate(() => window.canUseScope("audit:read")),
            ).toBe(false);
            if (role === "reader") {
                await expect(
                    page.locator('[data-action="update-server"]:visible'),
                ).toHaveCount(0);
                await expect(
                    page.locator('[data-action="refresh-facts"]:visible'),
                ).toHaveCount(0);
                await expect(page.locator("#bulk-update")).toBeHidden();
            } else {
                await expect(
                    page
                        .locator('[data-action="refresh-facts"]:visible')
                        .first(),
                ).toBeVisible();
                await expect(
                    page.locator('[data-action="enable-apt"]:visible'),
                ).toHaveCount(0);
            }
        }
        expect(forbiddenReads).toEqual([]);
    });
    test("confirmed MFA challenge, recovery replay rejection and sensitive reauthentication", async ({
        baseURL,
        page,
    }) => {
        await page.goto(`${baseURL}/account`);
        await page
            .getByRole("button", { name: "Set up authenticator" })
            .click();
        await expect(page.locator("#mfa-secret")).toHaveText(/^[A-Z2-7]+$/);
        const secret = await page.locator("#mfa-secret").textContent();
        await page.locator("#mfa-confirm-code").fill(totp(secret));
        await page
            .getByRole("button", { name: "Enable MFA", exact: true })
            .click();
        await expect(page.locator("#recovery-codes")).not.toBeEmpty();
        const codes = (await page.locator("#recovery-codes").textContent())
            .trim()
            .split("\n");
        expect(codes).toHaveLength(10);
        await page.getByRole("link", { name: "Sign in again" }).click();
        await page.locator("#username").fill("admin");
        await page.locator("#password").fill(password);
        await page
            .getByRole("button", { name: "Sign in", exact: true })
            .click();
        await expect(page.locator("#mfa-login-form")).toBeVisible();
        const incomplete = await page.evaluate(async () =>
            (await fetch("/api/auth/status")).json(),
        );
        expect(incomplete.authenticated).toBe(false);
        await page.locator("#mfa-login-code").fill(codes[0]);
        await page.getByRole("button", { name: "Verify and sign in" }).click();
        await expect(page).toHaveURL(`${baseURL}/`);
        await page.goto(`${baseURL}/account`);
        await expect(page.locator("#mfa-status")).toHaveText(
            "Authenticator enabled.",
        );
        await page.locator("#reauth-password").fill(password);
        await page.locator("#reauth-code").fill(codes[0]);
        await page.getByRole("button", { name: "Confirm identity" }).click();
        await expect(page.locator("#account-status")).toContainText(
            "invalid authentication factor",
        );
        await page.locator("#reauth-code").fill(codes[1]);
        await page.getByRole("button", { name: "Confirm identity" }).click();
        await expect(page.locator("#account-status")).toContainText(
            "Identity confirmed",
        );
        await page
            .getByRole("button", { name: "Disable MFA", exact: true })
            .click();
        await expect(page).toHaveURL(`${baseURL}/login`);
        await page.locator("#username").fill("admin");
        await page.locator("#password").fill(password);
        await page
            .getByRole("button", { name: "Sign in", exact: true })
            .click();
        await expect(page).toHaveURL(`${baseURL}/`);
    });
});
