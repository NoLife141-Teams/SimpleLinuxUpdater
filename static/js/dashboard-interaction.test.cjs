const test = require("node:test");
const assert = require("node:assert/strict");

const DashboardProjectionConsumption = require("./dashboard-projection-consumption.js");
const StatusPageInteraction = require("./status-page-interaction.js");
const StatusFormatting = require("./status-formatting.js");
const StatusTransport = require("./status-transport.js");
const StatusTableAdapter = require("./status-table-adapter.js");
const StatusActionAdapter = require("./status-action-adapter.js");
const StatusRendering = require("./status-rendering.js");

test("Status formatting presents shared operator values consistently", () => {
    assert.equal(StatusFormatting.duration(65000), "1m 5s");
    assert.equal(StatusFormatting.diskCapacity(16 * 1024 * 1024, 64 * 1024 * 1024), "16 GiB free of 64 GiB total");
    assert.equal(StatusFormatting.uptime(90061), "1d");
    assert.equal(StatusFormatting.statusLabel("pending_approval"), "pending approval");
});

test("Status transport falls back to polling without dashboard events", () => {
    const intervals = [];
    const timers = {
        setInterval(fn, ms) { intervals.push({ fn, ms }); return intervals.length; },
        clearInterval() {},
        setTimeout() { return 1; },
        clearTimeout() {}
    };
    const controller = StatusTransport.createController({ timers });
    controller.start();
    assert.equal(controller.isLive(), false);
    assert.deepEqual(intervals.slice(-2).map(item => item.ms), [5000, 30000]);
});

test("Status table and action adapters preserve operator preferences and decision facts", () => {
    const values = new Map();
    const table = StatusTableAdapter.create({
        getItem: key => values.get(key) || null,
        setItem: (key, value) => values.set(key, value)
    }, "widths");
    table.save({ name: 160 });
    assert.deepEqual(table.load(), { name: 160 });
    assert.equal(table.boundedWidth(999, 100, 240, 140), 240);
    assert.equal(StatusActionAdapter.approvalImpact("Run security update?", {
        packages: ["openssl"],
        removedPackages: ["legacy-lib"]
    }), "Run security update?\n\nPackages: openssl\n\nMay remove packages: legacy-lib");
});

test("Status rendering owns canonical metric filter labels", () => {
    const rendering = StatusRendering.create({
        getElementById() { return null; },
        querySelectorAll() { return []; }
    });
    assert.equal(rendering.metricFilterLabel("pending_approval"), "Show hosts waiting for approval");
    assert.equal(rendering.metricFilterLabel("active"), "Show hosts with active maintenance phases");
    assert.equal(rendering.metricFilterLabel(""), "Show all hosts");
});

test("Dashboard Projection Consumption owns canonical approval and authentication facts", () => {
    const facts = DashboardProjectionConsumption.presentationFacts;
    const server = {
        name: "edge-01",
        status: "pending_approval",
        has_key: false,
        has_password: true,
        pending_updates: [
            { package: "openssl", security: true },
            { package: "kernel", security: true, kept_back: true }
        ],
        upgrade_plan: {
            standard_package_count: 1,
            kept_back_package_count: 1,
            standard_security_count: 1,
            total_security_count: 2,
            full_upgrade_package_count: 2,
            full_upgrade_plan_available: true,
            kept_back_security_plan_available: true
        }
    };

    assert.deepEqual(facts.approvalCounts(server), {
        total: 2,
        standard: 1,
        keptBack: 1,
        full: 2,
        security: 1,
        totalSecurity: 2,
        keptBackSecurity: 1,
        standardSecurity: 1,
        fullPlanAvailable: true,
        keptBackSecurityPlanAvailable: true,
        newPackages: [],
        removedPackages: [],
        keptBackSecurityNewPackages: [],
        keptBackSecurityRemovedPackages: []
    });
    assert.deepEqual(facts.authFacts(server, true), {
        label: "Global SSH key + password",
        effectiveKey: true,
        usesGlobalKey: true
    });
});

test("Status Page Interaction consumes canonical presentation facts for action plans", () => {
    const store = StatusPageInteraction.createStore({
        presentationFacts: DashboardProjectionConsumption.presentationFacts
    });
    store.dispatch({
        type: "serversSnapshotReceived",
        servers: [{
            name: "edge-01",
            status: "pending_approval",
            has_key: false,
            has_password: true,
            pending_updates: [{ package: "openssl", security: true }],
            upgrade_plan: { standard_package_count: 1, standard_security_count: 1 }
        }]
    });
    store.dispatch({ type: "globalKeyAvailabilityChanged", available: true });
    store.dispatch({ type: "selectionChanged", name: "edge-01", selected: true });

    const plan = store.planAction("edge-01", "approve_security");
    const bulkPlan = store.planBulkAction("approve_security");
    assert.equal(plan.enabled, true);
    assert.equal(bulkPlan.eligibleHosts[0].auth, "Global SSH key + password");
    assert.equal(plan.payloadFacts.counts.standardSecurity, 1);
});

test("Status Page Interaction preserves filters, selection, and drawer state", () => {
    const store = StatusPageInteraction.createStore({
        presentationFacts: DashboardProjectionConsumption.presentationFacts
    });
    store.dispatch({ type: "serversSnapshotReceived", servers: [
        { name: "prod-01", status: "updating", tags: ["prod"] },
        { name: "lab-01", status: "idle", tags: ["lab"] }
    ] });
    store.dispatch({ type: "filtersChanged", patch: { search: "prod" } });
    store.dispatch({ type: "selectionChanged", name: "prod-01", selected: true });
    store.dispatch({ type: "drawerOpened", name: "prod-01", tab: "logs" });

    const view = store.getView();
    assert.deepEqual(view.visibleServers.map(server => server.name), ["prod-01"]);
    assert.deepEqual(view.selectedNames, ["prod-01"]);
    assert.deepEqual(view.drawer, { open: true, serverName: "prod-01", tab: "logs", logFollow: true });
});

test("Status Page Interaction serializes refreshes and rejects stale snapshots", () => {
    const store = StatusPageInteraction.createStore({
        presentationFacts: DashboardProjectionConsumption.presentationFacts
    });
    const firstEffects = store.dispatch({ type: "refreshRequested", streams: ["servers"], priority: "deferable", reason: "poll" });
    assert.deepEqual(firstEffects[0], {
        type: "fetchSnapshot",
        stream: "servers",
        requestId: 1,
        priority: "deferable",
        reason: "poll"
    });
    assert.deepEqual(store.dispatch({ type: "refreshRequested", streams: ["servers"], priority: "immediate", reason: "event" }), []);
    store.dispatch({ type: "serversSnapshotReceived", requestId: 1, servers: [{ name: "accepted", status: "idle" }] });
    const queued = store.getView().sync.streams.servers.inFlight;
    assert.equal(queued.requestId, 2);
    assert.equal(queued.priority, "immediate");
    store.dispatch({ type: "serversSnapshotReceived", requestId: 1, servers: [{ name: "stale", status: "idle" }] });
    assert.equal(store.getServer("stale"), null);
    assert.equal(store.getServer("accepted").name, "accepted");
});

test("Dashboard Projection Consumption preserves canonical actions while deriving legacy presentation fallbacks", () => {
    const presentation = DashboardProjectionConsumption.project({
        globalKeyAvailable: true,
        statusView: {
            servers: [{
                name: "edge-01",
                status: "pending_approval",
                has_password: true,
                pending_updates: [{ package: "openssl", security: true }]
            }],
            visibleServers: [{ name: "edge-01" }],
            pageServers: [{ name: "edge-01" }],
            groups: [{ key: "", items: [{ name: "edge-01" }] }],
            dashboardServers: [{ name: "edge-01", actions: { approve_security: { enabled: false, reason: "Policy blocked" } } }],
            actionViews: { "edge-01": { approve_security: { enabled: false, reason: "Policy blocked" } } },
            actions: { inFlightServerNames: [] },
            primaryServerName: "edge-01"
        }
    });
    const server = presentation.serversByName["edge-01"];
    assert.equal(server.auth.label, "Global SSH key + password");
    assert.equal(server.approvalCounts.standard, 1);
    assert.equal(server.actions.approve_security.enabled, false);
});
