const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { createStore } = require("../../static/js/manage-page-interaction.js");

test("inventory projection owns filters, sort, grouping, pagination, and global-key facts", () => {
    const store = createStore();
    store.dispatch({ type: "inventorySnapshotReceived", items: [{ name: "beta", host: "b", user: "u", tags: ["prod"], has_password: true }, { name: "alpha", host: "a", user: "u", tags: ["dev"], has_key: true }] });
    store.dispatch({ type: "globalKeySnapshotReceived", hasKey: true });
    store.dispatch({ type: "filtersChanged", patch: { auth: "key", group: "auth", pageSize: 10 } });
    const view = store.getView();
    assert.deepEqual(view.inventory.items.map(item => item.name), ["alpha", "beta"]);
    assert.equal(view.inventory.groups[0].key, "key / no password");
});

test("streams retain accepted views and discard stale responses", () => {
    const store = createStore();
    const first = store.dispatch({ type: "snapshotRequested", stream: "inventory" }).find(effect => effect.type === "fetchSnapshot");
    store.dispatch({ type: "inventorySnapshotReceived", requestID: first.requestID, items: [{ name: "alpha" }] });
    store.dispatch({ type: "snapshotRequested", stream: "inventory" });
    store.dispatch({ type: "inventorySnapshotReceived", requestID: first.requestID, items: [{ name: "stale" }] });
    assert.deepEqual(store.getView().inventory.items.map(item => item.name), ["alpha"]);
});

test("editor sessions invalidate stale host-key results and command plans exclude competitors", () => {
    const store = createStore();
    store.dispatch({ type: "inventorySnapshotReceived", items: [{ name: "alpha", host: "a", user: "u", port: 22 }] });
    store.dispatch({ type: "editorOpened", name: "alpha" });
    const session = store.getView().editor.sessionID;
    store.dispatch({ type: "editorChanged", patch: { host: "b" } });
    store.dispatch({ type: "hostKeyReceived", sessionID: session, host: "a", port: 22, hostKey: { fingerprint: "old" } });
    assert.equal(store.getView().editor.hostKey, null);
    const first = store.dispatch({ type: "commandRequested", command: "saveEditor" }).find(effect => effect.type === "executeCommand");
    assert.equal(store.dispatch({ type: "commandRequested", command: "saveEditor" })[0].type, "commandRejected");
    store.dispatch({ type: "commandCompleted", plan: first.plan });
});

test("audit pagination corrects stale pages and selection stays logical", () => {
    const store = createStore();
    store.dispatch({ type: "auditQueryChanged", patch: { page: 3, pageSize: 20 } });
    const request = store.dispatch({ type: "snapshotRequested", stream: "audit" }).find(effect => effect.type === "fetchSnapshot");
    const effects = store.dispatch({ type: "auditSnapshotReceived", requestID: request.requestID, data: { items: [{ id: 1 }], total: 1 } });
    assert.equal(store.getView().audit.query.page, 1);
    assert.equal(effects.some(effect => effect.type === "fetchSnapshot"), true);
    store.dispatch({ type: "auditDetailSelected", id: 1 });
    assert.equal(store.getView().audit.selectedID, "1");
});

test("Manage adapter no longer declares legacy interaction globals", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/manage.js"), "utf8");
    assert.doesNotMatch(source, /let\s+manageServers\s*=/);
    assert.doesNotMatch(source, /let\s+editingServerName\s*=/);
    assert.doesNotMatch(source, /let\s+auditEvents\s*=/);
    assert.doesNotMatch(source, /let\s+editKnownHostState\s*=/);
});
