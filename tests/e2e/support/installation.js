const { spawn } = require("node:child_process");
const fs = require("node:fs/promises");
const os = require("node:os");
const path = require("node:path");

// Each scenario gets its own process and all process-local state, a fresh
// temporary installation, and a port allocated atomically by the kernel.
async function startInstallation(binary = process.env.SLU_E2E_TEST_BINARY) {
    if (!binary)
        throw new Error(
            "SLU_E2E_TEST_BINARY must name the disposable Go test composition",
        );
    const directory = await fs.mkdtemp(
        path.join(os.tmpdir(), "slu-browser-476-"),
    );
    const ready = path.join(directory, "ready.json");
    const log = await fs.open(path.join(directory, "server.log"), "w");
    const child = spawn(
        path.resolve(binary),
        ["-test.run=^TestDashboardScalingE2EServe$", "-test.timeout=10m"],
        {
            cwd: path.resolve("."),
            env: {
                ...Object.fromEntries(
                    Object.entries(process.env).filter(
                        ([key]) =>
                            !key.startsWith("DEBIAN_UPDATER_") &&
                            !key.startsWith("SLU_SCALING_"),
                    ),
                ),
                TMPDIR: directory,
                GOMAXPROCS: "2",
                SLU_SCALING_E2E_LISTEN: "127.0.0.1:0",
                SLU_SCALING_E2E_READY: ready,
            },
            stdio: ["ignore", log.fd, log.fd],
        },
    );
    const exited = new Promise((resolve) => {
        child.once("error", (error) => resolve({ error: String(error) }));
        child.once("exit", (code, signal) => resolve({ code, signal }));
    });
    let stopped = false;
    async function stop() {
        if (stopped) return;
        stopped = true;
        if (child.exitCode === null) child.kill("SIGTERM");
        const timer = setTimeout(() => child.kill("SIGKILL"), 10000);
        const result = await exited;
        clearTimeout(timer);
        await log.close();
        const contents =
            result.code === 0
                ? ""
                : await fs.readFile(path.join(directory, "server.log"), "utf8");
        await fs.rm(directory, { recursive: true, force: true });
        if (result.code !== 0)
            throw new Error(
                `Disposable installation failed: ${JSON.stringify(result)}\n${contents}`,
            );
    }
    try {
        for (let attempt = 0; attempt < 300; attempt++) {
            try {
                const fact = JSON.parse(await fs.readFile(ready, "utf8"));
                if (
                    fact.pid !== child.pid ||
                    !/^127\.0\.0\.1:\d+$/.test(fact.listen)
                )
                    throw new Error("Unowned listener");
                return {
                    url: `http://${fact.listen}`,
                    directory,
                    pid: child.pid,
                    stop,
                };
            } catch (error) {
                if (error.code !== "ENOENT") throw error;
            }
            if (child.exitCode !== null)
                throw new Error(`Server exited ${child.exitCode}`);
            await new Promise((resolve) => setTimeout(resolve, 25));
        }
        throw new Error("Disposable installation startup timed out");
    } catch (error) {
        await stop();
        throw error;
    }
}
module.exports = { startInstallation };
