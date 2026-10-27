// Run each discovered business scenario in its own Playwright invocation. A
// zero-test grep, skip, retry or failed fixture teardown is a validation failure.
const { spawn } = require("node:child_process");
const fs = require("node:fs/promises");
const path = require("node:path");
const [binaryArg, outputArg] = process.argv.slice(2);
if (!binaryArg || !outputArg)
    throw new Error("Pass the disposable Go test binary and output directory");
const cli = path.resolve("node_modules/@playwright/test/cli.js");
const output = path.resolve(outputArg);
const environment = {
    ...process.env,
    SLU_E2E_TEST_BINARY: path.resolve(binaryArg),
};
async function run(args, file, extra = {}) {
    const log = await fs.open(file, "w"),
        errors = await fs.open(file + ".stderr", "w");
    try {
        const child = spawn(process.execPath, [cli, ...args], {
            env: { ...environment, ...extra },
            stdio: ["ignore", log.fd, errors.fd],
        });
        return await new Promise((resolve, reject) => {
            child.once("error", reject);
            child.once("exit", (code, signal) => resolve({ code, signal }));
        });
    } finally {
        await log.close();
        await errors.close();
    }
}
(async () => {
    await fs.mkdir(output, { recursive: true });
    const listFile = path.join(output, "discovered.json");
    const listed = await run(["test", "--list", "--reporter=json"], listFile);
    if (listed.code !== 0) throw new Error("Scenario discovery failed");
    const manifest = JSON.parse(await fs.readFile(listFile, "utf8")),
        specs = [];
    function visit(suites) {
        for (const suite of suites) {
            specs.push(...(suite.specs || []));
            visit(suite.suites || []);
        }
    }
    visit(manifest.suites);
    if (!specs.length) throw new Error("No scenarios discovered");
    const results = [];
    let cursor = 0;
    async function worker() {
        while (cursor < specs.length) {
            const index = cursor++,
                spec = specs[index],
                file = path.join(
                    output,
                    `${String(index + 1).padStart(3, "0")}.json`,
                );
            const title =
                spec.title.replace(/[.*+?^${}()|[\]\\]/g, "\\$&") + "$";
            const exited = await run(
                [
                    "test",
                    path.join("tests/e2e", spec.file),
                    "--grep",
                    title,
                    "--workers=1",
                    "--retries=0",
                    "--reporter=json",
                ],
                file,
                {
                    SLU_E2E_OUTPUT_DIR: path.join(
                        output,
                        `results-${index + 1}`,
                    ),
                },
            );
            let stats;
            try {
                stats = JSON.parse(await fs.readFile(file, "utf8")).stats;
            } catch (_) {}
            const passed =
                exited.code === 0 &&
                stats?.expected === 1 &&
                stats?.unexpected === 0 &&
                stats?.skipped === 0 &&
                stats?.flaky === 0;
            results.push({
                file: spec.file,
                title: spec.title,
                exit: exited.code,
                executed: stats
                    ? stats.expected + stats.unexpected + stats.flaky
                    : 0,
                passed,
                skipped: stats?.skipped ?? null,
            });
            console.log(
                `${index + 1}/${specs.length}: ${passed ? "passed" : "FAILED"} ${spec.file}: ${spec.title}`,
            );
        }
    }
    await Promise.all([worker(), worker()]);
    const report = {
        discovered: specs.length,
        invocations: results.length,
        executed: results.reduce((n, r) => n + r.executed, 0),
        passed: results.filter((r) => r.passed).length,
        failed: results.filter((r) => !r.passed).length,
        results,
    };
    await fs.writeFile(
        path.join(output, "summary.json"),
        JSON.stringify(report, null, 2) + "\n",
    );
    if (report.failed) process.exitCode = 1;
})().catch((error) => {
    console.error(error);
    process.exitCode = 1;
});
