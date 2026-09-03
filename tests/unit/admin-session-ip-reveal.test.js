const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

const {
    normalizeVisibleSeconds,
    remainingVisibilitySeconds
} = require("../../static/js/admin-session-ip-reveal.js");

test("session IP reveal bounds temporary visibility without changing its defaults", () => {
    assert.equal(normalizeVisibleSeconds(undefined), 30);
    assert.equal(normalizeVisibleSeconds(0), 30);
    assert.equal(normalizeVisibleSeconds(-5), 1);
    assert.equal(normalizeVisibleSeconds(12), 12);
    assert.equal(normalizeVisibleSeconds("45"), 30);

    assert.equal(remainingVisibilitySeconds(31_000, 1_000), 30);
    assert.equal(remainingVisibilitySeconds(1_001, 1_000), 1);
    assert.equal(remainingVisibilitySeconds(999, 1_000), 0);
});

test("Admin delegates the session IP reveal modal lifecycle to its focused module", () => {
    const root = path.resolve(__dirname, "../..");
    const moduleSource = fs.readFileSync(path.join(root, "static/js/admin-session-ip-reveal.js"), "utf8");
    const adminSource = fs.readFileSync(path.join(root, "static/js/admin.js"), "utf8");
    const templateSource = fs.readFileSync(path.join(root, "templates/admin.html"), "utf8");

    [
        "function openSessionIPRevealModal(",
        "function closeSessionIPRevealModal(",
        "function hideTemporarySessionIPReveal(",
        "async function submitSessionIPReveal("
    ].forEach(marker => assert.equal(moduleSource.includes(marker), true, marker));
    assert.doesNotMatch(adminSource, /const sessionIPReveal\s*=\s*\{/);
    assert.doesNotMatch(adminSource, /function (open|close|hideTemporary)SessionIPReveal/);
    assert.doesNotMatch(adminSource, /async function submitSessionIPReveal/);

    const moduleIndex = templateSource.indexOf('/static/js/admin-session-ip-reveal.js');
    const adminIndex = templateSource.indexOf('/static/js/admin.js');
    assert.notEqual(moduleIndex, -1);
    assert.ok(moduleIndex < adminIndex, "session IP reveal module must load before admin.js");
});
