const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");

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

test("search projection preserves the existing trimmed matching behavior", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({ type: "filtersChanged", patch: { search: "  alpha  " } });
    assert.deepEqual(store.getView().visibleServers.map(server => server.name), ["alpha"]);
});

test("high-risk filtering falls back to server CVE data before dashboard intake", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [{ name: "alpha", status: "pending_approval", pending_updates: [{ cves: ["CVE-1"] }] }]
    });
    store.dispatch({ type: "filtersChanged", patch: { quick: "high_risk" } });
    assert.deepEqual(store.getView().visibleServers.map(server => server.name), ["alpha"]);
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

test("an action press keeps its logical target while a deferable snapshot is accepted", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({ type: "interactionStarted" });
    store.dispatch({ type: "refreshRequested", stream: "servers", priority: "deferable" });
    store.dispatch({ type: "serversSnapshotReceived", requestId: 1, servers: [{ name: "renamed", status: "idle" }] });
    store.dispatch({ type: "interactionEnded" });

    const plan = store.planAction("alpha", "update", { actionLabel: "update" });
    assert.equal(plan.enabled, true);
    store.dispatch({ type: "actionStarted", plan });
    assert.equal(store.getView().actions.inFlight.some(action => action.operationId === plan.id), true);
    assert.deepEqual(store.getView().servers.map(server => server.name), ["renamed"]);
});

test("a new action press refreshes its context when no render is deferred", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [{ name: "alpha", status: "idle" }, { name: "beta", status: "idle" }]
    });
    store.dispatch({ type: "selectionChanged", name: "alpha", selected: true });
    store.dispatch({ type: "interactionStarted" });
    store.dispatch({ type: "interactionEnded" });

    store.dispatch({ type: "selectionChanged", name: "beta", selected: true });
    store.dispatch({ type: "interactionStarted" });
    assert.deepEqual(store.planBulkAction("update").visibleNames, ["alpha", "beta"]);
});

test("single action plans contain canonical facts without transport details", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({
        type: "dashboardSnapshotReceived",
        snapshot: { servers: [{ name: "alpha", actions: { update: { enabled: true, reason: "Ready", readiness: "ready" } } }] }
    });

    const plan = store.planAction("alpha", "update", { actionLabel: "update" });
    assert.equal(plan.enabled, true);
    assert.equal(plan.actionKey, "update");
    assert.deepEqual(plan.serverNames, ["alpha"]);
    assert.equal("url" in plan, false);
    assert.equal("callback" in plan, false);
});

test("an in-flight host rejects competing single and bulk action plans", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    store.dispatch({ type: "selectionChanged", name: "alpha", selected: true });
    const first = store.planAction("alpha", "update", { actionLabel: "update" });
    store.dispatch({ type: "actionStarted", plan: first });

    assert.equal(store.planAction("alpha", "autoremove").enabled, false);
    assert.equal(store.planBulkAction("update").eligibleNames.length, 0);
    assert.equal(store.getView().actions.inFlightServerNames.includes("alpha"), true);
});

test("bulk plans classify visible eligible, visible ineligible, and hidden selected hosts", () => {
    const store = createStore();
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [
            { name: "alpha", status: "idle", has_key: true },
            { name: "beta", status: "updating", has_password: true },
            { name: "hidden", status: "idle" }
        ]
    });
    ["alpha", "beta", "hidden"].forEach(name => store.dispatch({ type: "selectionChanged", name, selected: true }));
    store.dispatch({ type: "filtersChanged", patch: { search: "a" } });

    const plan = store.planBulkAction("update", { actionLabel: "update" });
    assert.deepEqual(plan.eligibleNames, ["alpha"]);
    assert.deepEqual(plan.ineligible.map(item => item.name), ["beta"]);
    assert.deepEqual(plan.hiddenNames, ["hidden"]);
    assert.equal(plan.skippedHosts.find(item => item.name === "hidden").reason, "Hidden by current filter or page");
});

test("action completion clears in-flight state and emits refresh and announcement effects", () => {
    const store = createStore();
    store.dispatch({ type: "serversSnapshotReceived", servers: [{ name: "alpha", status: "idle" }] });
    const plan = store.planAction("alpha", "update", { actionLabel: "update" });
    store.dispatch({ type: "actionStarted", plan });

    const effects = store.dispatch({
        type: "actionCompleted",
        operationId: plan.id,
        refreshStreams: ["servers", "dashboard"],
        message: "Update started"
    });
    assert.deepEqual(store.getView().actions.inFlightServerNames, []);
    assert.deepEqual(effects.filter(effect => effect.type === "fetchSnapshot").map(effect => effect.stream), ["servers", "dashboard"]);
    assert.equal(effects.some(effect => effect.type === "announceResult" && effect.status === "completed"), true);
});

test("browser adapters do not restore superseded action globals or DOM-derived bulk planning", () => {
    const root = path.resolve(__dirname, "../..");
    const adapterSource = ["static/js/index.js", "static/js/index-bulk-actions.js"]
        .map(file => fs.readFileSync(path.join(root, file), "utf8"))
        .join("\n");
    [
        "singleHostActionsInFlight",
        "bulkActionInFlightLabel",
        "refreshAllFactsInFlight",
        "singleHostActionKey",
        "isSingleHostActionInFlight",
        "row-select:checked"
    ].forEach(legacyName => assert.equal(adapterSource.includes(legacyName), false, `${legacyName} must remain deleted`));
});
