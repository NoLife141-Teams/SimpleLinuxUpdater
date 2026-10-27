const { chromium } = require(process.cwd() + "/node_modules/@playwright/test");
const { startInstallation } = require(
    process.cwd() + "/tests/e2e/support/installation",
);
const {
    stubAdminApi,
    stubManageApi,
    stubDashboardApi,
    makeServer,
    makePendingUpdates,
} = require(process.cwd() + "/tests/e2e/support/builders");
const fs = require("node:fs/promises"),
    path = require("node:path");
(async () => {
    const [binary, output] = process.argv.slice(2);
    if (!binary || !output)
        throw new Error(
            "Pass the disposable Go test binary and output directory",
        );
    process.env.SLU_E2E_TEST_BINARY = path.resolve(binary);
    await fs.mkdir(output, { recursive: true });
    const installation = await startInstallation();
    let browser;
    const evidence = [];
    try {
        browser = await chromium.launch();
        for (const [size, viewport] of [
            ["desktop", { width: 1440, height: 1000 }],
            ["mobile", { width: 390, height: 844 }],
        ])
            for (const domain of ["status", "admin", "manage"]) {
                const context = await browser.newContext({
                    baseURL: installation.url,
                    viewport,
                    reducedMotion: "reduce",
                    timezoneId: "UTC",
                    locale: "en-US",
                });
                try {
                    await context.request.post("/api/auth/setup", {
                        headers: { Origin: installation.url },
                        data: { username: "admin", password: "StrongPass1234" },
                    });
                    // Setup only happens once; subsequent contexts use the same exact fixture credential.
                    await context.request.post("/api/auth/login", {
                        headers: { Origin: installation.url },
                        data: { username: "admin", password: "StrongPass1234" },
                    });
                    const page = await context.newPage();
                    const errors = [];
                    const consoleErrors = [];
                    page.on("console", (message) => {
                        if (message.type() === "error")
                            consoleErrors.push(message.text());
                    });
                    page.on("pageerror", (e) => errors.push(String(e)));
                    await page.clock.setFixedTime(
                        new Date("2026-05-28T12:00:00Z"),
                    );
                    const servers = [
                        makeServer(
                            "demo-host",
                            "pending_approval",
                            makePendingUpdates(3),
                        ),
                        makeServer("standby-host"),
                    ];
                    if (domain === "admin") await stubAdminApi(page, {});
                    if (domain === "manage")
                        await stubManageApi(page, { servers });
                    if (domain === "status")
                        await stubDashboardApi(page, () => servers);
                    await page.goto(domain === "status" ? "/" : `/${domain}`);
                    await page.waitForTimeout(650);
                    await page.evaluate(() => window.scrollTo(0, 0));
                    await page.screenshot({
                        path: path.join(output, `${domain}-${size}.png`),
                        fullPage: true,
                    });
                    const overflow = await page.evaluate(
                        () => document.documentElement.scrollWidth > innerWidth,
                    );
                    evidence.push({
                        domain,
                        size,
                        errors,
                        consoleErrors,
                        overflow,
                    });
                    if (errors.length || consoleErrors.length || overflow)
                        throw new Error(
                            `${domain} ${size} presentation regression: ${JSON.stringify({ errors, consoleErrors, overflow })}`,
                        );
                } finally {
                    await context.close();
                }
            }
    } finally {
        if (browser) await browser.close();
        await installation.stop();
    }
    await fs.writeFile(
        path.join(output, "behavior.json"),
        JSON.stringify(evidence, null, 2) + "\n",
    );
})().catch((e) => {
    console.error(e);
    process.exitCode = 1;
});
