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

test("stream failure retains the last accepted source and reports only that source", () => {
    const store = createStore();
    const accepted = store.dispatch({ type: "snapshotRequested", stream: "inventory" })
        .find(effect => effect.type === "fetchSnapshot");
    store.dispatch({
        type: "inventorySnapshotReceived",
        requestID: accepted.requestID,
        items: [{ name: "accepted", host: "host", user: "root" }]
    });
    const failing = store.dispatch({ type: "snapshotRequested", stream: "inventory" })
        .find(effect => effect.type === "fetchSnapshot");
    const effects = store.dispatch({
        type: "snapshotFailed",
        stream: "inventory",
        requestID: failing.requestID,
        error: "offline"
    });

    assert.deepEqual(store.getView().inventory.items.map(server => server.name), ["accepted"]);
    assert.equal(store.getView().streams.inventory.lastError, "offline");
    assert.equal(store.getView().streams.audit.lastError, "");
    assert.deepEqual(effects.find(effect => effect.type === "announce"), {
        type: "announce",
        scope: "inventory",
        message: "offline",
        error: true
    });
});

test("queued refresh starts after the active request settles", () => {
    const store = createStore();
    const first = store.dispatch({ type: "snapshotRequested", stream: "audit", payload: { reason: "initial" } })
        .find(effect => effect.type === "fetchSnapshot");
    assert.deepEqual(store.dispatch({ type: "snapshotRequested", stream: "audit", payload: { reason: "poll" } }), []);

    const effects = store.dispatch({
        type: "auditSnapshotReceived",
        requestID: first.requestID,
        data: { items: [], total: 0 }
    });
    const queued = effects.find(effect => effect.type === "fetchSnapshot");
    assert.equal(queued.stream, "audit");
    assert.equal(queued.reason, "poll");
    assert.notEqual(queued.requestID, first.requestID);
});

test("command effects and projections stay transport neutral", () => {
    const store = createStore();
    store.dispatch({
        type: "inventorySnapshotReceived",
        items: [{ name: "alpha", host: "host", user: "root", port: 22 }]
    });
    store.dispatch({ type: "editorOpened", name: "alpha" });
    const effects = store.dispatch({ type: "commandRequested", command: "saveEditor" });
    const serialized = JSON.stringify({ effects, view: store.getView() });

    for (const forbidden of ["/api/", "FormData", "HTMLElement", "querySelector", "fetch("]) {
        assert.equal(serialized.includes(forbidden), false, `public contract leaked ${forbidden}`);
    }
});

test("accepted projections are immutable copies from the caller perspective", () => {
    const store = createStore();
    store.dispatch({
        type: "inventorySnapshotReceived",
        items: [{ name: "alpha", host: "host", user: "root", tags: ["prod"] }]
    });
    const first = store.getView();
    first.inventory.items[0].name = "mutated";
    first.inventory.items[0].tags.push("caller");

    const second = store.getView();
    assert.equal(second.inventory.items[0].name, "alpha");
    assert.deepEqual(second.inventory.items[0].tags, ["prod"]);
});

test("inventory projection is the complete source for rows, lookup, and paging", () => {
    const store = createStore();
    const servers = Array.from({ length: 12 }, (_, index) => ({
        name: `host-${String(index + 1).padStart(2, "0")}`,
        host: `192.0.2.${index + 1}`,
        user: "root"
    }));
    store.dispatch({ type: "inventorySnapshotReceived", items: servers });
    store.dispatch({ type: "filtersChanged", patch: { pageSize: 10 } });

    const view = store.getView();
    assert.equal(view.inventory.allItems.length, 12);
    assert.equal(view.inventory.items.length, 10);
    assert.equal(view.inventory.allItems.find(server => server.name === "host-12").host, "192.0.2.12");
    assert.equal(view.inventory.totalPages, 2);
});

test("Manage adapter owns no accepted inventory cache or paging state", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/manage.js"), "utf8");
    assert.doesNotMatch(source, /\bserverCache\b/);
    assert.doesNotMatch(source, /\bmanageServers\b/);
    assert.doesNotMatch(source, /\bmanageGlobalKeyAvailable\b/);
    assert.doesNotMatch(source, /(?:^|[^\w.])sortKey\s*=/m);
    assert.doesNotMatch(source, /(?:^|[^\w.])sortDir\s*=/m);
    assert.doesNotMatch(source, /(?:^|[^\w.])page\s*=/m);
});

test("server command eligibility is owned at the Manage Page Interaction seam", () => {
    const store = createStore();
    const invalidCreate = store.dispatch({
        type: "commandRequested",
        command: "createServer",
        payload: { name: "", host: "", user: "" }
    });
    assert.equal(invalidCreate[0].type, "commandRejected");
    assert.deepEqual(invalidCreate[0].invalidFields, ["name", "host", "user"]);

    const validCreate = store.dispatch({
        type: "commandRequested",
        command: "createServer",
        payload: { name: " alpha ", host: " host ", port: "2222", user: " root ", tags: ["prod", "prod"], hasKeyFile: true, trustHostKey: true }
    }).find(effect => effect.type === "executeCommand");
    assert.deepEqual(validCreate.plan.payload, {
        name: "alpha",
        host: "host",
        port: 2222,
        user: "root",
        tags: ["prod"],
        trustHostKey: true,
        uploadKey: true
    });
    assert.equal(store.dispatch({
        type: "commandRequested",
        command: "createServer",
        payload: { name: "beta", host: "host", user: "root" }
    })[0].type, "commandRejected");
    store.dispatch({ type: "commandCompleted", plan: validCreate.plan });

    const deletion = store.dispatch({
        type: "commandRequested",
        command: "deleteServer",
        payload: { serverName: "alpha" }
    }).find(effect => effect.type === "executeCommand");
    assert.equal(deletion.plan.key, "deleteServer:alpha");
    assert.equal(store.dispatch({
        type: "commandRequested",
        command: "uploadServerKey",
        payload: { serverName: "alpha" }
    })[0].type, "commandRejected");
});

test("host-key responses require the active request, editor session, host, and port", () => {
    const store = createStore();
    store.dispatch({
        type: "inventorySnapshotReceived",
        items: [{ name: "alpha", host: "old.example", port: 22, user: "root" }]
    });
    store.dispatch({ type: "editorOpened", name: "alpha" });
    const sessionID = store.getView().editor.sessionID;
    const first = store.dispatch({ type: "snapshotRequested", stream: "hostKey" })
        .find(effect => effect.type === "fetchSnapshot");
    store.dispatch({
        type: "hostKeyReceived",
        requestID: first.requestID,
        sessionID,
        host: "old.example",
        port: 22,
        hostKey: { fingerprint: "SHA256:old", alreadyTrusted: true }
    });
    assert.equal(store.getView().editor.hostKey.fingerprint, "SHA256:old");

    store.dispatch({ type: "editorChanged", patch: { host: "new.example" } });
    const second = store.dispatch({ type: "snapshotRequested", stream: "hostKey" })
        .find(effect => effect.type === "fetchSnapshot");
    store.dispatch({
        type: "hostKeyReceived",
        requestID: first.requestID,
        sessionID,
        host: "old.example",
        port: 22,
        hostKey: { fingerprint: "SHA256:stale", alreadyTrusted: true }
    });
    assert.equal(store.getView().editor.hostKey, null);
    store.dispatch({
        type: "hostKeyReceived",
        requestID: second.requestID,
        sessionID,
        host: "new.example",
        port: 22,
        hostKey: { fingerprint: "SHA256:new", alreadyTrusted: false }
    });
    assert.equal(store.getView().editor.hostKey.fingerprint, "SHA256:new");
});

test("Manage adapter owns no accepted editor, host-key, or save state", () => {
    const source = fs.readFileSync(path.join(__dirname, "../../static/js/manage.js"), "utf8");
    for (const legacy of ["editingServerName", "editSaveInProgress", "editKnownHostState"]) {
        assert.doesNotMatch(source, new RegExp(`\\b${legacy}\\b`));
    }
    assert.doesNotMatch(source, /manageAdapterState\.(?:hostKeyModalPromise|hostKeyModalResolvers|editKnownHostCheckPromise)/);
});
