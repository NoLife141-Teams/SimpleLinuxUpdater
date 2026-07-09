const test = require("node:test");
const assert = require("node:assert/strict");

const { createStore } = require("../../static/js/status-page-interaction.js");

test("snapshot intake clones adapter-owned data", () => {
    const store = createStore();
    const servers = [{ name: "alpha", status: "idle", tags: ["prod"] }];
    const dashboard = { servers: [{ name: "alpha", timeline: { state: "idle" } }] };

    store.dispatch({ type: "serversSnapshotReceived", servers });
    store.dispatch({ type: "dashboardSnapshotReceived", snapshot: dashboard });
    servers[0].status = "error";
    servers[0].tags.push("mutated");
    dashboard.servers[0].timeline.state = "error";

    assert.deepEqual(store.getServer("alpha"), { name: "alpha", status: "idle", tags: ["prod"] });
    assert.deepEqual(store.getDashboardServer("alpha"), { name: "alpha", timeline: { state: "idle" } });
});

test("canonical and legacy dashboard action data produce the same action view", () => {
    const canonicalStore = createStore();
    canonicalStore.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "pending_approval" }] });
    canonicalStore.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: {
            servers: [{
                name: "alpha",
                actions: {
                    approve_all: {
                        enabled: true,
                        reason: "",
                        readiness: "",
                        blocking_status: ""
                    }
                }
            }]
        }
    });

    const legacyStore = createStore();
    legacyStore.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "pending_approval" }] });
    legacyStore.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: { servers: [{ name: "alpha", approval_triage: { can_approve_all: true } }] }
    });

    assert.deepEqual(legacyStore.getAction("alpha", "approve_all"), canonicalStore.getAction("alpha", "approve_all"));
});

test("malformed canonical actions fall back to legacy eligibility", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: {
            servers: [{
                name: "alpha",
                actions: { update: { enabled: "yes" } },
                approval_triage: { can_run_checks: true }
            }]
        }
    });

    assert.equal(store.getAction("alpha", "update").enabled, true);
});

test("navigation restore validates persisted filters and emits persistence as data", () => {
    const store = createStore();
    store.dispatch({
        type: "navigationRestored",
        value: {
            search: "prod",
            statusFilter: "not-a-status",
            authFilter: "key",
            groupBy: "tag",
            pageSize: "50",
            fleetQuickFilter: "high_risk",
            fleetTagFilter: "critical",
            selectedServerName: "alpha"
        }
    });

    assert.deepEqual(store.getView().filters, {
        search: "prod",
        status: "",
        auth: "key",
        groupBy: "tag",
        quick: "high_risk",
        tag: "critical"
    });
    assert.equal(store.getView().pageSize, 50);

    const effects = store.dispatch({ type: "filtersChanged", patch: { search: "ops" } });
    assert.deepEqual(effects.find(effect => effect.type === "persistFilters").value.search, "ops");
});

test("navigation projection keeps filtered selections and groups the visible page", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [
            { name: "beta", status: "idle", tags: ["prod"], has_key: true },
            { name: "alpha", status: "error", tags: ["dev"], has_password: true }
        ]
    });
    store.dispatch({ type: "selectionChanged", name: "alpha", selected: true });
    store.dispatch({ type: "selectionChanged", name: "beta", selected: true });
    store.dispatch({ type: "filtersChanged", patch: { status: "idle", groupBy: "tag" } });

    const view = store.getView();
    assert.deepEqual(view.visibleServers.map(server => server.name), ["beta"]);
    assert.deepEqual(view.visibleSelectedNames, ["beta"]);
    assert.deepEqual(view.hiddenSelectedNames, ["alpha"]);
    assert.deepEqual(view.groups.map(group => group.key), ["prod"]);
    assert.equal(view.primaryServerName, "beta");
});

test("server removal prunes selection and closes an invalid drawer", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [
            { name: "alpha", status: "idle" },
            { name: "beta", status: "pending_approval", pending_updates: [{ package: "curl" }] }
        ]
    });
    store.dispatch({ type: "selectionChanged", name: "beta", selected: true });
    store.dispatch({ type: "primaryServerSelected", name: "beta" });
    store.dispatch({ type: "drawerOpened", name: "beta", tab: "pending" });
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });

    const view = store.getView();
    assert.deepEqual(view.selectedNames, []);
    assert.equal(view.primaryServerName, "alpha");
    assert.equal(view.drawer.open, false);
});

test("pending drawer tab falls back to logs when pending details disappear", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [{ name: "alpha", status: "pending_approval", pending_updates: [{ package: "curl" }] }]
    });
    store.dispatch({ type: "drawerOpened", name: "alpha", tab: "pending" });
    assert.equal(store.getView().drawer.tab, "pending");

    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle", pending_updates: [] }] });
    assert.equal(store.getView().drawer.tab, "logs");
});

test("refresh requests coalesce per stream and retain the highest queued priority", () => {
    const store = createStore();
    const first = store.dispatch({ type: "refreshRequested", stream: "servers", priority: "deferable", reason: "poll" });
    const firstFetch = first.find(effect => effect.type === "fetchSnapshot");
    assert.equal(firstFetch.requestId, 1);

    assert.equal(store.dispatch({ type: "refreshRequested", stream: "servers", priority: "deferable", reason: "poll" }).length, 0);
    assert.equal(store.dispatch({ type: "refreshRequested", stream: "servers", priority: "immediate", reason: "sse" }).length, 0);

    const completion = store.dispatch({
        type: "serversSnapshotReceived",
        requestId: 1,
        servers: [{ name: "alpha", status: "idle" }]
    });
    const queuedFetch = completion.find(effect => effect.type === "fetchSnapshot");
    assert.equal(queuedFetch.requestId, 2);
    assert.equal(queuedFetch.priority, "immediate");
});

test("out-of-order responses are discarded independently per stream", () => {
    const store = createStore();
    store.dispatch({ type: "refreshRequested", stream: "servers" });
    store.dispatch({ type: "refreshRequested", stream: "servers", priority: "immediate" });
    store.dispatch({ type: "serversSnapshotReceived", requestId: 1, servers: [{ name: "alpha", status: "idle" }] });

    assert.deepEqual(store.dispatch({
        type: "serversSnapshotReceived",
        requestId: 1,
        servers: [{ name: "stale", status: "error" }]
    }), []);
    assert.deepEqual(store.getView().servers.map(server => server.name), ["alpha"]);

    store.dispatch({ type: "dashboardSnapshotReceived", snapshot: { servers: [{ name: "alpha", marker: "dashboard" }] } });
    assert.equal(store.getDashboardServer("alpha").marker, "dashboard");
});

test("snapshot failure preserves the last successful view and records sync metadata", () => {
    const store = createStore();
    store.dispatch({ type: "refreshRequested", stream: "servers" });
    store.dispatch({ type: "serversSnapshotReceived", requestId: 1, servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({ type: "refreshRequested", stream: "servers" });
    store.dispatch({ type: "snapshotFailed", stream: "servers", requestId: 2, error: "offline" });

    assert.deepEqual(store.getView().servers.map(server => server.name), ["alpha"]);
    assert.equal(store.getView().sync.streams.servers.lastError, "offline");
});

test("deferable snapshots render after interaction release while immediate snapshots render now", () => {
    const store = createStore();
    store.dispatch({ type: "interactionStarted" });
    store.dispatch({ type: "refreshRequested", stream: "servers", priority: "deferable" });
    const deferred = store.dispatch({
        type: "serversSnapshotReceived",
        requestId: 1,
        servers: [{ name: "alpha", status: "idle" }]
    });
    assert.equal(deferred.some(effect => effect.type === "render"), false);
    assert.equal(store.getView().sync.deferredRender, true);

    const ended = store.dispatch({ type: "interactionEnded", delayMs: 350 });
    assert.equal(ended[0].type, "scheduleInteractionRelease");
    const released = store.dispatch({ type: "interactionReleased" });
    assert.equal(released.some(effect => effect.type === "render"), true);

    store.dispatch({ type: "interactionStarted" });
    store.dispatch({ type: "refreshRequested", stream: "servers", priority: "immediate" });
    const immediate = store.dispatch({
        type: "serversSnapshotReceived",
        requestId: 2,
        servers: [{ name: "alpha", status: "done" }]
    });
    assert.equal(immediate.some(effect => effect.type === "render" && effect.priority === "immediate"), true);
});
