const { spawnSync } = require("node:child_process");
const fs = require("node:fs");
const os = require("node:os");
const path = require("node:path");
module.exports = async () => {
    if (process.env.PLAYWRIGHT_WEBSERVER_COMMAND)
        throw new Error(
            "PLAYWRIGHT_WEBSERVER_COMMAND is retired: use SLU_E2E_TEST_BINARY with the disposable Go test composition. See docs/frontend-isolation.md.",
        );
    if (process.env.SLU_E2E_TEST_BINARY) return;
    const directory = fs.mkdtempSync(
        path.join(os.tmpdir(), "slu-browser-binary-476-"),
    );
    const binary = path.join(directory, "server.test");
    const result = spawnSync("go", ["test", "-c", "-o", binary, "."], {
        stdio: "inherit",
        env: { ...process.env, GOMAXPROCS: process.env.GOMAXPROCS || "2" },
    });
    if (result.status !== 0) {
        fs.rmSync(directory, { recursive: true, force: true });
        throw new Error(`Browser composition build exited ${result.status}`);
    }
    process.env.SLU_E2E_TEST_BINARY = binary;
    return () => fs.rmSync(directory, { recursive: true, force: true });
};
