const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const test = require("node:test");

test("shared styles use the Segoe UI stack for every font role", () => {
    const root = path.resolve(__dirname, "../..");
    for (const relativePath of ["static/css/base.css", "static/css/maintenance.css"]) {
        const source = fs.readFileSync(path.join(root, relativePath), "utf8");
        assert.match(source, /--sans:\s*"Segoe UI"/, `${relativePath} must define Segoe UI first`);
        assert.match(source, /--mono:\s*var\(--sans\)/, `${relativePath} must use the shared Segoe UI stack for monospace roles`);
    }
});
