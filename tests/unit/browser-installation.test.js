const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const os = require("node:os");
const { readOwnedReadiness } = require("../e2e/support/installation");
async function readyFile(t) {
    const directory = await fs.mkdtemp(
        path.join(os.tmpdir(), "slu-readiness-476-"),
    );
    t.after(() => fs.rm(directory, { recursive: true, force: true }));
    return path.join(directory, "ready.json");
}

test("missing empty and partially written readiness remain retryable", async (t) => {
    const file = await readyFile(t);
    assert.equal(await readOwnedReadiness(file, process.pid), null);
    await fs.writeFile(file, "");
    assert.equal(await readOwnedReadiness(file, process.pid), null);
    await fs.writeFile(file, '{"pid":');
    assert.equal(await readOwnedReadiness(file, process.pid), null);
});
test("a complete readiness fact accepts its owned PID and loopback listener", async (t) => {
    const file = await readyFile(t),
        fact = { pid: process.pid, listen: "127.0.0.1:45678" };
    await fs.writeFile(file, JSON.stringify(fact));
    assert.deepEqual(await readOwnedReadiness(file, process.pid), fact);
});
test("a foreign readiness PID remains a fatal ownership error", async (t) => {
    const file = await readyFile(t);
    await fs.writeFile(
        file,
        JSON.stringify({ pid: process.pid + 1, listen: "127.0.0.1:45678" }),
    );
    await assert.rejects(
        readOwnedReadiness(file, process.pid),
        /Unowned listener/,
    );
});
test("a non-loopback readiness listener remains a fatal ownership error", async (t) => {
    const file = await readyFile(t);
    await fs.writeFile(
        file,
        JSON.stringify({ pid: process.pid, listen: "0.0.0.0:45678" }),
    );
    await assert.rejects(
        readOwnedReadiness(file, process.pid),
        /Unowned listener/,
    );
});
