// Compatibility entry point for #475 qualification. Every browser scenario now
// owns its test router; there is no idle global webServer or fixed shared port.
const { spawn } = require("node:child_process");
const fs = require("node:fs/promises");
const path = require("node:path");
const [binaryArg, outputArg, ...playwrightArgs] = process.argv.slice(2);
if (!binaryArg || !outputArg)
    throw new Error("Pass a Go test binary and output directory");
(async () => {
    const output = path.resolve(outputArg);
    await fs.mkdir(output, { recursive: true });
    const child = spawn(
        process.execPath,
        [
            path.resolve("node_modules/@playwright/test/cli.js"),
            "test",
            "--workers=1",
            "--retries=0",
            ...playwrightArgs,
        ],
        {
            env: {
                ...process.env,
                SLU_E2E_TEST_BINARY: path.resolve(binaryArg),
                SLU_E2E_OUTPUT_DIR: path.join(output, "test-results"),
            },
            stdio: "inherit",
        },
    );
    const result = await new Promise((resolve, reject) => {
        child.once("error", reject);
        child.once("exit", (code, signal) => resolve({ code, signal }));
    });
    await fs.writeFile(
        path.join(output, "lifecycle.json"),
        JSON.stringify(
            {
                playwright: result,
                // A successful suite proves every fixture joined its server
                // with exit zero. Retain the existing lifecycle consumer field.
                server: {
                    code: result.code === 0 ? 0 : null,
                    signal: null,
                    scope: "per-scenario fixture teardown",
                },
                installation:
                    "fresh test-only router per scenario, joined by fixture teardown",
            },
            null,
            2,
        ) + "\n",
    );
    if (result.code !== 0)
        throw new Error(
            `Browser qualification failed: ${JSON.stringify(result)}`,
        );
})().catch((error) => {
    console.error(error);
    process.exitCode = 1;
});
